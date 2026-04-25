package payments

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

// IntakeSource is the narrow DB interface the intake report needs.
// Satisfied by *store.DB.FetchOrdersByDateRange.
type IntakeSource interface {
	FetchOrdersByDateRange(ctx context.Context, start, end time.Time) ([]model.Order, error)
}

// IntakeReporter sends a morning summary of yesterday's order intake to
// ops/finance. Pulls all orders created in the previous UTC day from the
// local Postgres (never touches SYSPRO or Shopify — the report reflects
// *our* view of the pipeline, which is exactly what ops needs to spot
// stuck/failed rows).
type IntakeReporter struct {
	source         IntakeSource
	mailer         EmailSender
	recipients     []string
	storeName      string
	hour           int
	now            func() time.Time
	logger         *slog.Logger
	healthcheckURL string
	deadLetterDir  string
	archiveDir     string
	stateDir       string
	ntfyTopic      string

	mu sync.Mutex
}

// IntakeReporterConfig bundles inputs for NewIntakeReporter.
type IntakeReporterConfig struct {
	Source     IntakeSource
	Mailer     EmailSender
	Recipients []string
	StoreName  string
	Hour       int // UTC, typically 6 (= 07:00 BST)
	Logger     *slog.Logger

	// Resilience layer — see DailyReporterConfig for full docs.
	HealthcheckURL string
	DeadLetterDir  string
	ArchiveDir     string
	StateDir       string
	NtfyTopic      string
}

// NewIntakeReporter validates inputs. Callers should pre-check Mailer /
// Recipients presence at boot to get a nicer log line.
func NewIntakeReporter(cfg IntakeReporterConfig) (*IntakeReporter, error) {
	if cfg.Source == nil {
		return nil, fmt.Errorf("intake reporter: source required")
	}
	if cfg.Mailer == nil {
		return nil, fmt.Errorf("intake reporter: mailer required")
	}
	if len(cfg.Recipients) == 0 {
		return nil, fmt.Errorf("intake reporter: no recipients")
	}
	if cfg.Hour < 0 || cfg.Hour > 23 {
		return nil, fmt.Errorf("intake reporter: hour out of range")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &IntakeReporter{
		source:         cfg.Source,
		mailer:         cfg.Mailer,
		recipients:     cfg.Recipients,
		storeName:      cfg.StoreName,
		hour:           cfg.Hour,
		now:            time.Now,
		logger:         cfg.Logger,
		healthcheckURL: cfg.HealthcheckURL,
		deadLetterDir:  cfg.DeadLetterDir,
		archiveDir:     cfg.ArchiveDir,
		stateDir:       cfg.StateDir,
		ntfyTopic:      cfg.NtfyTopic,
	}, nil
}

// Run blocks until ctx is cancelled. Check cadence mirrors DailyReporter
// — 15-minute ticks, fires once per UTC day when the hour matches.
func (r *IntakeReporter) Run(ctx context.Context) {
	r.logger.Info("intake report scheduler started",
		"hour_utc", r.hour,
		"recipients", len(r.recipients),
	)
	lastSent := readLastSent(r.stateDir, "intake")
	check := func() {
		now := r.now().UTC()
		if now.Hour() != r.hour {
			return
		}
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		if !lastSent.Before(today) {
			return
		}
		yesterday := today.AddDate(0, 0, -1)
		if err := r.SendForDate(ctx, yesterday); err != nil {
			r.logger.Error("intake report send failed",
				"error", err, "date", yesterday.Format("2006-01-02"))
			return
		}
		lastSent = today
		_ = writeLastSent(r.stateDir, "intake", today)
	}

	check()
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("intake report scheduler stopping")
			return
		case <-ticker.C:
			check()
		}
	}
}

