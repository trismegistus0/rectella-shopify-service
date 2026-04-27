package fulfilment

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

type mockDispatchQuerier struct {
	mu      sync.Mutex
	results map[string]syspro.SORQRYResult
	err     error
	calls   int
}

func (m *mockDispatchQuerier) QueryDispatchedOrders(ctx context.Context, orderNumbers []string) (map[string]syspro.SORQRYResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.results, nil
}

type mockFulfilmentPusher struct {
	mu               sync.Mutex
	foIDs            map[int64]string // shopifyOrderID → fulfillmentOrderID
	foErr            error
	createResults    map[string]string // fulfillmentOrderID → fulfilmentGID
	createErr        error
	getCalls         int
	createCalls      int
	lastCreateInputs []FulfilmentInput
}

func (m *mockFulfilmentPusher) GetFulfillmentOrderID(ctx context.Context, shopifyOrderID int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	if m.foErr != nil {
		return "", m.foErr
	}
	if id, ok := m.foIDs[shopifyOrderID]; ok {
		return id, nil
	}
	return "", fmt.Errorf("no fulfillment order for %d", shopifyOrderID)
}

func (m *mockFulfilmentPusher) CreateFulfillment(ctx context.Context, input FulfilmentInput) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	m.lastCreateInputs = append(m.lastCreateInputs, input)
	if m.createErr != nil {
		return "", m.createErr
	}
	if gid, ok := m.createResults[input.FulfillmentOrderID]; ok {
		return gid, nil
	}
	return "gid://shopify/Fulfillment/default", nil
}

type mockFulfilmentStore struct {
	mu            sync.Mutex
	orders        []model.Order
	fetchErr      error
	updateErr     error
	fetchCalls    int
	updateCalls   int
	updatedOrders map[int64]string // orderID → fulfilmentGID
}

func (m *mockFulfilmentStore) FetchSubmittedOrders(ctx context.Context) ([]model.Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetchCalls++
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}
	return m.orders, nil
}

func (m *mockFulfilmentStore) UpdateOrderFulfilled(ctx context.Context, orderID int64, shopifyFulfilmentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls++
	if m.updateErr != nil {
		return m.updateErr
	}
	if m.updatedOrders == nil {
		m.updatedOrders = make(map[int64]string)
	}
	m.updatedOrders[orderID] = shopifyFulfilmentID
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSyncer_EmptyBatch(t *testing.T) {
	q := &mockDispatchQuerier{}
	p := &mockFulfilmentPusher{}
	s := &mockFulfilmentStore{orders: nil}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())
	if q.calls != 0 {
		t.Errorf("expected 0 SORQRY calls for empty batch, got %d", q.calls)
	}
	if p.getCalls != 0 {
		t.Errorf("expected 0 Shopify calls, got %d", p.getCalls)
	}
}

func TestSyncer_Success_OrderFulfilled(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{
			"015562": {SalesOrder: "015562", OrderStatus: "9", Carrier: "Avanti"},
		},
	}
	p := &mockFulfilmentPusher{
		foIDs:         map[int64]string{1001: "gid://shopify/FulfillmentOrder/100"},
		createResults: map[string]string{"gid://shopify/FulfillmentOrder/100": "gid://shopify/Fulfillment/200"},
	}
	s := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	if q.calls != 1 {
		t.Fatalf("expected 1 SORQRY call, got %d", q.calls)
	}
	if p.createCalls != 1 {
		t.Fatalf("expected 1 CreateFulfillment call, got %d", p.createCalls)
	}
	if s.updateCalls != 1 {
		t.Fatalf("expected 1 UpdateOrderFulfilled call, got %d", s.updateCalls)
	}
	if gid := s.updatedOrders[1]; gid != "gid://shopify/Fulfillment/200" {
		t.Errorf("expected fulfilment GID, got %q", gid)
	}
	// Verify tracking info passed through.
	if len(p.lastCreateInputs) != 1 {
		t.Fatalf("expected 1 create input, got %d", len(p.lastCreateInputs))
	}
	if p.lastCreateInputs[0].Carrier != "Avanti" {
		t.Errorf("expected carrier Avanti, got %q", p.lastCreateInputs[0].Carrier)
	}
}

