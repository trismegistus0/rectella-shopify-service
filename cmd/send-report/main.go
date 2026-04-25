// Command send-report force-sends a daily email report for a given date.
// Operator one-shot: same code path as the scheduled reporters in
// cmd/server, just invoked directly so we don't have to wait for the
// 01:00 / 06:00 UTC ticks to validate the wiring.
//
// Usage:
//
//	go run ./cmd/send-report --type=cash   --date=2026-04-24
//	go run ./cmd/send-report --type=intake --date=2026-04-24
//
// --date defaults to yesterday (UTC). Both reports require the
// GRAPH_* env vars; cash also needs SHOPIFY_ACCESS_TOKEN +
// CREDIT_CONTROL_TO; intake needs ORDER_INTAKE_TO. Source the same
// dotenv the service uses before running.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/trismegistus0/rectella-shopify-service/config"
	"github.com/trismegistus0/rectella-shopify-service/internal/payments"
	"github.com/trismegistus0/rectella-shopify-service/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "send-report: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	reportType := flag.String("type", "", "cash | intake (required)")
	dateStr := flag.String("date", "", "YYYY-MM-DD; default = yesterday UTC. Mutually exclusive with --from/--to.")
	fromStr := flag.String("from", "", "YYYY-MM-DD; backfill range start (inclusive). Pairs with --to.")
	toStr := flag.String("to", "", "YYYY-MM-DD; backfill range end (inclusive). Pairs with --from.")
	coverNote := flag.String("cover-note", "", "Optional intro paragraph prepended to the email body. Useful for one-off operator sends like the credit-control cutover announcement. May contain newlines.")
	coverNoteFile := flag.String("cover-note-file", "", "Path to a text file whose contents become the cover note. Lets you keep multi-paragraph copy out of the shell command.")
	flag.Parse()

	if *reportType != "cash" && *reportType != "intake" {
		return errors.New("--type must be 'cash' or 'intake'")
	}
	if (*fromStr != "") != (*toStr != "") {
		return errors.New("--from and --to must be set together")
	}
	rangeMode := *fromStr != ""
	if rangeMode && *dateStr != "" {
		return errors.New("--date is mutually exclusive with --from/--to")
	}
	if rangeMode && *reportType != "cash" {
		return errors.New("--from/--to range mode only supports --type=cash today")
	}

	var date, rangeStart, rangeEnd time.Time
	if rangeMode {
		s, err := time.Parse("2006-01-02", *fromStr)
		if err != nil {
			return fmt.Errorf("invalid --from %q: %w", *fromStr, err)
		}
		e, err := time.Parse("2006-01-02", *toStr)
		if err != nil {
			return fmt.Errorf("invalid --to %q: %w", *toStr, err)
		}
		// Range is half-open [start, end+1day) so the last day is included.
		rangeStart = s
		rangeEnd = e.AddDate(0, 0, 1)
		if !rangeStart.Before(rangeEnd) {
			return fmt.Errorf("--from must be on or before --to")
		}
	} else if *dateStr == "" {
		date = time.Now().UTC().AddDate(0, 0, -1)
	} else {
		d, err := time.Parse("2006-01-02", *dateStr)
		if err != nil {
			return fmt.Errorf("invalid --date %q: %w", *dateStr, err)
		}
		date = d
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if cfg.GraphTenantID == "" || cfg.GraphClientID == "" ||
		cfg.GraphClientSecret == "" || cfg.GraphSenderMailbox == "" {
		return errors.New("GRAPH_* env vars incomplete (need TENANT_ID, CLIENT_ID, CLIENT_SECRET, SENDER_MAILBOX)")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mailer := payments.NewMailer(payments.MailerConfig{
		TenantID:      cfg.GraphTenantID,
		ClientID:      cfg.GraphClientID,
		ClientSecret:  cfg.GraphClientSecret,
		SenderMailbox: cfg.GraphSenderMailbox,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch *reportType {
	case "cash":
		if len(cfg.CreditControlTo) == 0 {
			return errors.New("CREDIT_CONTROL_TO not set")
		}
		if cfg.ShopifyAccessToken == "" {
			return errors.New("SHOPIFY_ACCESS_TOKEN not set (cash report needs Shopify Admin API access)")
		}
		fetcher := payments.NewTransactionsFetcher(cfg.ShopifyStoreURL, cfg.ShopifyAccessToken, logger)
		reporter, err := payments.NewDailyReporter(payments.DailyReporterConfig{
			Source:        fetcher,
			Mailer:        mailer,
			Recipients:    cfg.CreditControlTo,
			StoreName:     cfg.ShopifyStoreURL,
			Hour:          cfg.DailyReportHour,
			Logger:        logger,
			DeadLetterDir: cfg.DeadLetterDir,
			ArchiveDir:    cfg.SentReportArchiveDir,
			NtfyTopic:     cfg.NtfyTopic,
			// Healthcheck + StateDir intentionally omitted — operator
			// runs are out-of-band, shouldn't trigger the daily heartbeat
			// or update the persistent last-sent state.
		})
		if err != nil {
			return fmt.Errorf("cash reporter init: %w", err)
		}
		// Resolve cover note (file beats inline if both provided).
		note := *coverNote
		if *coverNoteFile != "" {
			b, err := os.ReadFile(*coverNoteFile)
			if err != nil {
				return fmt.Errorf("reading --cover-note-file: %w", err)
			}
			note = string(b)
		}
		if note != "" {
			reporter.SetCoverNote(note)
		}
		if rangeMode {
			if err := reporter.SendForRange(ctx, rangeStart, rangeEnd); err != nil {
				return fmt.Errorf("cash range report send: %w", err)
			}
			fmt.Printf("OK — cash-receipt range report sent for %s to %s to %v\n",
				rangeStart.Format("2006-01-02"),
				rangeEnd.AddDate(0, 0, -1).Format("2006-01-02"),
				cfg.CreditControlTo)
			return nil
		}
		if err := reporter.SendForDate(ctx, date); err != nil {
			return fmt.Errorf("cash report send: %w", err)
		}
		fmt.Printf("OK — cash-receipt report sent for %s to %v\n",
			date.Format("2006-01-02"), cfg.CreditControlTo)
		return nil

	case "intake":
		if len(cfg.OrderIntakeTo) == 0 {
			return errors.New("ORDER_INTAKE_TO not set")
		}
		pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("postgres connect: %w", err)
		}
		defer pool.Close()
		db := &store.DB{Pool: pool}
		intake, err := payments.NewIntakeReporter(payments.IntakeReporterConfig{
			Source:        db,
			Mailer:        mailer,
			Recipients:    cfg.OrderIntakeTo,
			StoreName:     cfg.ShopifyStoreURL,
			Hour:          cfg.OrderIntakeHour,
			Logger:        logger,
			DeadLetterDir: cfg.DeadLetterDir,
			ArchiveDir:    cfg.SentReportArchiveDir,
			NtfyTopic:     cfg.NtfyTopic,
		})
		if err != nil {
			return fmt.Errorf("intake reporter init: %w", err)
		}
		if err := intake.SendForDate(ctx, date); err != nil {
			return fmt.Errorf("intake report send: %w", err)
		}
		fmt.Printf("OK — order-intake report sent for %s to %v\n",
			date.Format("2006-01-02"), cfg.OrderIntakeTo)
		return nil
	}

	return nil // unreachable
}
