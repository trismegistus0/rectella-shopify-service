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
	dateStr := flag.String("date", "", "YYYY-MM-DD; default = yesterday UTC")
	flag.Parse()

	if *reportType != "cash" && *reportType != "intake" {
		return errors.New("--type must be 'cash' or 'intake'")
	}

	var date time.Time
	if *dateStr == "" {
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
			Source:     fetcher,
			Mailer:     mailer,
			Recipients: cfg.CreditControlTo,
			StoreName:  cfg.ShopifyStoreURL,
			Hour:       cfg.DailyReportHour,
			Logger:     logger,
		})
		if err != nil {
			return fmt.Errorf("cash reporter init: %w", err)
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
			Source:     db,
			Mailer:     mailer,
			Recipients: cfg.OrderIntakeTo,
			StoreName:  cfg.ShopifyStoreURL,
			Hour:       cfg.OrderIntakeHour,
			Logger:     logger,
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
