package inventory

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type mockQuerier struct {
	mu    sync.Mutex
	stock map[string]float64
	err   error
	calls int
}

func (m *mockQuerier) QueryStock(ctx context.Context, skus []string, warehouse string) (map[string]float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.stock, nil
}

type mockPusher struct {
	mu        sync.Mutex
	lastPush  map[string]int
	err       error
	pushCalls int
}

func (m *mockPusher) SetInventoryLevels(ctx context.Context, quantities map[string]int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushCalls++
	if m.err != nil {
		return m.err
	}
	m.lastPush = make(map[string]int, len(quantities))
	for k, v := range quantities {
		m.lastPush[k] = v
	}
	return nil
}

type mockReservationStore struct {
	mu       sync.Mutex
	reserved map[string]int
	err      error
	calls    int
}

func (m *mockReservationStore) FetchReservedQuantities(ctx context.Context) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.reserved, nil
}

func syncerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSyncer_FullSync_ComputesEffectiveQuantity(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 120.0, "CBBQ0002": 50.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{"CBBQ0001": 3}}
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001", "CBBQ0002"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push, got %d", p.pushCalls)
	}
	if p.lastPush["CBBQ0001"] != 117 {
		t.Errorf("CBBQ0001: expected 117, got %d", p.lastPush["CBBQ0001"])
	}
	if p.lastPush["CBBQ0002"] != 50 {
		t.Errorf("CBBQ0002: expected 50, got %d", p.lastPush["CBBQ0002"])
	}
}

func TestSyncer_FullSync_ClampsNegatives(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 2.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{"CBBQ0001": 5}}
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.lastPush["CBBQ0001"] != 0 {
		t.Errorf("expected 0 (clamped), got %d", p.lastPush["CBBQ0001"])
	}
}

func TestSyncer_FullSync_SysproFailure_NoPush(t *testing.T) {
	q := &mockQuerier{err: fmt.Errorf("syspro logon failed")}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 0 {
		t.Errorf("expected no push on SYSPRO failure, got %d", p.pushCalls)
	}
}

func TestSyncer_FullSync_PartialSysproData(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 100.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001", "CBBQ0002"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push (partial data), got %d", p.pushCalls)
	}
	if len(p.lastPush) != 1 {
		t.Errorf("expected 1 SKU in push, got %d", len(p.lastPush))
	}
}

func TestSyncer_TriggeredSync_UsesCachedSyspro(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 100.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{"CBBQ0001": 2}}
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	initialQueryCalls := q.calls
	syncer.triggeredSync(context.Background())
	if q.calls != initialQueryCalls {
		t.Errorf("triggered sync should not query SYSPRO")
	}
	if s.calls != 2 {
		t.Errorf("expected 2 reservation store calls, got %d", s.calls)
	}
}

func TestSyncer_TriggeredSync_ColdCache_NoPush(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.triggeredSync(context.Background())
	if p.pushCalls != 0 {
		t.Errorf("expected no push on cold cache, got %d", p.pushCalls)
	}
}

func TestSyncer_Run_StopsOnContextCancel(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}
	syncer := NewSyncer(q, p, s, 50*time.Millisecond, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
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

func TestSyncer_ConsecutiveFailures(t *testing.T) {
	q := &mockQuerier{err: fmt.Errorf("syspro down")}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	syncer.fullSync(context.Background())
	syncer.fullSync(context.Background())
	if syncer.consecutiveFailures != 3 {
		t.Errorf("expected 3, got %d", syncer.consecutiveFailures)
	}
	q.err = nil
	q.stock = map[string]float64{"CBBQ0001": 10}
	syncer.fullSync(context.Background())
	if syncer.consecutiveFailures != 0 {
		t.Errorf("expected 0 after success, got %d", syncer.consecutiveFailures)
	}
}

func TestSyncer_ReservationStoreError_StillPushes(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 100.0}}
	p := &mockPusher{}
	s := &mockReservationStore{err: fmt.Errorf("db connection lost")}
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push even with DB error, got %d", p.pushCalls)
	}
	if p.lastPush["CBBQ0001"] != 100 {
		t.Errorf("expected 100 (no reservation subtracted), got %d", p.lastPush["CBBQ0001"])
	}
}

