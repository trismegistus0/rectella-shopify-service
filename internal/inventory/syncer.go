package inventory

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"
)

// InventoryQuerier queries SYSPRO for stock levels.
type InventoryQuerier interface {
	QueryStock(ctx context.Context, skus []string, warehouse string) (map[string]float64, error)
}

// InventoryPusher sets stock levels in Shopify.
type InventoryPusher interface {
	SetInventoryLevels(ctx context.Context, quantities map[string]int) error
}

// ReservationStore queries pending order quantities from the database.
type ReservationStore interface {
	FetchReservedQuantities(ctx context.Context) (map[string]int, error)
}

// Syncer orchestrates one-way stock sync from SYSPRO to Shopify.
type Syncer struct {
	querier   InventoryQuerier
	pusher    InventoryPusher
	store     ReservationStore
	interval  time.Duration
	warehouse string
	skus      []string
	triggerCh <-chan struct{}
	logger    *slog.Logger

	syncMu              sync.Mutex // single-flight guard
	mu                  sync.Mutex // protects cachedStock + consecutiveFailures
	cachedStock         map[string]float64
	consecutiveFailures int
}

func NewSyncer(
	querier InventoryQuerier,
	pusher InventoryPusher,
	store ReservationStore,
	interval time.Duration,
	warehouse string,
	skus []string,
	triggerCh <-chan struct{},
	logger *slog.Logger,
) *Syncer {
	return &Syncer{
		querier:   querier,
		pusher:    pusher,
		store:     store,
		interval:  interval,
		warehouse: warehouse,
		skus:      skus,
		triggerCh: triggerCh,
		logger:    logger,
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	s.logger.Info("stock sync started", "interval", s.interval, "skus", len(s.skus))

	// First tick at T+0.
	s.tick(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("stock sync stopping")
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return
		case <-ticker.C:
			s.tick(ctx)
		case <-s.triggerCh:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(2 * time.Second)
			debounceCh = debounceTimer.C
		case <-debounceCh:
			debounceCh = nil
			s.triggeredTick(ctx)
		}
	}
}

func (s *Syncer) tick(ctx context.Context) {
	if !s.syncMu.TryLock() {
		s.logger.Debug("stock sync already running, skipping tick")
		return
	}
	defer s.syncMu.Unlock()
	syncCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s.fullSync(syncCtx)
}

func (s *Syncer) triggeredTick(ctx context.Context) {
	if !s.syncMu.TryLock() {
		s.logger.Debug("stock sync already running, skipping triggered sync")
		return
	}
	defer s.syncMu.Unlock()
	syncCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s.triggeredSync(syncCtx)
}

func (s *Syncer) fullSync(ctx context.Context) {
	start := time.Now()
	stock, err := s.querier.QueryStock(ctx, s.skus, s.warehouse)
	if err != nil {
		s.mu.Lock()
		s.consecutiveFailures++
		failures := s.consecutiveFailures
		s.mu.Unlock()
		lvl := slog.LevelWarn
		if failures >= 3 {
			lvl = slog.LevelError
		}
		s.logger.Log(ctx, lvl, "stock sync failed",
			"consecutive_failures", failures,
			"error", err,
		)
		return
	}
	if len(stock) == 0 {
		s.logger.Warn("SYSPRO returned no stock data, skipping push")
		return
	}
	s.mu.Lock()
	s.cachedStock = stock
	s.consecutiveFailures = 0
	s.mu.Unlock()
	quantities := s.computeEffective(ctx, stock)
	if err := s.pusher.SetInventoryLevels(ctx, quantities); err != nil {
		s.logger.Error("pushing inventory to Shopify", "error", err)
		return
	}
	s.logger.Info("stock sync complete",
		"skus_updated", len(quantities),
		"skus_skipped", len(s.skus)-len(quantities),
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func (s *Syncer) triggeredSync(ctx context.Context) {
	s.mu.Lock()
	cached := s.cachedStock
	s.mu.Unlock()
	if len(cached) == 0 {
		s.logger.Debug("no cached SYSPRO data, skipping triggered sync")
		return
	}
	quantities := s.computeEffective(ctx, cached)
	if err := s.pusher.SetInventoryLevels(ctx, quantities); err != nil {
		s.logger.Error("triggered sync push failed", "error", err)
		return
	}
	s.logger.Info("triggered stock sync complete", "skus_updated", len(quantities))
}

func (s *Syncer) computeEffective(ctx context.Context, stock map[string]float64) map[string]int {
	reserved, err := s.store.FetchReservedQuantities(ctx)
	if err != nil {
		s.logger.Warn("fetching reserved quantities, using zero", "error", err)
		reserved = map[string]int{}
	}
	quantities := make(map[string]int, len(stock))
	for sku, sysproQty := range stock {
		reservedQty := reserved[sku]
		effective := int(math.Round(sysproQty)) - reservedQty
		if effective < 0 {
			effective = 0
		}
		quantities[sku] = effective
		s.logger.Debug("stock level computed",
			"sku", sku,
			"syspro_qty", sysproQty,
			"reserved_qty", reservedQty,
			"effective_qty", effective,
		)
	}
	return quantities
}
