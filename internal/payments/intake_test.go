package payments

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

type fakeIntakeSource struct {
	mu        sync.Mutex
	calls     int
	sinceSeen time.Time
	untilSeen time.Time
	toReturn  []model.Order
	err       error
}

func (f *fakeIntakeSource) FetchOrdersByDateRange(ctx context.Context, start, end time.Time) ([]model.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.sinceSeen = start
	f.untilSeen = end
	if f.err != nil {
		return nil, f.err
	}
	return f.toReturn, nil
}

func newIntakeTestReporter(t *testing.T, src IntakeSource, send EmailSender) *IntakeReporter {
	t.Helper()
	r, err := NewIntakeReporter(IntakeReporterConfig{
		Source:     src,
		Mailer:     send,
		Recipients: []string{"ops@example.com"},
		StoreName:  "Barbequick",
		Hour:       6,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewIntakeReporter: %v", err)
	}
	return r
}

func TestIntake_SendForDate_Happy(t *testing.T) {
	ctx := context.Background()
	orders := []model.Order{
		{OrderNumber: "#BBQ1001", ShopifyOrderID: 1, Status: model.OrderStatusFulfilled, SysproOrderNumber: "016001", PaymentAmount: 45.50, ShipFirstName: "Alice", ShipLastName: "Doe", CreatedAt: time.Now().UTC()},
		{OrderNumber: "#BBQ1002", ShopifyOrderID: 2, Status: model.OrderStatusSubmitted, SysproOrderNumber: "", PaymentAmount: 22.00, CreatedAt: time.Now().UTC()}, // stuck
		{OrderNumber: "#BBQ1003", ShopifyOrderID: 3, Status: model.OrderStatusFailed, LastError: "VAT strip failed", PaymentAmount: 10.00, CreatedAt: time.Now().UTC()},
		{OrderNumber: "#BBQ1004", ShopifyOrderID: 4, Status: model.OrderStatusPending, PaymentAmount: 99.99, CreatedAt: time.Now().UTC()},
	}
	src := &fakeIntakeSource{toReturn: orders}
	send := &fakeSender{}
	r := newIntakeTestReporter(t, src, send)

	date := time.Date(2026, 4, 23, 0, 0, 0, 0, time.UTC)
	if err := r.SendForDate(ctx, date); err != nil {
		t.Fatalf("SendForDate: %v", err)
	}

	if src.calls != 1 {
		t.Errorf("src.calls = %d, want 1", src.calls)
	}
	// Window check: start = 2026-04-23T00:00Z, end = 2026-04-24T00:00Z
	if !src.sinceSeen.Equal(date) {
		t.Errorf("since = %v, want %v", src.sinceSeen, date)
	}
	if !src.untilSeen.Equal(date.AddDate(0, 0, 1)) {
		t.Errorf("until = %v, want %v", src.untilSeen, date.AddDate(0, 0, 1))
	}
	if send.calls != 1 {
		t.Errorf("send.calls = %d, want 1", send.calls)
	}
	if !strings.Contains(send.lastSubject, "4 orders") {
		t.Errorf("subject = %q, want '4 orders'", send.lastSubject)
	}
	if !strings.Contains(send.lastSubject, "£177.49") {
		t.Errorf("subject = %q, want '£177.49'", send.lastSubject)
	}
	// Body is HTML (Mailer will detect `<` and send as HTML).
	if !strings.Contains(send.lastBody, "<strong>4</strong>") {
		t.Errorf("body missing count, got %q", send.lastBody)
	}
	if !strings.Contains(send.lastBody, "Stuck") {
		t.Errorf("body missing Stuck section for BBQ1002")
	}
	// Attachment is CSV.
	if send.lastAtt == nil {
		t.Fatal("no attachment")
	}
	if !strings.HasSuffix(send.lastAtt.Filename, ".csv") {
		t.Errorf("filename = %q", send.lastAtt.Filename)
	}
	csv := string(send.lastAtt.Body)
	if !strings.Contains(csv, "#BBQ1001") || !strings.Contains(csv, "#BBQ1003") {
		t.Errorf("csv missing order rows: %q", csv)
	}
	if !strings.Contains(csv, "VAT strip failed") {
		t.Error("csv missing last_error column")
	}
	if !strings.Contains(csv, "016001") {
		t.Error("csv missing SYSPRO SO column")
	}
}

func TestIntake_SendForDate_ZeroOrdersStillSends(t *testing.T) {
	ctx := context.Background()
	src := &fakeIntakeSource{toReturn: nil}
	send := &fakeSender{}
	r := newIntakeTestReporter(t, src, send)

	if err := r.SendForDate(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("SendForDate: %v", err)
	}
	if send.calls != 1 {
		t.Errorf("zero-order day should still send (liveness), got send.calls = %d", send.calls)
	}
	if !strings.Contains(send.lastSubject, "0 orders") {
		t.Errorf("subject = %q", send.lastSubject)
	}
}

func TestIntake_SendForDate_SourceError(t *testing.T) {
	ctx := context.Background()
	src := &fakeIntakeSource{err: errors.New("boom")}
	send := &fakeSender{}
	r := newIntakeTestReporter(t, src, send)

	err := r.SendForDate(ctx, time.Now().UTC())
	if err == nil {
		t.Fatal("expected error from source failure")
	}
	if send.calls != 0 {
		t.Errorf("send should not fire on source error, got calls = %d", send.calls)
	}
}

func TestIntake_SummariseIntake(t *testing.T) {
	orders := []model.Order{
		{Status: model.OrderStatusFulfilled, PaymentAmount: 10},
		{Status: model.OrderStatusSubmitted, SysproOrderNumber: "1", PaymentAmount: 20},
		{Status: model.OrderStatusSubmitted, SysproOrderNumber: "", PaymentAmount: 30}, // stuck
		{Status: model.OrderStatusPending, PaymentAmount: 40},
		{Status: model.OrderStatusProcessing, PaymentAmount: 50}, // counted as pending
		{Status: model.OrderStatusFailed, PaymentAmount: 60},
		{Status: model.OrderStatusDeadLetter, PaymentAmount: 70},
		{Status: model.OrderStatusCancelled, PaymentAmount: 80},
	}
	s := summariseIntake(orders)
	if s.Count != 8 {
		t.Errorf("Count = %d, want 8", s.Count)
	}
	if s.GrossTotal != 360 {
		t.Errorf("GrossTotal = %v, want 360", s.GrossTotal)
	}
	if s.Pending != 2 {
		t.Errorf("Pending = %d, want 2 (pending + processing)", s.Pending)
	}
	if s.Submitted != 2 {
		t.Errorf("Submitted = %d, want 2", s.Submitted)
	}
	if s.Stuck != 1 {
		t.Errorf("Stuck = %d, want 1", s.Stuck)
	}
	if s.Fulfilled != 1 || s.Failed != 1 || s.DeadLetter != 1 || s.Cancelled != 1 {
		t.Errorf("status counts off: %+v", s)
	}
}

func TestIntake_Validation(t *testing.T) {
	_, err := NewIntakeReporter(IntakeReporterConfig{})
	if err == nil {
		t.Error("want error for empty config")
	}
	_, err = NewIntakeReporter(IntakeReporterConfig{Source: &fakeIntakeSource{}, Mailer: &fakeSender{}, Recipients: []string{"a@b"}, Hour: 25})
	if err == nil {
		t.Error("want error for hour out of range")
	}
}
