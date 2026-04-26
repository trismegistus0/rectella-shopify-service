package inventory

import (
	"context"
	"fmt"
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

// SKULister discovers the authoritative list of SKUs to sync on each cycle.
// Implemented by the Shopify client (productVariants GraphQL pagination),
// because SYSPRO 8 e.net has no stock-code-list business object and Shopify
// is the natural source of truth for "what's sellable". When the lister is
// nil the syncer falls back to the static `skus` slice passed at construction.
type SKULister interface {
	ListAllSKUs(ctx context.Context) ([]string, error)
}

// Syncer orchestrates one-way stock sync from SYSPRO to Shopify.
type Syncer struct {
	querier   InventoryQuerier
	pusher    InventoryPusher
	store     ReservationStore
	lister    SKULister // nil = static mode, use skus slice instead
	interval  time.Duration
	warehouse string
	skus      []string // static fallback when lister is nil OR lister returns empty
	triggerCh <-chan struct{}
	logger    *slog.Logger

	syncMu              sync.Mutex // single-flight guard
	mu                  sync.Mutex // protects cachedStock + consecutiveFailures + orphan dedupe
	cachedStock         map[string]float64
	consecutiveFailures int

	// Orphan-SKU dedupe: only fire ntfy when the orphan set changes,
	// and at most once per hour, so a persistent unmatched SKU
	// doesn't pager-flood the operator at every 15-min cycle.
	ntfyTopic            string
	lastOrphanSet        map[string]struct{}
	lastOrphanNotifiedAt time.Time
}

// NewSyncer constructs a stock syncer. When lister is non-nil the syncer
// operates in dynamic mode: each cycle fetches the current SKU list from
// the lister (Shopify productVariants) and runs INVQRY per SKU. When lister
// is nil it falls back to the static skus slice.
func NewSyncer(
	querier InventoryQuerier,
	pusher InventoryPusher,
	store ReservationStore,
	lister SKULister,
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
		lister:    lister,
		interval:  interval,
		warehouse: warehouse,
		skus:      skus,
		triggerCh: triggerCh,
		logger:    logger,
	}
}

// SetNtfyTopic enables fire-and-forget ntfy events when the stock sync
// finds Shopify SKUs that don't exist in the SYSPRO WEBS warehouse
// (orphan SKUs — typically a new product Clare added on Shopify
// without a matching SYSPRO record). Empty topic = no events (default).
func (s *Syncer) SetNtfyTopic(topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ntfyTopic = topic
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	mode := "static"
	if s.lister != nil {
		mode = "dynamic"
	}
	s.logger.Info("stock sync started",
		"interval", s.interval,
		"mode", mode,
		"static_skus", len(s.skus),
		"warehouse", s.warehouse,
	)

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

// resolveSKUs returns the list of SKUs to sync this cycle. Dynamic mode
// calls the lister (Shopify) and falls back to the static slice if the
// lister fails or returns empty. Static mode always uses the slice.
func (s *Syncer) resolveSKUs(ctx context.Context) []string {
	if s.lister == nil {
		return s.skus
	}
	skus, err := s.lister.ListAllSKUs(ctx)
	if err != nil {
		s.logger.Warn("stock list discovery failed, falling back to static SKU list",
			"error", err,
			"static_count", len(s.skus),
		)
		return s.skus
	}
	if len(skus) == 0 {
		s.logger.Warn("stock list discovery returned no SKUs, falling back to static list",
			"static_count", len(s.skus),
		)
		return s.skus
	}
	s.logger.Info("stock list refreshed", "warehouse", s.warehouse, "count", len(skus))
	return skus
}

func (s *Syncer) fullSync(ctx context.Context) {
	start := time.Now()
	skus := s.resolveSKUs(ctx)
	if len(skus) == 0 {
		s.logger.Warn("stock sync skipped: no SKUs to sync (dynamic discovery empty and no static fallback)")
		return
	}
	stock, err := s.querier.QueryStock(ctx, skus, s.warehouse)
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
		"skus_discovered", len(skus),
		"skus_updated", len(quantities),
		"skus_skipped", len(skus)-len(quantities),
		"duration_ms", time.Since(start).Milliseconds(),
	)

	s.notifyOrphanSKUsIfChanged(skus, stock)
}

// notifyOrphanSKUsIfChanged compares the Shopify-known SKU list against
// the SYSPRO INVQRY response and fires a low-priority ntfy event when
// orphans appear (Shopify-known but SYSPRO-unknown). Dedupes against
// the previously-fired set and rate-limits to at most one push per
// hour so a persistent orphan doesn't pager-flood at 15-min cycles.
//
// Best-effort observability — operator surfaces the gap during the
// post-handoff care window and adds the missing stock code in SYSPRO
// (or removes/draft-flags the product in Shopify).
func (s *Syncer) notifyOrphanSKUsIfChanged(skus []string, stock map[string]float64) {
	if len(skus) == 0 || len(stock) == len(skus) {
		return
	}
	orphans := make([]string, 0)
	orphanSet := make(map[string]struct{})
	for _, sku := range skus {
		if sku == "" {
			continue
		}
		if _, ok := stock[sku]; !ok {
			orphans = append(orphans, sku)
			orphanSet[sku] = struct{}{}
		}
	}
	if len(orphans) == 0 {
		return
	}

	s.mu.Lock()
	topic := s.ntfyTopic
	prev := s.lastOrphanSet
	last := s.lastOrphanNotifiedAt
	changed := !sameStringSet(prev, orphanSet)
	rateLimitElapsed := time.Since(last) > time.Hour
	if changed || rateLimitElapsed {
		s.lastOrphanSet = orphanSet
		s.lastOrphanNotifiedAt = time.Now()
	}
	s.mu.Unlock()

	if !changed && !rateLimitElapsed {
		return
	}

	sample := orphans
	if len(sample) > 5 {
		sample = sample[:5]
	}
	body := "Stock sync found Shopify SKUs with no matching record in SYSPRO " + s.warehouse + ":\n" +
		"  count: " + itoa(len(orphans)) + "\n" +
		"  sample: " + joinComma(sample) + "\n\n" +
		"These will appear as 'Sold out' on the storefront until added to SYSPRO."
	pingNtfyEvent(topic, "Rectella stock sync: orphan SKUs", body)
}

func sameStringSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func itoa(i int) string {
	// avoids importing strconv just for this — keeps the helper self-contained
	return fmt.Sprintf("%d", i)
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
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
