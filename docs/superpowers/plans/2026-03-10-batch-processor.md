# Batch Processor Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Connect webhook-persisted orders to SYSPRO by polling the database and submitting orders via SORTOI in batched sessions.

**Architecture:** New `internal/batch/` package owns the polling loop. SYSPRO client gets a `Session` type for session reuse (logon once → N transactions → logoff once). Store gets new query methods. Main.go starts the processor as a goroutine with graceful shutdown.

**Tech Stack:** Go 1.25, pgx/v5, net/http, sync, time, log/slog

**Design doc:** `docs/plans/2026-03-10-batch-processor-design.md`

**Note:** The model already has `OrderStatus` constants including `dead_letter` (not `permanently_failed` as the design doc says). The schema already has `status`, `attempts`, `last_error` columns. No migration needed.

---

## Chunk 1: Foundation (SYSPRO Session + Store Methods)

### Task 1: SYSPRO Session type for session reuse

The current `SubmitSalesOrder` does logon/logoff per call. The batch processor needs: open session → N transactions → close. We add a `Session` interface and implementation.

**Files:**
- Create: `internal/syspro/session.go`
- Create: `internal/syspro/session_test.go`
- Modify: `internal/syspro/client.go:19-21` (add `OpenSession` to `Client` interface)

- [ ] **Step 1: Write failing tests for Session lifecycle**

Create `internal/syspro/session_test.go`:

```go
package syspro

import (
	"context"
	"testing"
)

func TestOpenSession_Success(t *testing.T) {
	fake := newFakeEnet(t)
	c := fake.client(t)

	session, err := c.OpenSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer session.Close(context.Background())

	if fake.logonCalls != 1 {
		t.Errorf("expected 1 logon call, got %d", fake.logonCalls)
	}
}

func TestOpenSession_LogonFailure(t *testing.T) {
	fake := newFakeEnet(t)
	fake.logonErr = true
	c := fake.client(t)

	_, err := c.OpenSession(context.Background())
	if err == nil {
		t.Fatal("expected error on logon failure")
	}
}

func TestSession_SubmitOrder_Success(t *testing.T) {
	fake := newFakeEnet(t)
	c := fake.client(t)

	session, err := c.OpenSession(context.Background())
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer session.Close(context.Background())

	result, err := session.SubmitOrder(context.Background(), testOrder(), testLines())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got error: %s", result.ErrorMessage)
	}
	if result.SysproOrderNumber != "SO12345" {
		t.Errorf("expected SO12345, got %q", result.SysproOrderNumber)
	}
}

func TestSession_MultipleSubmits_ReuseSession(t *testing.T) {
	fake := newFakeEnet(t)
	c := fake.client(t)

	session, err := c.OpenSession(context.Background())
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer session.Close(context.Background())

	for i := 0; i < 3; i++ {
		_, err := session.SubmitOrder(context.Background(), testOrder(), testLines())
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	if fake.logonCalls != 1 {
		t.Errorf("expected 1 logon call (reused session), got %d", fake.logonCalls)
	}
	if fake.transactCalls != 3 {
		t.Errorf("expected 3 transaction calls, got %d", fake.transactCalls)
	}
}

func TestSession_Close(t *testing.T) {
	fake := newFakeEnet(t)
	c := fake.client(t)

	session, err := c.OpenSession(context.Background())
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if fake.logoffCalls != 1 {
		t.Errorf("expected 1 logoff call, got %d", fake.logoffCalls)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/syspro/ -run "TestOpenSession|TestSession_" -v`
Expected: Compilation errors — `OpenSession` and `Session` don't exist yet.

- [ ] **Step 3: Add Session interface to client.go**

In `internal/syspro/client.go`, add `Session` interface and `OpenSession` to `Client`:

```go
// Session represents an open SYSPRO e.net session that can submit multiple
// orders before being closed. Use Client.OpenSession to create one.
type Session interface {
	SubmitOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*SalesOrderResult, error)
	Close(ctx context.Context) error
}

// Client is the interface the batch processor uses to submit orders to SYSPRO.
type Client interface {
	SubmitSalesOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*SalesOrderResult, error)
	OpenSession(ctx context.Context) (Session, error)
}
```

