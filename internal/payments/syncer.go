package payments

import (
	"context"
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
// SYSPRO ARSTPY to turn them into cash receipts. Mirrors the structure
// of internal/inventory/syncer.go: interval ticker, single-flight
// guard, 3-minute per-cycle timeout.
type Syncer struct {
	store       PaymentStore
	poster      CashReceiptPoster
	interval    time.Duration
	batchSize   int
	customer    string
	bank        string
	paymentType string
	logger      *slog.Logger

	syncMu sync.Mutex
}

// SyncerConfig bundles required + optional inputs for NewSyncer. Bank
// and PaymentType are SYSPRO installation values (cashbook code +
// payment-method code) that must be set on every ARSTPY post.
type SyncerConfig struct {
	Store       PaymentStore
	Poster      CashReceiptPoster
	Interval    time.Duration
	Customer    string // SYSPRO customer (always "WEBS01" for Phase 1)
	Bank        string // ARSPAY_CASH_BOOK
	PaymentType string // ARSPAY_PAYMENT_TYPE
	Logger      *slog.Logger
}

// NewSyncer builds a payments syncer.
func NewSyncer(cfg SyncerConfig) *Syncer {
	return &Syncer{
		store:       cfg.Store,
		poster:      cfg.Poster,
		interval:    cfg.Interval,
		batchSize:   50,
		customer:    cfg.Customer,
		bank:        cfg.Bank,
		paymentType: cfg.PaymentType,
		logger:      cfg.Logger,
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

	var posted, failed int
	for _, p := range rows {
		if err := ctx.Err(); err != nil {
			s.logger.Info("payments cycle aborted", "error", err)
			return
		}

		receipt := syspro.CashReceipt{
			CustomerCode:  s.customer,
			Bank:          s.bank,
			PaymentType:   s.paymentType,
			InvoiceNumber: p.OrderNumber,
			Amount:        p.GrossAmount,
			BankCharges:   p.FeeAmount,
			Currency:      p.Currency,
			PaymentMethod: p.PaymentGateway,
			PostedAt:      p.ProcessedAt,
		}
		ref, err := s.poster.PostCashReceipt(ctx, receipt)
		if err == nil {
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
			continue
		}
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
	s.logger.Info("payments cycle complete",
		"posted", posted,
		"failed", failed,
	)
}
