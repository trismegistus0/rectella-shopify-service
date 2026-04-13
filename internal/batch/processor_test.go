package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

// mockStore implements Store for testing.
type mockStore struct {
	orders     []model.OrderWithLines
	fetchErr   error
	updates    []statusUpdate
	submitted  []submittedUpdate
	updateErr  error
	markingErr error
}

type statusUpdate struct {
	OrderID   int64
	Status    model.OrderStatus
	Attempts  int
	LastError string
}

type submittedUpdate struct {
	OrderID           int64
	SysproOrderNumber string
	Attempts          int
}

func (m *mockStore) FetchPendingOrders(ctx context.Context, limit int) ([]model.OrderWithLines, error) {
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}
	if limit < len(m.orders) {
		return m.orders[:limit], nil
	}
	return m.orders, nil
}

func (m *mockStore) MarkOrderProcessing(ctx context.Context, orderID int64) (bool, error) {
	if m.markingErr != nil {
		return false, m.markingErr
	}
	return true, nil
}

func (m *mockStore) UpdateOrderStatus(ctx context.Context, orderID int64, status model.OrderStatus, attempts int, lastError string) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.updates = append(m.updates, statusUpdate{orderID, status, attempts, lastError})
	return nil
}

func (m *mockStore) UpdateOrderSubmitted(ctx context.Context, orderID int64, sysproOrderNumber string, attempts int) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.submitted = append(m.submitted, submittedUpdate{orderID, sysproOrderNumber, attempts})
	return nil
}

// mockSession implements syspro.Session for testing.
type mockSession struct {
	results []syspro.SalesOrderResult
	errs    []error
	call    int
	closed  bool
}

func (s *mockSession) SubmitOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
	i := s.call
	s.call++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	if i < len(s.results) {
		return &s.results[i], nil
	}
	return &syspro.SalesOrderResult{Success: true, SysproOrderNumber: "SO00001"}, nil
}

func (s *mockSession) Close(ctx context.Context) error {
	s.closed = true
	return nil
}

// mockClient implements syspro.Client for testing.
type mockClient struct {
	session   *mockSession
	openErr   error
	openCalls int
}

func (c *mockClient) OpenSession(ctx context.Context) (syspro.Session, error) {
	c.openCalls++
	if c.openErr != nil {
		return nil, c.openErr
	}
	return c.session, nil
}