// SendForDate pulls orders created on `date` (00:00 UTC inclusive to
// next 00:00 UTC exclusive), builds the HTML email body + CSV
// attachment, and sends. Public so operators can re-run a missed day.
func (r *IntakeReporter) SendForDate(ctx context.Context, date time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)

	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	orders, err := r.source.FetchOrdersByDateRange(fetchCtx, start, end)
	if err != nil {
		return fmt.Errorf("fetching orders: %w", err)
	}

	summary := summariseIntake(orders)
	storeTag := r.storeName
	if storeTag == "" {
		storeTag = "Shopify"
	}
	subject := fmt.Sprintf("[%s] Order intake — %s (%d orders, £%.2f)",
		storeTag, start.Format("2006-01-02"), summary.Count, summary.GrossTotal)

	anomalies := ValidateIntakeAnomalies(orders, start)
	if len(anomalies) > 0 {
		subject = "[⚠ ANOMALY] " + subject
		r.logger.Error("intake anomalies detected",
			"date", start.Format("2006-01-02"), "anomalies", anomalies)
	}

	html := buildIntakeHTML(start, storeTag, summary, anomalies)
	csvBody, err := buildIntakeCSV(orders)
	if err != nil {
		return fmt.Errorf("building csv: %w", err)
	}
	att := &Attachment{
		Filename:    fmt.Sprintf("order-intake-%s.csv", start.Format("2006-01-02")),
		ContentType: "text/csv; charset=utf-8",
		Body:        csvBody,
	}

	sendCtx, sendCancel := context.WithTimeout(ctx, 60*time.Second)
	defer sendCancel()
	if err := r.mailer.Send(sendCtx, r.recipients, subject, html, att); err != nil {
		dlPath, dlErr := writeDeadLetter(r.deadLetterDir, "intake", start, csvBody)
		r.logger.Error("intake report send failed — CSV preserved",
			"date", start.Format("2006-01-02"),
			"send_error", err, "dead_letter_path", dlPath, "dead_letter_error", dlErr)
		pingNtfyDeadLetter(ctx, r.ntfyTopic, "intake", start, dlPath, err)
		return fmt.Errorf("sending email: %w", err)
	}
	if archPath, archErr := archiveSentCSV(r.archiveDir, "intake", start, csvBody); archErr != nil {
		r.logger.Warn("sent-CSV archive failed (best-effort)", "error", archErr)
	} else if archPath != "" {
		r.logger.Debug("sent CSV archived", "path", archPath)
	}
	pingHealthcheck(ctx, r.healthcheckURL)
	r.logger.Info("intake report sent",
		"date", start.Format("2006-01-02"),
		"count", summary.Count,
		"gross", summary.GrossTotal,
		"stuck", summary.Stuck,
	)
	return nil
}

// IntakeSummary is the headline figures ops wants to see at a glance.
// Stuck = anything not cleanly moved to SYSPRO + fulfilment (failed,
// dead_letter, or submitted-but-no-syspro-number — the BBQ1026
// fingerprint).
type IntakeSummary struct {
	Count      int
	GrossTotal float64
	Pending    int
	Submitted  int
	Fulfilled  int
	Failed     int
	DeadLetter int
	Cancelled  int
	Stuck      int // submitted-but-no-syspro-number (invisible-to-fulfilment)
}

func summariseIntake(orders []model.Order) IntakeSummary {
	var s IntakeSummary
	s.Count = len(orders)
	for _, o := range orders {
		s.GrossTotal += o.PaymentAmount
		switch o.Status {
		case model.OrderStatusPending, model.OrderStatusProcessing:
			s.Pending++
		case model.OrderStatusSubmitted:
			s.Submitted++
			if o.SysproOrderNumber == "" {
				s.Stuck++
			}
		case model.OrderStatusFulfilled:
			s.Fulfilled++
		case model.OrderStatusFailed:
			s.Failed++
		case model.OrderStatusDeadLetter:
			s.DeadLetter++
		case model.OrderStatusCancelled:
			s.Cancelled++
		}
	}
	return s
}

// ValidateIntakeAnomalies returns human-readable warnings when the
// intake report data looks suspicious. Empty slice = OK. Conservative
// rules to avoid false positives:
//
//   1. Zero orders on a Mon–Fri (UTC) → orders.json query is likely
//      broken. Saturday/Sunday zero-days are normal at Rectella.
//   2. Non-empty orders + every payment_amount == 0 → payment field
//      lost/renamed. A real zero-payment day doesn't happen with
//      Shopify Payments live.
//   3. Stuck-rows fingerprint (BBQ1026 class): >0 orders and ALL of
//      them in `submitted` with empty syspro_order_number. Means the
//      batch processor's writeback is silently broken.
func ValidateIntakeAnomalies(orders []model.Order, day time.Time) []string {
	var out []string
	weekday := day.UTC().Weekday()
	isBusinessDay := weekday >= time.Monday && weekday <= time.Friday

	if len(orders) == 0 {
		if isBusinessDay {
			out = append(out, fmt.Sprintf("Zero orders on a business day (%s) — likely the orders listing query is broken or Shopify webhooks are not arriving.", weekday))
		}
		return out
	}

	allZeroPayment := true
	stuckCount := 0
	for _, o := range orders {
		if o.PaymentAmount > 0 {
			allZeroPayment = false
		}
		if o.Status == model.OrderStatusSubmitted && o.SysproOrderNumber == "" {
			stuckCount++
		}
	}
	if allZeroPayment {
		out = append(out, fmt.Sprintf("%d orders but every payment_amount is zero — likely the payment field is being lost.", len(orders)))
	}
	if stuckCount == len(orders) && stuckCount > 0 {
		out = append(out, fmt.Sprintf("All %d orders are in 'submitted' state with no SYSPRO sales-order number — BBQ1026 fingerprint, batch processor writeback may be broken.", stuckCount))
	}
	return out
}

