package payments

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// TransactionSource is the narrow interface the daily report needs from
// the Shopify transactions fetcher. Satisfied by *TransactionsFetcher.
type TransactionSource interface {
	FetchOrdersInWindow(ctx context.Context, since, until time.Time) ([]ShopifyTransaction, error)
}

// EmailSender is the narrow interface the daily report needs from the
// mailer. Satisfied by *Mailer.
type EmailSender interface {
	Send(ctx context.Context, to []string, subject, textBody string, att *Attachment) error
}

// DailyReporter sends one CSV per day to credit control. It wakes at
// `hour` UTC each day, pulls yesterday's settled transactions from
// Shopify, builds the CSV, and emails it with a plaintext summary. If
// yesterday had zero transactions the email is still sent so credit
// control knows the job is alive.
type DailyReporter struct {
	source       TransactionSource
	mailer       EmailSender
	recipients   []string
	storeName    string
	hour         int
	now          func() time.Time
	logger       *slog.Logger
	healthcheckURL string
	deadLetterDir string
	archiveDir    string
	stateDir      string
	ntfyTopic     string

	mu sync.Mutex
}

// DailyReporterConfig bundles inputs for NewDailyReporter.
type DailyReporterConfig struct {
	Source     TransactionSource
	Mailer     EmailSender
	Recipients []string
	StoreName  string // used in the email subject + filename
	Hour       int    // UTC hour, 0-23
	Logger     *slog.Logger

	// Resilience layer — all optional, features degrade gracefully.
	HealthcheckURL string // GET ping after each successful send (Healthchecks.io etc.)
	DeadLetterDir  string // where to drop the CSV if Send fails
	ArchiveDir     string // where to archive every successful CSV (audit trail)
	StateDir       string // where to persist last-send date (idempotency on restart)
	NtfyTopic      string // ntfy push topic for dead-letter alerts
}