func TestSyncer_Debounce_CoalescesMultipleSignals(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 100.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}
	triggerCh := make(chan struct{}, 1)
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		triggerCh, syncerLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		syncer.Run(ctx)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	initialPushes := p.pushCalls
	p.mu.Unlock()
	triggerCh <- struct{}{}
	time.Sleep(500 * time.Millisecond)
	triggerCh <- struct{}{}
	time.Sleep(3 * time.Second)
	p.mu.Lock()
	pushesAfterDebounce := p.pushCalls
	p.mu.Unlock()
	if pushesAfterDebounce-initialPushes != 1 {
		t.Errorf("expected 1 debounced push, got %d", pushesAfterDebounce-initialPushes)
	}
	cancel()
	<-done
}

// --- Real-world stock data tests ---

func TestComputeEffective_RealWorldQuantities(t *testing.T) {
	// CBBQ0001 in RILT has stock across warehouses BURN (primary), AAAA, BQUR
	// — all returning 0. Verify computation handles zero-stock correctly.
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 0.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}
	syncer := NewSyncer(q, p, s, time.Hour, "BURN", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push, got %d", p.pushCalls)
	}
	if p.lastPush["CBBQ0001"] != 0 {
		t.Errorf("expected 0 for zero-stock SKU, got %d", p.lastPush["CBBQ0001"])
	}
}

func TestComputeEffective_WithReservedOrders(t *testing.T) {
	// SYSPRO shows 10 available, 3 pending orders with qty 2 each = 6 reserved.
	// Effective should be 10 - 6 = 4.
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 10.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{"CBBQ0001": 6}}
	syncer := NewSyncer(q, p, s, time.Hour, "BURN", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push, got %d", p.pushCalls)
	}
	if p.lastPush["CBBQ0001"] != 4 {
		t.Errorf("expected effective=4 (10 - 6), got %d", p.lastPush["CBBQ0001"])
	}
}

func TestComputeEffective_ReservedExceedsAvailable(t *testing.T) {
	// Reserved qty (15) exceeds available (8). Must clamp to 0, never go negative.
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 8.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{"CBBQ0001": 15}}
	syncer := NewSyncer(q, p, s, time.Hour, "BURN", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push, got %d", p.pushCalls)
	}
	if p.lastPush["CBBQ0001"] != 0 {
		t.Errorf("expected 0 (clamped), got %d", p.lastPush["CBBQ0001"])
	}
}

func TestComputeEffective_MultiSKU(t *testing.T) {
	// Multiple SKUs: some with reservations, some without.
	// Each should be computed independently.
	q := &mockQuerier{stock: map[string]float64{
		"CBBQ0001": 20.0,
		"CBBQ0002": 50.0,
		"CBBQ0003": 0.0,
		"CBBQ0004": 5.0,
	}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{
		"CBBQ0001": 3,
		"CBBQ0004": 10, // exceeds available — should clamp
	}}
	syncer := NewSyncer(q, p, s, time.Hour, "BURN",
		[]string{"CBBQ0001", "CBBQ0002", "CBBQ0003", "CBBQ0004"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push, got %d", p.pushCalls)
	}
	cases := map[string]int{
		"CBBQ0001": 17, // 20 - 3
		"CBBQ0002": 50, // 50 - 0
		"CBBQ0003": 0,  // 0 - 0
		"CBBQ0004": 0,  // 5 - 10, clamped to 0
	}
	for sku, want := range cases {
		if got := p.lastPush[sku]; got != want {
			t.Errorf("%s: expected %d, got %d", sku, want, got)
		}
	}
}

func TestComputeEffective_NoReservations(t *testing.T) {
	// No pending/processing orders at all. Effective = SYSPRO qty directly.
	q := &mockQuerier{stock: map[string]float64{
		"CBBQ0001": 75.0,
		"CBBQ0002": 120.0,
	}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}
	syncer := NewSyncer(q, p, s, time.Hour, "BURN", []string{"CBBQ0001", "CBBQ0002"},
		make(chan struct{}, 1), syncerLogger())
	syncer.fullSync(context.Background())
	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push, got %d", p.pushCalls)
	}
	if p.lastPush["CBBQ0001"] != 75 {
		t.Errorf("CBBQ0001: expected 75, got %d", p.lastPush["CBBQ0001"])
	}
	if p.lastPush["CBBQ0002"] != 120 {
		t.Errorf("CBBQ0002: expected 120, got %d", p.lastPush["CBBQ0002"])
	}
}