func buildIntakeHTML(date time.Time, storeTag string, s IntakeSummary, anomalies []string) string {
	var b strings.Builder
	if len(anomalies) > 0 {
		fmt.Fprintf(&b, "<div style=\"background:#fee;border:2px solid #b33;padding:12px;margin-bottom:16px\">\n")
		fmt.Fprintf(&b, "<p style=\"margin:0 0 8px 0\"><strong style=\"color:#b33\">⚠ DATA ANOMALY DETECTED</strong></p>\n")
		fmt.Fprintf(&b, "<ul style=\"margin:0 0 0 16px;padding:0\">\n")
		for _, a := range anomalies {
			fmt.Fprintf(&b, "<li>%s</li>\n", html.EscapeString(a))
		}
		fmt.Fprintf(&b, "</ul>\n")
		fmt.Fprintf(&b, "<p style=\"margin:8px 0 0 0\">Cross-check the figures below against Shopify admin before acting on them.</p>\n")
		fmt.Fprintf(&b, "</div>\n")
	}
	fmt.Fprintf(&b, "<p>Order intake for <strong>%s</strong> on <strong>%s</strong>.</p>\n",
		html.EscapeString(storeTag), date.Format("2006-01-02"))
	fmt.Fprintf(&b, "<ul>\n")
	fmt.Fprintf(&b, "<li>Orders received: <strong>%d</strong></li>\n", s.Count)
	fmt.Fprintf(&b, "<li>Gross total: <strong>£%.2f</strong></li>\n", s.GrossTotal)
	fmt.Fprintf(&b, "</ul>\n")
	fmt.Fprintf(&b, "<p><strong>Pipeline status breakdown:</strong></p>\n<ul>\n")
	fmt.Fprintf(&b, "<li>Pending in queue: %d</li>\n", s.Pending)
	fmt.Fprintf(&b, "<li>Submitted to SYSPRO: %d</li>\n", s.Submitted)
	fmt.Fprintf(&b, "<li>Fulfilled: %d</li>\n", s.Fulfilled)
	fmt.Fprintf(&b, "<li>Failed: %d</li>\n", s.Failed)
	fmt.Fprintf(&b, "<li>Dead-lettered: %d</li>\n", s.DeadLetter)
	fmt.Fprintf(&b, "<li>Cancelled: %d</li>\n", s.Cancelled)
	if s.Stuck > 0 {
		fmt.Fprintf(&b, "<li><strong style=\"color:#b33\">Stuck (submitted but no SYSPRO number): %d — needs manual review</strong></li>\n", s.Stuck)
	}
	fmt.Fprintf(&b, "</ul>\n")
	fmt.Fprintf(&b, "<p>Full per-order breakdown attached as CSV.</p>\n")
	return b.String()
}

func buildIntakeCSV(orders []model.Order) ([]byte, error) {
	var buf bytes.Buffer
	// UTF-8 BOM so Excel renders £ glyphs correctly (avoids "Â£" mojibake).
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{
		"Order Number", "Shopify Order ID", "Status",
		"SYSPRO SO", "Payment Ref", "Total", "Attempts",
		"Last Error", "Customer", "Created (UTC)",
	}); err != nil {
		return nil, fmt.Errorf("writing csv header: %w", err)
	}
	for _, o := range orders {
		customer := strings.TrimSpace(o.ShipFirstName + " " + o.ShipLastName)
		if customer == "" {
			customer = o.ShipEmail
		}
		row := []string{
			o.OrderNumber,
			fmt.Sprintf("%d", o.ShopifyOrderID),
			string(o.Status),
			o.SysproOrderNumber,
			o.PaymentReference,
			fmt.Sprintf("£%.2f", o.PaymentAmount),
			fmt.Sprintf("%d", o.Attempts),
			o.LastError,
			customer,
			o.CreatedAt.UTC().Format(time.RFC3339),
		}
		if err := w.Write(row); err != nil {
			return nil, fmt.Errorf("writing csv row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("csv writer: %w", err)
	}
	return buf.Bytes(), nil
}