// NewDailyReporter validates inputs and returns a reporter. Returns an
// error if required fields are missing — callers that want graceful
// disablement should check config presence before calling.
func NewDailyReporter(cfg DailyReporterConfig) (*DailyReporter, error) {
	if cfg.Source == nil {
		return nil, fmt.Errorf("daily reporter: source required")
	}
	if cfg.Mailer == nil {
		return nil, fmt.Errorf("daily reporter: mailer required")
	}
	if len(cfg.Recipients) == 0 {
		return nil, fmt.Errorf("daily reporter: no recipients")
	}
	if cfg.Hour < 0 || cfg.Hour > 23 {
		return nil, fmt.Errorf("daily reporter: hour out of range")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &DailyReporter{
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

// Run blocks until ctx is cancelled. Wakes once per hour, checks the
// current UTC hour against the configured send hour, and if the send
// hour matches and today's report has not yet been sent it runs
// SendForDate. The check cadence is deliberately coarse — the daily
// report is a once-per-day job, and a ~1h granularity avoids drift
// and makes the scheduler trivially testable.
func (r *DailyReporter) Run(ctx context.Context) {
	r.logger.Info("daily report scheduler started",
		"hour_utc", r.hour,
		"recipients", len(r.recipients),
	)
	// Idempotency: persist last-fired date to disk so an operator restart
	// between r.hour and r.hour+1 doesn't re-fire today's send. Falls
	// back to in-memory zero-value if the state dir isn't configured (in
	// which case restart-during-the-fire-window can double-send — known
	// limitation, documented in the handover §8).
	lastSent := readLastSent(r.stateDir, "cash")
	check := func() {
		now := r.now().UTC()
		if now.Hour() != r.hour {
			return
		}
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		if !lastSent.Before(today) {
			return // already sent today
		}
		yesterday := today.AddDate(0, 0, -1)
		if err := r.SendForDate(ctx, yesterday); err != nil {
			r.logger.Error("daily report send failed", "error", err, "date", yesterday.Format("2006-01-02"))
			return
		}
		lastSent = today
		_ = writeLastSent(r.stateDir, "cash", today)
	}

	check()
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("daily report scheduler stopping")
			return
		case <-ticker.C:
			check()
		}
	}
}

// SendForRange pulls all transactions in [start, end) UTC and emails one
// bulk CSV via BuildRangeCSV. Used by the operator backfill flow to
// send a multi-day validation email. Recipient list and store name come
// from the same DailyReporterConfig as SendForDate.
func (r *DailyReporter) SendForRange(ctx context.Context, start, end time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !start.Before(end) {
		return fmt.Errorf("invalid range: start %s not before end %s",
			start.Format("2006-01-02"), end.Format("2006-01-02"))
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	txns, err := r.source.FetchOrdersInWindow(fetchCtx, start, end)
	if err != nil {
		return fmt.Errorf("fetching transactions: %w", err)
	}

	csv, err := BuildRangeCSV(start, end, txns)
	if err != nil {
		return fmt.Errorf("building csv: %w", err)
	}

	gross, fee, net, count := SummariseTotals(txns)
	storeTag := r.storeName
	if storeTag == "" {
		storeTag = "Shopify"
	}
	endLabel := end.AddDate(0, 0, -1).Format("2006-01-02") // inclusive end-date
	subject := fmt.Sprintf("[%s] Cash receipts — %s to %s (%d transactions)",
		storeTag, start.Format("2006-01-02"), endLabel, count)
	body := fmt.Sprintf(
		"Cash-receipt backfill report.\n\n"+
			"Window:        %s to %s (UTC)\n"+
			"Transactions:  %d\n"+
			"Gross:         £%.2f\n"+
			"Fees:          £%.2f\n"+
			"Net:           £%.2f\n\n"+
			"Per-day breakdown attached.\n",
		start.Format("2006-01-02"), endLabel, count, gross, fee, net,
	)
	if anomaly := ValidateCashReceiptCSV(txns); anomaly != "" {
		subject = "[⚠ ANOMALY] " + subject
		body = "*** DATA ANOMALY DETECTED ***\n" + anomaly + "\n*** Treat the figures below with caution and cross-check against Shopify admin. ***\n\n" + body
		r.logger.Error("range cash-receipt anomaly detected",
			"start", start.Format("2006-01-02"), "end", endLabel, "anomaly", anomaly)
	}
	att := &Attachment{
		Filename:    fmt.Sprintf("cash-receipts-%s-to-%s.csv", start.Format("2006-01-02"), endLabel),
		ContentType: "text/csv; charset=utf-8",
		Body:        csv,
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, 60*time.Second)
	defer sendCancel()
	if err := r.mailer.Send(sendCtx, r.recipients, subject, body, att); err != nil {
		dlPath, dlErr := writeDeadLetter(r.deadLetterDir, "cash-range", start, csv)
		r.logger.Error("range report send failed — CSV preserved",
			"start", start.Format("2006-01-02"), "end", endLabel,
			"send_error", err, "dead_letter_path", dlPath, "dead_letter_error", dlErr)
		pingNtfyDeadLetter(ctx, r.ntfyTopic, "cash-range", start, dlPath, err)
		return fmt.Errorf("sending email: %w", err)
	}
	if archPath, archErr := archiveSentCSVRange(r.archiveDir, "cash", start, end, csv); archErr != nil {
		r.logger.Warn("sent-CSV archive failed (best-effort)", "error", archErr)
	} else if archPath != "" {
		r.logger.Debug("sent CSV archived", "path", archPath)
	}
	pingHealthcheck(ctx, r.healthcheckURL)
	r.logger.Info("range report sent",
		"start", start.Format("2006-01-02"),
		"end", endLabel,
		"count", count,
		"gross", gross,
		"fee", fee,
		"net", net,
	)
	return nil
}

// SendForDate pulls the transactions that settled on the given date
// (00:00 UTC inclusive to next 00:00 UTC exclusive), builds the CSV,
// and emails it. Public so operators can re-send a missed day via a
// one-shot command if the scheduler drops.
func (r *DailyReporter) SendForDate(ctx context.Context, date time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)

	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	txns, err := r.source.FetchOrdersInWindow(fetchCtx, start, end)
	if err != nil {
		return fmt.Errorf("fetching transactions: %w", err)
	}

	csv, err := BuildCSV(start, txns)
	if err != nil {
		return fmt.Errorf("building csv: %w", err)
	}

	gross, fee, net, count := SummariseTotals(txns)
	storeTag := r.storeName
	if storeTag == "" {
		storeTag = "Shopify"
	}
	subject := fmt.Sprintf("[%s] Daily cash receipts — %s (%d)", storeTag, start.Format("2006-01-02"), count)
	body := fmt.Sprintf(
		"Daily cash-receipt report for %s.\n\n"+
			"Transactions: %d\n"+
			"Gross:        %.2f\n"+
			"Fees:         %.2f\n"+
			"Net:          %.2f\n\n"+
			"Full breakdown attached.\n",
		start.Format("2006-01-02"), count, gross, fee, net,
	)
	if anomaly := ValidateCashReceiptCSV(txns); anomaly != "" {
		subject = "[⚠ ANOMALY] " + subject
		body = "*** DATA ANOMALY DETECTED ***\n" + anomaly + "\n*** Treat the figures below with caution and cross-check against Shopify admin. ***\n\n" + body
		r.logger.Error("cash-receipt anomaly detected",
			"date", start.Format("2006-01-02"), "anomaly", anomaly)
	}
	att := &Attachment{
		Filename:    fmt.Sprintf("cash-receipts-%s.csv", start.Format("2006-01-02")),
		ContentType: "text/csv; charset=utf-8",
		Body:        csv,
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, 60*time.Second)
	defer sendCancel()
	if err := r.mailer.Send(sendCtx, r.recipients, subject, body, att); err != nil {
		dlPath, dlErr := writeDeadLetter(r.deadLetterDir, "cash", start, csv)
		r.logger.Error("daily report send failed — CSV preserved",
			"date", start.Format("2006-01-02"),
			"send_error", err, "dead_letter_path", dlPath, "dead_letter_error", dlErr)
		pingNtfyDeadLetter(ctx, r.ntfyTopic, "cash", start, dlPath, err)
		return fmt.Errorf("sending email: %w", err)
	}
	if archPath, archErr := archiveSentCSV(r.archiveDir, "cash", start, csv); archErr != nil {
		r.logger.Warn("sent-CSV archive failed (best-effort)", "error", archErr)
	} else if archPath != "" {
		r.logger.Debug("sent CSV archived", "path", archPath)
	}
	pingHealthcheck(ctx, r.healthcheckURL)
	r.logger.Info("daily report sent",
		"date", start.Format("2006-01-02"),
		"count", count,
		"gross", gross,
		"net", net,
	)
	return nil
}
