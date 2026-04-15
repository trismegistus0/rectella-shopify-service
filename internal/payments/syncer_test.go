package payments

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/store"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

type fakeStore struct {
	mu       sync.Mutex
	pending  []store.PaymentPosting
	posted   []int64
	failed   map[int64]string
	postErr  error
	fetchErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{failed: make(map[int64]string)}
}

func (s *fakeStore) FetchUnpostedPayments(ctx context.Context, limit int) ([]store.PaymentPosting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fetchErr != nil {
		return nil, s.fetchErr
	}
	out := make([]store.PaymentPosting, len(s.pending))
	copy(out, s.pending)
	return out, nil
}

func (s *fakeStore) MarkPaymentPosted(ctx context.Context, id int64, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.postErr != nil {
		return s.postErr
	}
	s.posted = append(s.posted, id)
	return nil
}

func (s *fakeStore) MarkPaymentFailed(ctx context.Context, id int64, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[id] = msg
	return nil
}

type fakePoster struct {
	refByID map[int64]string
	err     error
}

func (p *fakePoster) PostCashReceipt(ctx context.Context, r syspro.CashReceipt) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	// Echo the invoice back as a fake receipt ref.
	return "REF-" + r.InvoiceNumber, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func samplePayment(id int64, order string) store.PaymentPosting {
	return store.PaymentPosting{
		ID:                   id,
		ShopifyOrderID:       10000 + id,
		ShopifyTransactionID: 99000 + id,
		OrderNumber:          order,
		GrossAmount:          125.00,
		FeeAmount:            3.75,
		NetAmount:            121.25,
		Currency:             "GBP",
		PaymentGateway:       "shopify_payments",
		ProcessedAt:          time.Date(2026, 4, 15, 10, 30, 0, 0, time.UTC),
		Status:               "pending",
	}
}

func TestSyncer_EmptyPending(t *testing.T) {
	s := NewSyncer(newFakeStore(), &fakePoster{}, time.Minute, "WEBS01", testLogger())
	s.cycle(context.Background())
}

func TestSyncer_HappyPath(t *testing.T) {
	fs := newFakeStore()
	fs.pending = []store.PaymentPosting{samplePayment(1, "#BBQ1001"), samplePayment(2, "#BBQ1002")}
	s := NewSyncer(fs, &fakePoster{}, time.Minute, "WEBS01", testLogger())
	s.cycle(context.Background())
	if len(fs.posted) != 2 {
		t.Errorf("want 2 posted, got %d", len(fs.posted))
	}
	if len(fs.failed) != 0 {
		t.Errorf("want 0 failed, got %d", len(fs.failed))
	}
}

func TestSyncer_NotImplementedLeavesPending(t *testing.T) {
	fs := newFakeStore()
	fs.pending = []store.PaymentPosting{samplePayment(1, "#BBQ1001")}
	poster := &fakePoster{err: syspro.ErrCashReceiptNotImplemented}
	s := NewSyncer(fs, poster, time.Minute, "WEBS01", testLogger())
	s.cycle(context.Background())
	if len(fs.posted) != 0 {
		t.Errorf("want 0 posted, got %d", len(fs.posted))
	}
	if len(fs.failed) != 0 {
		t.Errorf("NotImplemented should not mark as failed, got %d", len(fs.failed))
	}
}

func TestSyncer_FailureMarksFailed(t *testing.T) {
	fs := newFakeStore()
	fs.pending = []store.PaymentPosting{samplePayment(1, "#BBQ1001")}
	poster := &fakePoster{err: errors.New("ARSPAY rejected: customer unknown")}
	s := NewSyncer(fs, poster, time.Minute, "WEBS01", testLogger())
	s.cycle(context.Background())
	if len(fs.failed) != 1 {
		t.Errorf("want 1 failed, got %d", len(fs.failed))
	}
	if fs.failed[1] == "" {
		t.Error("failed row should have error message")
	}
}

func TestSyncer_FetchError(t *testing.T) {
	fs := newFakeStore()
	fs.fetchErr = errors.New("db down")
	s := NewSyncer(fs, &fakePoster{}, time.Minute, "WEBS01", testLogger())
	s.cycle(context.Background())
}

func TestSyncer_ContextCanceledMidBatch(t *testing.T) {
	fs := newFakeStore()
	fs.pending = []store.PaymentPosting{
		samplePayment(1, "#BBQ1001"),
		samplePayment(2, "#BBQ1002"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewSyncer(fs, &fakePoster{}, time.Minute, "WEBS01", testLogger())
	s.cycle(ctx)
}
