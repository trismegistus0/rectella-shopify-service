package payments

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/store"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

// PaymentStore is the subset of *store.DB the syncer needs. Narrow
// interface keeps the unit tests trivial.
type PaymentStore interface {
	FetchUnpostedPayments(ctx context.Context, limit int) ([]store.PaymentPosting, error)
	MarkPaymentPosted(ctx context.Context, id int64, sysproRef string) error
	MarkPaymentFailed(ctx context.Context, id int64, errMsg string) error
}

// CashReceiptPoster is satisfied by *syspro.EnetClient. Also lets tests
// inject a fake poster.
type CashReceiptPoster interface {
	PostCashReceipt(ctx context.Context, r syspro.CashReceipt) (string, error)
}

// Syncer polls payment_postings for rows in `pending` status and calls
// SYSPRO ARSPAY to turn them into cash receipts. Mirrors the structure
// of internal/inventory/syncer.go: interval ticker, single-flight
// guard, 3-minute per-cycle timeout.
//
// Until the ARSPAY XML builder lands the poster will return
// syspro.ErrCashReceiptNotImplemented. The syncer treats that sentinel
// as "not yet, leave it alone" — the row stays `pending` and the
// attempt counter does not increment. Once the builder is implemented
// the feature flag flips and the same rows will drain on the next tick.
type Syncer struct {
	store     PaymentStore
	poster    CashReceiptPoster
	interval  time.Duration
	batchSize int
	customer  string
	logger    *slog.Logger

	syncMu sync.Mutex
}

// NewSyncer builds a payments syncer. `customer` is the SYSPRO customer
// code (always "WEBS01" for Phase 1).
func NewSyncer(store PaymentStore, poster CashReceiptPoster, interval time.Duration, customer string, logger *slog.Logger) *Syncer {
	return &Syncer{
		store:     store,
		poster:    poster,
		interval:  interval,
		batchSize: 50,
		customer:  customer,
		logger:    logger,
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	s.logger.Info("payments sync started", "interval", s.interval, "customer", s.customer)
	s.tick(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("payments sync stopping")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Syncer) tick(ctx context.Context) {
	if !s.syncMu.TryLock() {
		s.logger.Debug("payments sync already running, skipping tick")
		return
	}
	defer s.syncMu.Unlock()
	cycleCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s.cycle(cycleCtx)
}

func (s *Syncer) cycle(ctx context.Context) {
	rows, err := s.store.FetchUnpostedPayments(ctx, s.batchSize)
	if err != nil {
		s.logger.Error("fetching unposted payments", "error", err)
		return
	}
	if len(rows) == 0 {
		s.logger.Debug("no pending payments")
		return
	}
	s.logger.Info("payments cycle starting", "pending", len(rows))

	var posted, skipped, failed int
	for _, p := range rows {
		if err := ctx.Err(); err != nil {
			s.logger.Info("payments cycle aborted", "error", err)
			return
		}

		receipt := syspro.CashReceipt{
			CustomerCode:  s.customer,
			InvoiceNumber: p.OrderNumber,
			Amount:        p.GrossAmount,
			BankCharges:   p.FeeAmount,
			Currency:      p.Currency,
			PaymentMethod: p.PaymentGateway,
			PostedAt:      p.ProcessedAt,
		}
		ref, err := s.poster.PostCashReceipt(ctx, receipt)
		switch {
		case err == nil:
			if mErr := s.store.MarkPaymentPosted(ctx, p.ID, ref); mErr != nil {
				s.logger.Error("marking payment posted", "id", p.ID, "error", mErr)
				failed++
				continue
			}
			posted++
			s.logger.Info("payment posted",
				"id", p.ID,
				"order_number", p.OrderNumber,
				"gross", p.GrossAmount,
				"syspro_ref", ref,
			)
		case errors.Is(err, syspro.ErrCashReceiptNotImplemented):
			// Feature flag is off. Leave the row pending, don't count
			// it as a failure. Emit once per cycle (not per row) so
			// logs don't flood while scaffolded.
			skipped++
		default:
			if mErr := s.store.MarkPaymentFailed(ctx, p.ID, err.Error()); mErr != nil {
				s.logger.Error("marking payment failed", "id", p.ID, "error", mErr)
			}
			failed++
			s.logger.Warn("payment post failed",
				"id", p.ID,
				"order_number", p.OrderNumber,
				"error", err,
			)
		}
	}
	if skipped > 0 {
		s.logger.Info("payments sync skipped (ARSPAY not implemented)", "skipped", skipped)
	}
	s.logger.Info("payments cycle complete",
		"posted", posted,
		"skipped", skipped,
		"failed", failed,
	)
}