func TestSyncer_NonDispatched_Skipped(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{
			"015562": {SalesOrder: "015562", OrderStatus: "1"}, // open, not complete
		},
	}
	p := &mockFulfilmentPusher{}
	s := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	if p.getCalls != 0 {
		t.Errorf("expected no Shopify calls for non-dispatched order, got %d", p.getCalls)
	}
	if s.updateCalls != 0 {
		t.Errorf("expected no DB updates, got %d", s.updateCalls)
	}
}

// TestSyncer_CancelledOrder_Skipped verifies the SYSPRO cancellation status
// "\\" is NOT treated as dispatch-complete. A cancelled SYSPRO order must
// never create a Shopify fulfilment. Regression guard for scenario R from
// the overnight plan.
func TestSyncer_CancelledOrder_Skipped(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{
			"015562": {SalesOrder: "015562", OrderStatus: "\\"}, // cancelled per CLAUDE.md
		},
	}
	p := &mockFulfilmentPusher{}
	s := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	if p.getCalls != 0 {
		t.Errorf("expected no GetFulfillmentOrderID calls for cancelled order, got %d", p.getCalls)
	}
	if p.createCalls != 0 {
		t.Errorf("expected no CreateFulfillment calls for cancelled order, got %d", p.createCalls)
	}
	if s.updateCalls != 0 {
		t.Errorf("expected no DB updates, got %d", s.updateCalls)
	}
}

func TestSyncer_SORQRYFailure_NoFulfilments(t *testing.T) {
	q := &mockDispatchQuerier{err: fmt.Errorf("syspro logon failed")}
	p := &mockFulfilmentPusher{}
	s := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	if p.getCalls != 0 {
		t.Errorf("expected no Shopify calls on SORQRY failure, got %d", p.getCalls)
	}
}

func TestSyncer_StoreFetchError(t *testing.T) {
	q := &mockDispatchQuerier{}
	p := &mockFulfilmentPusher{}
	s := &mockFulfilmentStore{fetchErr: fmt.Errorf("db connection lost")}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	if q.calls != 0 {
		t.Errorf("expected no SORQRY calls on store error, got %d", q.calls)
	}
}

func TestSyncer_GetFulfilmentOrderIDFailure_Continues(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{
			"015562": {SalesOrder: "015562", OrderStatus: "9"},
			"015563": {SalesOrder: "015563", OrderStatus: "9"},
		},
	}
	p := &mockFulfilmentPusher{
		foIDs:         map[int64]string{1002: "gid://shopify/FulfillmentOrder/102"},
		createResults: map[string]string{"gid://shopify/FulfillmentOrder/102": "gid://shopify/Fulfillment/202"},
	}
	s := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
			{ID: 2, ShopifyOrderID: 1002, SysproOrderNumber: "015563", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	// First order fails GetFulfillmentOrderID (no mock entry for 1001), second succeeds.
	if s.updateCalls != 1 {
		t.Errorf("expected 1 successful update (second order), got %d", s.updateCalls)
	}
	if _, ok := s.updatedOrders[2]; !ok {
		t.Error("expected order 2 to be updated")
	}
}

func TestSyncer_CreateFulfilmentFailure_Continues(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{
			"015562": {SalesOrder: "015562", OrderStatus: "9"},
		},
	}
	p := &mockFulfilmentPusher{
		foIDs:     map[int64]string{1001: "gid://shopify/FulfillmentOrder/100"},
		createErr: fmt.Errorf("shopify 500"),
	}
	s := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	if s.updateCalls != 0 {
		t.Errorf("expected no DB update on Shopify create failure, got %d", s.updateCalls)
	}
}

func TestSyncer_UpdateOrderFulfilledFailure_Continues(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{
			"015562": {SalesOrder: "015562", OrderStatus: "9"},
			"015563": {SalesOrder: "015563", OrderStatus: "9"},
		},
	}
	p := &mockFulfilmentPusher{
		foIDs: map[int64]string{
			1001: "gid://shopify/FulfillmentOrder/100",
			1002: "gid://shopify/FulfillmentOrder/102",
		},
	}
	s := &mockFulfilmentStore{
		updateErr: fmt.Errorf("db write failed"),
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
			{ID: 2, ShopifyOrderID: 1002, SysproOrderNumber: "015563", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	// Both orders attempted Shopify fulfilment despite DB failures.
	if p.createCalls != 2 {
		t.Errorf("expected 2 Shopify create calls, got %d", p.createCalls)
	}
}

func TestSyncer_OrderWithoutSysproNumber_Skipped(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{},
	}
	p := &mockFulfilmentPusher{}
	s := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	// Empty SYSPRO order number → filtered out → no SORQRY call.
	if q.calls != 0 {
		t.Errorf("expected 0 SORQRY calls for order without SYSPRO number, got %d", q.calls)
	}
}