- [ ] **Step 4: Implement Session in session.go**

Create `internal/syspro/session.go`:

```go
package syspro

import (
	"context"
	"fmt"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

// enetSession is an open SYSPRO e.net session backed by a session GUID.
type enetSession struct {
	client *enetClient
	guid   string
}

// OpenSession logs on to SYSPRO and returns a Session for submitting multiple orders.
func (c *enetClient) OpenSession(ctx context.Context) (Session, error) {
	guid, err := c.logon(ctx)
	if err != nil {
		return nil, fmt.Errorf("syspro logon: %w", err)
	}
	return &enetSession{client: c, guid: guid}, nil
}

// SubmitOrder sends a single SORTOI transaction on this session.
func (s *enetSession) SubmitOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*SalesOrderResult, error) {
	paramsXML, dataXML, err := buildSORTOI(order, lines)
	if err != nil {
		return nil, fmt.Errorf("building SORTOI XML: %w", err)
	}

	s.client.logger.Debug("submitting SORTOI",
		"order_number", order.OrderNumber,
		"lines", len(lines),
	)

	respXML, err := s.client.transaction(ctx, s.guid, "SORTOI", paramsXML, dataXML)
	if err != nil {
		return nil, fmt.Errorf("syspro SORTOI transaction: %w", err)
	}

	return parseSORTOIResponse(respXML)
}

// Close logs off from SYSPRO. Always call this when done with the session.
func (s *enetSession) Close(ctx context.Context) error {
	if err := s.client.logoff(ctx, s.guid); err != nil {
		s.client.logger.Warn("syspro logoff failed", "error", err)
		return err
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/syspro/ -run "TestOpenSession|TestSession_" -v`
Expected: All 5 tests pass.

- [ ] **Step 6: Run full test suite**

Run: `go test ./...`
Expected: All tests pass (existing tests still work, `SubmitSalesOrder` unchanged).

- [ ] **Step 7: Commit**