func (c *mockClient) SubmitSalesOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
	return nil, errors.New("use OpenSession for batch")
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeOrder(id int64, attempts int) model.OrderWithLines {
	return model.OrderWithLines{
		Order: model.Order{
			ID:              id,
			ShopifyOrderID:  id * 1000,
			OrderNumber:     fmt.Sprintf("#BBQ%d", id),
			Status:          model.OrderStatusPending,
			CustomerAccount: "WEBS01",
			Attempts:        attempts,
			OrderDate:       time.Now(),
		},
		Lines: []model.OrderLine{
			{SKU: "CBBQ0001", Quantity: 1, UnitPrice: 99.99},
		},
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	ms := &mockStore{}
	mc := &mockClient{session: &mockSession{}}
	p := New(ms, mc, 50*time.Millisecond, testLogger())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	// Let it tick at least once.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

func TestProcessBatch_NoPendingOrders(t *testing.T) {
	ms := &mockStore{}
	mc := &mockClient{session: &mockSession{}}
	p := New(ms, mc, time.Minute, testLogger())

	err := p.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.openCalls != 0 {
		t.Errorf("expected no session opened for empty batch, got %d", mc.openCalls)
	}
}

func TestProcessBatch_SingleOrderSuccess(t *testing.T) {
	ms := &mockStore{
		orders: []model.OrderWithLines{makeOrder(1, 0)},
	}
	session := &mockSession{
		results: []syspro.SalesOrderResult{
			{Success: true, SysproOrderNumber: "SO12345"},
		},
	}
	mc := &mockClient{session: session}
	p := New(ms, mc, time.Minute, testLogger())

	err := p.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ms.submitted) != 1 {
		t.Fatalf("expected 1 submitted update, got %d", len(ms.submitted))
	}
	if ms.submitted[0].SysproOrderNumber != "SO12345" {
		t.Errorf("expected syspro order SO12345, got %s", ms.submitted[0].SysproOrderNumber)
	}
	if !session.closed {
		t.Error("expected session to be closed")
	}
}

func TestProcessBatch_BusinessError_ContinuesBatch(t *testing.T) {
	ms := &mockStore{
		orders: []model.OrderWithLines{
			makeOrder(1, 0),
			makeOrder(2, 0),
			makeOrder(3, 0),
		},
	}
	session := &mockSession{
		results: []syspro.SalesOrderResult{
			{Success: true, SysproOrderNumber: "SO001"},
			{Success: false, ErrorMessage: "bad SKU"},
			{Success: true, SysproOrderNumber: "SO003"},
		},
	}
	mc := &mockClient{session: session}
	p := New(ms, mc, time.Minute, testLogger())

	err := p.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 submitted + 1 failed
	if len(ms.submitted) != 2 {
		t.Fatalf("expected 2 submitted updates, got %d", len(ms.submitted))
	}
	if len(ms.updates) != 1 {
		t.Fatalf("expected 1 status update (failed), got %d", len(ms.updates))
	}
	if ms.updates[0].Status != model.OrderStatusFailed {
		t.Errorf("order 2: expected failed, got %s", ms.updates[0].Status)
	}
}

func TestProcessBatch_InfraError_StopsBatch(t *testing.T) {
	ms := &mockStore{
		orders: []model.OrderWithLines{
			makeOrder(1, 0),
			makeOrder(2, 0),
			makeOrder(3, 0),
		},
	}
	session := &mockSession{
		results: []syspro.SalesOrderResult{
			{Success: true, SysproOrderNumber: "SO001"},
		},
		errs: []error{nil, errors.New("connection timeout"), nil},
	}
	mc := &mockClient{session: session}
	p := New(ms, mc, time.Minute, testLogger())

	err := p.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Order 1: submitted, Order 2: stays pending (attempts incremented), Order 3: untouched
	if len(ms.submitted) != 1 {
		t.Fatalf("expected 1 submitted update, got %d", len(ms.submitted))
	}
	if len(ms.updates) != 1 {
		t.Fatalf("expected 1 status update (infra error), got %d", len(ms.updates))
	}
	if ms.updates[0].Status != model.OrderStatusPending {
		t.Errorf("order 2: expected pending (retry), got %s", ms.updates[0].Status)
	}
	if ms.updates[0].Attempts != 1 {
		t.Errorf("order 2: expected attempts=1, got %d", ms.updates[0].Attempts)
	}
}

func TestProcessBatch_InfraError_DeadLetterAfter3(t *testing.T) {
	ms := &mockStore{
		orders: []model.OrderWithLines{makeOrder(1, 2)}, // already 2 attempts
	}
	session := &mockSession{
		errs: []error{errors.New("connection timeout")},
	}
	mc := &mockClient{session: session}
	p := New(ms, mc, time.Minute, testLogger())

	err := p.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ms.updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ms.updates))
	}
	if ms.updates[0].Status != model.OrderStatusDeadLetter {
		t.Errorf("expected dead_letter after 3rd attempt, got %s", ms.updates[0].Status)
	}
	if ms.updates[0].Attempts != 3 {
		t.Errorf("expected attempts=3, got %d", ms.updates[0].Attempts)
	}
}

func TestProcessBatch_OpenSessionFailure(t *testing.T) {
	ms := &mockStore{
		orders: []model.OrderWithLines{makeOrder(1, 0)},
	}
	mc := &mockClient{openErr: errors.New("VPN down")}
	p := New(ms, mc, time.Minute, testLogger())

	err := p.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No status updates — orders stay pending for next cycle
	if len(ms.updates) != 0 {
		t.Errorf("expected no updates when session can't open, got %d", len(ms.updates))
	}
}