func TestSyncer_Run_StopsOnContextCancel(t *testing.T) {
	q := &mockDispatchQuerier{}
	p := &mockFulfilmentPusher{}
	s := &mockFulfilmentStore{orders: nil}
	syncer := NewFulfilmentSyncer(q, p, s, 50*time.Millisecond, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		syncer.Run(ctx)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

func TestSyncer_MultipleBatches(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{
			"015562": {SalesOrder: "015562", OrderStatus: "9"},
			"015563": {SalesOrder: "015563", OrderStatus: "9", Carrier: "DPD"},
			"015564": {SalesOrder: "015564", OrderStatus: "1"}, // not complete
		},
	}
	p := &mockFulfilmentPusher{
		foIDs: map[int64]string{
			1001: "gid://shopify/FulfillmentOrder/100",
			1002: "gid://shopify/FulfillmentOrder/102",
			1003: "gid://shopify/FulfillmentOrder/104",
		},
	}
	s := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
			{ID: 2, ShopifyOrderID: 1002, SysproOrderNumber: "015563", Status: model.OrderStatusSubmitted},
			{ID: 3, ShopifyOrderID: 1003, SysproOrderNumber: "015564", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, s, time.Hour, testLogger())
	syncer.processOrders(context.Background())

	// Only 2 of 3 orders are status "9" → 2 fulfilments created.
	if p.createCalls != 2 {
		t.Errorf("expected 2 create calls, got %d", p.createCalls)
	}
	if s.updateCalls != 2 {
		t.Errorf("expected 2 updates, got %d", s.updateCalls)
	}
	if _, ok := s.updatedOrders[1]; !ok {
		t.Error("expected order 1 to be fulfilled")
	}
	if _, ok := s.updatedOrders[2]; !ok {
		t.Error("expected order 2 to be fulfilled")
	}
	if _, ok := s.updatedOrders[3]; ok {
		t.Error("order 3 (status 1) should NOT be fulfilled")
	}
}

// TestIsShopifyAPIError separates per-order benign conditions from
// systemic API failures. Per-order "no open fulfillment orders found"
// must NOT count as an API error (otherwise orders that were already
// manually fulfilled in Shopify pre-go-live would page the operator
// every cycle).
func TestIsShopifyAPIError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"no open fulfillment orders", fmt.Errorf("no open fulfillment orders found for order 1234"), false},
		{"graphql access denied", fmt.Errorf("querying fulfillment orders: graphql errors: Access denied for fulfillmentOrders field."), true},
		{"network failure", fmt.Errorf("querying fulfillment orders: dial tcp: connection refused"), true},
		{"create fulfilment 5xx", fmt.Errorf("graphql errors: 500 internal server error"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isShopifyAPIError(tt.err); got != tt.want {
				t.Errorf("isShopifyAPIError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestHandleAPIErrorEscalation verifies the rate-limit + consecutive-cycle
// gate. Single-cycle blip must NOT page; >= 2 consecutive bad cycles must
// page once; subsequent cycles within the hour must NOT re-page; clean
// cycle must reset the counter.
func TestHandleAPIErrorEscalation(t *testing.T) {
	syncer := NewFulfilmentSyncer(nil, nil, nil, time.Hour, testLogger())
	syncer.SetNtfyTopic("test-topic")

	// Cycle 1: API error. Must NOT page (need 2 consecutive).
	syncer.handleAPIErrorEscalation(5, "graphql errors: Access denied", 50)
	if !syncer.lastNotifiedAt.IsZero() {
		t.Error("first failed cycle should not page")
	}
	if syncer.consecutiveFailureCycles != 1 {
		t.Errorf("expected 1 consecutive failure, got %d", syncer.consecutiveFailureCycles)
	}

	// Cycle 2: API error again. Must page (>= 2 consecutive).
	syncer.handleAPIErrorEscalation(5, "graphql errors: Access denied", 50)
	if syncer.lastNotifiedAt.IsZero() {
		t.Error("second consecutive failed cycle should page")
	}
	if syncer.consecutiveFailureCycles != 2 {
		t.Errorf("expected 2 consecutive failures, got %d", syncer.consecutiveFailureCycles)
	}
	firstPing := syncer.lastNotifiedAt

	// Cycle 3: API error within rate-limit window. Must NOT re-page.
	syncer.handleAPIErrorEscalation(5, "graphql errors: Access denied", 50)
	if !syncer.lastNotifiedAt.Equal(firstPing) {
		t.Error("third consecutive failed cycle within rate-limit window should NOT re-page")
	}

	// Cycle 4: Clean cycle. Must reset counter.
	syncer.handleAPIErrorEscalation(0, "", 50)
	if syncer.consecutiveFailureCycles != 0 {
		t.Errorf("clean cycle should reset counter, got %d", syncer.consecutiveFailureCycles)
	}
	if syncer.lastShopifyErrorMessage != "" {
		t.Errorf("clean cycle should clear last error, got %q", syncer.lastShopifyErrorMessage)
	}
}

// TestHandleAPIErrorEscalation_RateLimitExpiry verifies that after the
// 1-hour rate-limit window elapses, a still-failing fulfilment syncer
// pages again (so a sustained outage isn't permanently muted by the
// initial ping).
func TestHandleAPIErrorEscalation_RateLimitExpiry(t *testing.T) {
	syncer := NewFulfilmentSyncer(nil, nil, nil, time.Hour, testLogger())
	syncer.SetNtfyTopic("test-topic")

	syncer.handleAPIErrorEscalation(5, "graphql errors: Access denied", 50)
	syncer.handleAPIErrorEscalation(5, "graphql errors: Access denied", 50)
	firstPing := syncer.lastNotifiedAt
	if firstPing.IsZero() {
		t.Fatal("expected first ping to fire")
	}

	// Pretend the last ping happened 90 minutes ago.
	syncer.mu.Lock()
	syncer.lastNotifiedAt = time.Now().Add(-90 * time.Minute)
	syncer.mu.Unlock()
	prev := syncer.lastNotifiedAt

	// Another bad cycle — past the rate-limit window. Must re-page.
	syncer.handleAPIErrorEscalation(5, "graphql errors: Access denied", 50)
	if !syncer.lastNotifiedAt.After(prev) {
		t.Errorf("after rate-limit expiry, sustained outage should re-page; lastNotifiedAt=%v prev=%v", syncer.lastNotifiedAt, prev)
	}
}

// TestProcessOrders_BenignErrorsDontEscalate ensures that "no open
// fulfillment orders found" errors (the post-fix steady state for
// Rectella's pre-go-live orders that were already manually fulfilled
// in Shopify) do NOT trip the API-error counter.
func TestProcessOrders_BenignErrorsDontEscalate(t *testing.T) {
	q := &mockDispatchQuerier{
		results: map[string]syspro.SORQRYResult{
			"015562": {SalesOrder: "015562", OrderStatus: "9"},
			"015563": {SalesOrder: "015563", OrderStatus: "9"},
		},
	}
	p := &mockFulfilmentPusher{
		// No matching foIDs — pusher returns "no fulfillment order for X"
		// which the production shopify.go phrases as "no open fulfillment
		// orders found". Patch the mock to mirror that exact substring.
		foIDs: nil,
		foErr: fmt.Errorf("no open fulfillment orders found for order 1001"),
	}
	store := &mockFulfilmentStore{
		orders: []model.Order{
			{ID: 1, ShopifyOrderID: 1001, SysproOrderNumber: "015562", Status: model.OrderStatusSubmitted},
			{ID: 2, ShopifyOrderID: 1002, SysproOrderNumber: "015563", Status: model.OrderStatusSubmitted},
		},
	}
	syncer := NewFulfilmentSyncer(q, p, store, time.Hour, testLogger())
	syncer.SetNtfyTopic("test-topic")

	syncer.processOrders(context.Background())
	syncer.processOrders(context.Background())

	// Despite 4 errors across 2 cycles, none are API-class; counter must stay 0.
	if syncer.consecutiveFailureCycles != 0 {
		t.Errorf("benign 'no open fulfillment orders' must NOT increment failure cycles, got %d", syncer.consecutiveFailureCycles)
	}
	if !syncer.lastNotifiedAt.IsZero() {
		t.Error("benign errors must NOT trigger ntfy")
	}
}