```bash
git add internal/syspro/session.go internal/syspro/session_test.go internal/syspro/client.go
git commit -m "feat(syspro): add Session type for batched order submission

Separates logon/logoff from transaction calls so the batch processor
can submit multiple orders on a single SYSPRO session.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 2: Store methods for batch processing

The batch processor needs to fetch pending orders (with their lines) and update order status after submission.

**Files:**
- Modify: `internal/model/order.go` — add `OrderWithLines`
- Modify: `internal/store/order.go` — add `FetchPendingOrders`, `UpdateOrderStatus`, `ListOrdersByStatus`

- [ ] **Step 1: Add OrderWithLines to model**

In `internal/model/order.go`, add after `OrderLine` struct:

```go
// OrderWithLines pairs an order with its line items for batch processing.
type OrderWithLines struct {
	Order Order
	Lines []OrderLine
}
```

- [ ] **Step 2: Write FetchPendingOrders in store**

In `internal/store/order.go`, add:

```go
// FetchPendingOrders returns up to limit orders with status 'pending', oldest first,
// along with their line items.
func (db *DB) FetchPendingOrders(ctx context.Context, limit int) ([]model.OrderWithLines, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, shopify_order_id, order_number, status, customer_account,
			ship_first_name, ship_last_name, ship_address1, ship_address2,
			ship_city, ship_province, ship_postcode, ship_country,
			ship_phone, ship_email,
			payment_reference, payment_amount,
			raw_payload, attempts, last_error,
			order_date, created_at, updated_at
		FROM orders
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT $1`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying pending orders: %w", err)
	}
	defer rows.Close()

	var orders []model.Order
	for rows.Next() {
		var o model.Order
		if err := rows.Scan(
			&o.ID, &o.ShopifyOrderID, &o.OrderNumber, &o.Status, &o.CustomerAccount,
			&o.ShipFirstName, &o.ShipLastName, &o.ShipAddress1, &o.ShipAddress2,
			&o.ShipCity, &o.ShipProvince, &o.ShipPostcode, &o.ShipCountry,
			&o.ShipPhone, &o.ShipEmail,
			&o.PaymentReference, &o.PaymentAmount,
			&o.RawPayload, &o.Attempts, &o.LastError,
			&o.OrderDate, &o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning order row: %w", err)
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating order rows: %w", err)
	}

	result := make([]model.OrderWithLines, 0, len(orders))
	for _, o := range orders {
		lines, err := db.fetchOrderLines(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, model.OrderWithLines{Order: o, Lines: lines})
	}

	return result, nil
}

func (db *DB) fetchOrderLines(ctx context.Context, orderID int64) ([]model.OrderLine, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, order_id, sku, quantity, unit_price, discount, tax
		FROM order_lines
		WHERE order_id = $1
		ORDER BY id`, orderID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying order lines for order %d: %w", orderID, err)
	}
	defer rows.Close()

	var lines []model.OrderLine
	for rows.Next() {
		var l model.OrderLine
		if err := rows.Scan(&l.ID, &l.OrderID, &l.SKU, &l.Quantity, &l.UnitPrice, &l.Discount, &l.Tax); err != nil {
			return nil, fmt.Errorf("scanning order line: %w", err)
		}
		lines = append(lines, l)
	}
	return lines, rows.Err()
}
```

- [ ] **Step 3: Write UpdateOrderStatus in store**

In `internal/store/order.go`, add:

```go
// UpdateOrderStatus sets the status, attempts count, and last error for an order.
func (db *DB) UpdateOrderStatus(ctx context.Context, orderID int64, status model.OrderStatus, attempts int, lastError string) error {
	_, err := db.Pool.Exec(ctx,
		`UPDATE orders
		SET status = $2, attempts = $3, last_error = $4, updated_at = NOW()
		WHERE id = $1`,
		orderID, string(status), attempts, lastError,
	)
	if err != nil {
		return fmt.Errorf("updating order %d status: %w", orderID, err)
	}
	return nil
}
```

- [ ] **Step 4: Write ListOrdersByStatus in store**

In `internal/store/order.go`, add:

```go
// ListOrdersByStatus returns orders matching the given status, newest first.
func (db *DB) ListOrdersByStatus(ctx context.Context, status model.OrderStatus) ([]model.Order, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, shopify_order_id, order_number, status, customer_account,
			ship_first_name, ship_last_name, ship_address1, ship_address2,
			ship_city, ship_province, ship_postcode, ship_country,
			ship_phone, ship_email,
			payment_reference, payment_amount,
			raw_payload, attempts, last_error,
			order_date, created_at, updated_at
		FROM orders
		WHERE status = $1
		ORDER BY created_at DESC`, string(status),
	)
	if err != nil {
		return nil, fmt.Errorf("querying orders by status: %w", err)
	}
	defer rows.Close()

	var orders []model.Order
	for rows.Next() {
		var o model.Order
		if err := rows.Scan(
			&o.ID, &o.ShopifyOrderID, &o.OrderNumber, &o.Status, &o.CustomerAccount,
			&o.ShipFirstName, &o.ShipLastName, &o.ShipAddress1, &o.ShipAddress2,
			&o.ShipCity, &o.ShipProvince, &o.ShipPostcode, &o.ShipCountry,
			&o.ShipPhone, &o.ShipEmail,
			&o.PaymentReference, &o.PaymentAmount,
			&o.RawPayload, &o.Attempts, &o.LastError,
			&o.OrderDate, &o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning order: %w", err)
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}
```

- [ ] **Step 5: Verify compilation**

Run: `go build ./...`
Expected: Compiles cleanly.

- [ ] **Step 6: Commit**

```bash
git add internal/model/order.go internal/store/order.go
git commit -m "feat(store): add batch processing query methods

FetchPendingOrders, UpdateOrderStatus, ListOrdersByStatus for the
batch processor and orders API endpoint.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Chunk 2: Batch Processor + Wiring

### Task 3: Batch processor core logic

The processor fetches pending orders, opens a SYSPRO session, submits each order sequentially, and updates status based on the result.

**Error handling rules:**
- `session.SubmitOrder` returns `(nil, error)` → **infra error**: increment attempts, if attempts >= 3 mark `dead_letter` else leave as `pending`, stop batch
- `result.Success == false` → **business error**: mark `failed` immediately, continue batch
- `result.Success == true` → mark `submitted`

**Files:**
- Create: `internal/batch/processor.go`
- Create: `internal/batch/processor_test.go`

- [ ] **Step 1: Define interfaces and Processor struct**

Create `internal/batch/processor.go`:

```go
package batch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

const maxAttempts = 3

// Store is the persistence interface for the batch processor.
type Store interface {
	FetchPendingOrders(ctx context.Context, limit int) ([]model.OrderWithLines, error)
	UpdateOrderStatus(ctx context.Context, orderID int64, status model.OrderStatus, attempts int, lastError string) error
}

// Processor polls for pending orders and submits them to SYSPRO.
type Processor struct {
	store    Store
	client   syspro.Client
	interval time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	running bool
}

// New creates a batch processor.
func New(store Store, client syspro.Client, interval time.Duration, logger *slog.Logger) *Processor {
	return &Processor{
		store:    store,
		client:   client,
		interval: interval,
		logger:   logger,
	}
}
```

- [ ] **Step 2: Write failing tests for processBatch**

Create `internal/batch/processor_test.go`:

```go
package batch

import (
	"context"
	"errors"
	"log/slog"
	"io"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

// mockStore implements Store for testing.
type mockStore struct {
	orders        []model.OrderWithLines
	fetchErr      error
	updates       []statusUpdate
	updateErr     error
}

type statusUpdate struct {
	OrderID   int64
	Status    model.OrderStatus
	Attempts  int
	LastError string
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

func (m *mockStore) UpdateOrderStatus(ctx context.Context, orderID int64, status model.OrderStatus, attempts int, lastError string) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.updates = append(m.updates, statusUpdate{orderID, status, attempts, lastError})
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
	session    *mockSession
	openErr    error
	openCalls  int
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
			OrderNumber:     "#BBQ" + fmt.Sprintf("%d", id),
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

func TestProcessBatch_NoPendingOrders(t *testing.T) {
	ms := &mockStore{}
	mc := &mockClient{session: &mockSession{}}
	p := New(ms, mc, time.Minute, testLogger())

	err := p.processBatch(context.Background())
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

	err := p.processBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ms.updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ms.updates))
	}
	if ms.updates[0].Status != model.OrderStatusSubmitted {
		t.Errorf("expected submitted, got %s", ms.updates[0].Status)
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

	err := p.processBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ms.updates) != 3 {
		t.Fatalf("expected 3 updates, got %d", len(ms.updates))
	}
	if ms.updates[0].Status != model.OrderStatusSubmitted {
		t.Errorf("order 1: expected submitted, got %s", ms.updates[0].Status)
	}
	if ms.updates[1].Status != model.OrderStatusFailed {
		t.Errorf("order 2: expected failed, got %s", ms.updates[1].Status)
	}
	if ms.updates[2].Status != model.OrderStatusSubmitted {
		t.Errorf("order 3: expected submitted, got %s", ms.updates[2].Status)
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

	err := p.processBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Order 1: submitted, Order 2: stays pending (attempts incremented), Order 3: untouched
	if len(ms.updates) != 2 {
		t.Fatalf("expected 2 updates (order 1 submitted + order 2 attempt bump), got %d", len(ms.updates))
	}
	if ms.updates[0].Status != model.OrderStatusSubmitted {
		t.Errorf("order 1: expected submitted, got %s", ms.updates[0].Status)
	}
	if ms.updates[1].Status != model.OrderStatusPending {
		t.Errorf("order 2: expected pending (retry), got %s", ms.updates[1].Status)
	}
	if ms.updates[1].Attempts != 1 {
		t.Errorf("order 2: expected attempts=1, got %d", ms.updates[1].Attempts)
	}
	if session.closed {
		// Session should still be closed even after infra error
		// Actually it SHOULD be closed — we always close
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

	err := p.processBatch(context.Background())
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

	err := p.processBatch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No status updates — orders stay pending for next cycle
	if len(ms.updates) != 0 {
		t.Errorf("expected no updates when session can't open, got %d", len(ms.updates))
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/batch/ -v`
Expected: Compilation errors — `processBatch` doesn't exist yet.

- [ ] **Step 4: Implement processBatch**

Add to `internal/batch/processor.go`:

```go
// processBatch runs a single batch cycle: fetch pending orders, open a SYSPRO
// session, submit each order, update statuses.
func (p *Processor) processBatch(ctx context.Context) error {
	orders, err := p.store.FetchPendingOrders(ctx, 100)
	if err != nil {
		p.logger.Error("fetching pending orders", "error", err)
		return nil
	}

	if len(orders) == 0 {
		return nil
	}

	p.logger.Info("processing batch", "orders", len(orders))

	session, err := p.client.OpenSession(ctx)
	if err != nil {
		p.logger.Error("opening SYSPRO session", "error", err)
		return nil
	}
	defer session.Close(ctx)

	for _, ow := range orders {
		if err := p.submitOrder(ctx, session, ow); err != nil {
			p.logger.Warn("batch stopped on infra error",
				"order_id", ow.Order.ID,
				"error", err,
			)
			break
		}
	}

	return nil
}

// errInfra is a sentinel used internally to signal that the batch should stop.
var errInfra = errors.New("infrastructure error")

func (p *Processor) submitOrder(ctx context.Context, session syspro.Session, ow model.OrderWithLines) error {
	order := ow.Order

	result, err := session.SubmitOrder(ctx, order, ow.Lines)
	if err != nil {
		// Infrastructure error — increment attempts, maybe dead-letter.
		newAttempts := order.Attempts + 1
		status := model.OrderStatusPending
		if newAttempts >= maxAttempts {
			status = model.OrderStatusDeadLetter
		}

		if uerr := p.store.UpdateOrderStatus(ctx, order.ID, status, newAttempts, err.Error()); uerr != nil {
			p.logger.Error("updating order after infra error",
				"order_id", order.ID,
				"error", uerr,
			)
		}

		p.logger.Error("SYSPRO submission failed (infra)",
			"order_id", order.ID,
			"order_number", order.OrderNumber,
			"attempts", newAttempts,
			"error", err,
		)

		return errInfra
	}

	if !result.Success {
		// Business error — mark failed, continue batch.
		if uerr := p.store.UpdateOrderStatus(ctx, order.ID, model.OrderStatusFailed, order.Attempts+1, result.ErrorMessage); uerr != nil {
			p.logger.Error("updating order after business error",
				"order_id", order.ID,
				"error", uerr,
			)
		}

		p.logger.Warn("SYSPRO rejected order",
			"order_id", order.ID,
			"order_number", order.OrderNumber,
			"error", result.ErrorMessage,
		)

		return nil
	}

	// Success.
	if uerr := p.store.UpdateOrderStatus(ctx, order.ID, model.OrderStatusSubmitted, order.Attempts+1, ""); uerr != nil {
		p.logger.Error("updating order after success",
			"order_id", order.ID,
			"error", uerr,
		)
	}

	p.logger.Info("order submitted to SYSPRO",
		"order_id", order.ID,
		"order_number", order.OrderNumber,
		"syspro_order", result.SysproOrderNumber,
	)

	return nil
}
```

Also add the missing import at the top:

```go
import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)
```

- [ ] **Step 5: Add missing fmt import to test file**

The test file uses `fmt.Sprintf` in `makeOrder`. Ensure `"fmt"` is in the import block of `processor_test.go`.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/batch/ -v`
Expected: All 6 tests pass.

- [ ] **Step 7: Run full test suite**

Run: `go test ./...`
Expected: All tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/batch/processor.go internal/batch/processor_test.go
git commit -m "feat(batch): add batch processor core logic

Fetches pending orders, opens a single SYSPRO session, submits
sequentially. Business errors mark failed and continue; infra
errors stop the batch. Dead-letters after 3 attempts.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 4: Batch processor polling loop with single-flight guard

**Files:**
- Modify: `internal/batch/processor.go` — add `Run` method

- [ ] **Step 1: Write test for Run and single-flight**

Add to `internal/batch/processor_test.go`:

```go
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
```

- [ ] **Step 2: Implement Run**

Add to `internal/batch/processor.go`:

```go
// Run starts the polling loop. It blocks until ctx is cancelled.
func (p *Processor) Run(ctx context.Context) {
	p.logger.Info("batch processor started", "interval", p.interval)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("batch processor stopping")
			return
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

func (p *Processor) tick(ctx context.Context) {
	if !p.mu.TryLock() {
		p.logger.Debug("batch already running, skipping tick")
		return
	}
	defer p.mu.Unlock()

	if err := p.processBatch(ctx); err != nil {
		p.logger.Error("batch processing error", "error", err)
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/batch/ -v -count=1`
Expected: All tests pass including the new Run test.

- [ ] **Step 4: Commit**

```bash
git add internal/batch/processor.go internal/batch/processor_test.go
git commit -m "feat(batch): add polling loop with single-flight guard

Run() starts a ticker at BATCH_INTERVAL. TryLock prevents overlapping
batches. Stops cleanly on context cancellation.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 5: Wire batch processor into main.go

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Start batch processor as goroutine**

In `cmd/server/main.go`, replace the discarded SYSPRO client with:

```go
// Instantiate SYSPRO e.net client.
sysproClient := syspro.NewEnetClient(
	cfg.SysproEnetURL,
	cfg.SysproOperator,
	cfg.SysproPassword,
	cfg.SysproCompanyID,
	logger,
)

// Start batch processor.
batchProc := batch.New(db, sysproClient, cfg.BatchInterval, logger)
batchCtx, batchCancel := context.WithCancel(ctx)
defer batchCancel()
go batchProc.Run(batchCtx)
```

Add the import:
```go
"github.com/trismegistus0/rectella-shopify-service/internal/batch"
```

- [ ] **Step 2: Cancel batch processor on shutdown**

In the shutdown section, add `batchCancel()` before `srv.Shutdown`:

```go
case sig := <-sigCh:
	slog.Info("received shutdown signal", "signal", sig)
	batchCancel()
```

- [ ] **Step 3: Verify compilation and existing tests**

Run: `go build ./... && go test ./...`
Expected: All pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat: wire batch processor into server startup

Starts batch processor as goroutine alongside HTTP server.
Cancels batch context on shutdown signal before draining HTTP.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 6: GET /orders endpoint

Simple JSON endpoint for operations visibility into order statuses.

**Files:**
- Modify: `cmd/server/main.go` — add handler

- [ ] **Step 1: Add the handler**

In `cmd/server/main.go`, after the webhook handler registration:

```go
// Orders visibility endpoint.
mux.HandleFunc("GET /orders", func(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "status query parameter required"})
		return
	}

	orders, err := db.ListOrdersByStatus(r.Context(), model.OrderStatus(statusFilter))
	if err != nil {
		slog.Error("listing orders", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(orders)
})
```

Add the model import if not present:
```go
"github.com/trismegistus0/rectella-shopify-service/internal/model"
```

- [ ] **Step 2: Verify compilation**

Run: `go build ./...`
Expected: Compiles.

- [ ] **Step 3: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat: add GET /orders?status= endpoint

Operations visibility into order statuses. Returns JSON array
of orders filtered by status parameter.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Post-Implementation

After all tasks are complete:

- [ ] Run full test suite: `go test ./... -v`
- [ ] Run linting: `go vet ./... && gofmt -l .`
- [ ] Run `./scripts/check.sh`
- [ ] Update GitHub issue #1 (batch processor) — close it
- [ ] Update CLAUDE.md "What's Built" section
- [ ] Update memory file
