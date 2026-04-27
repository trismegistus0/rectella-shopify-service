package fulfilment

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

// DispatchQuerier queries SYSPRO for dispatch status of sales orders.
type DispatchQuerier interface {
	QueryDispatchedOrders(ctx context.Context, orderNumbers []string) (map[string]syspro.SORQRYResult, error)
}

// FulfilmentPusher creates fulfilments in Shopify.
type FulfilmentPusher interface {
	GetFulfillmentOrderID(ctx context.Context, shopifyOrderID int64) (string, error)
	CreateFulfillment(ctx context.Context, input FulfilmentInput) (string, error)
}

// FulfilmentStore reads and updates order fulfilment state in the database.
type FulfilmentStore interface {
	FetchSubmittedOrders(ctx context.Context) ([]model.Order, error)
	UpdateOrderFulfilled(ctx context.Context, orderID int64, shopifyFulfilmentID string) error
}

// FulfilmentSyncer polls SYSPRO for dispatched orders and creates fulfilments in Shopify.
type FulfilmentSyncer struct {
	querier  DispatchQuerier
	pusher   FulfilmentPusher
	store    FulfilmentStore
	interval time.Duration
	logger   *slog.Logger
	syncMu   sync.Mutex // single-flight guard

	// Shopify-API-error escalation: count consecutive cycles that hit
	// API-level errors (e.g. "Access denied for fulfillmentOrders field"
	// from a missing scope), and rate-limit the ntfy ping so a sustained
	// outage doesn't pager-flood.
	mu                       sync.Mutex
	ntfyTopic                string
	consecutiveFailureCycles int
	lastShopifyErrorMessage  string
	lastNotifiedAt           time.Time
}

// SetNtfyTopic enables fire-and-forget ntfy events when the fulfilment
// write-back to Shopify hits an API-level error (graphql errors,
// network failures, scope revocation). Per-order benign errors like
// "no open fulfillment orders found" do NOT trigger pings — only
// errors that could leave the entire write-back blocked. Empty topic
// = no events (default).
func (s *FulfilmentSyncer) SetNtfyTopic(topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ntfyTopic = topic
}

// NewFulfilmentSyncer creates a new fulfilment syncer.
func NewFulfilmentSyncer(
	querier DispatchQuerier,
	pusher FulfilmentPusher,
	store FulfilmentStore,
	interval time.Duration,
	logger *slog.Logger,
) *FulfilmentSyncer {
	return &FulfilmentSyncer{
		querier:  querier,
		pusher:   pusher,
		store:    store,
		interval: interval,
		logger:   logger,
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (s *FulfilmentSyncer) Run(ctx context.Context) {
	s.logger.Info("fulfilment sync started", "interval", s.interval)

	// First tick at T+0.
	s.tick(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("fulfilment sync stopping")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *FulfilmentSyncer) tick(ctx context.Context) {
	if !s.syncMu.TryLock() {
		s.logger.Debug("fulfilment sync already running, skipping tick")
		return
	}
	defer s.syncMu.Unlock()
	syncCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s.processOrders(syncCtx)
}

func (s *FulfilmentSyncer) processOrders(ctx context.Context) {
	orders, err := s.store.FetchSubmittedOrders(ctx)
	if err != nil {
		s.logger.Error("fetching submitted orders", "error", err)
		return
	}
	if len(orders) == 0 {
		s.logger.Debug("no submitted orders to check")
		return
	}

	// Collect SYSPRO order numbers.
	orderNumbers := make([]string, 0, len(orders))
	for _, o := range orders {
		if o.SysproOrderNumber != "" {
			orderNumbers = append(orderNumbers, o.SysproOrderNumber)
		}
	}
	if len(orderNumbers) == 0 {
		return
	}

	dispatched, err := s.querier.QueryDispatchedOrders(ctx, orderNumbers)
	if err != nil {
		s.logger.Error("querying SYSPRO dispatch status", "error", err)
		return
	}

	fulfilled := 0
	apiErrorCount := 0
	var lastAPIError string
	for _, order := range orders {
		result, ok := dispatched[order.SysproOrderNumber]
		if !ok {
			continue
		}
		if result.OrderStatus != "9" {
			continue
		}

		foID, err := s.pusher.GetFulfillmentOrderID(ctx, order.ShopifyOrderID)
		if err != nil {
			s.logger.Warn("getting fulfillment order ID",
				"order_id", order.ID,
				"shopify_order_id", order.ShopifyOrderID,
				"error", err,
			)
			if isShopifyAPIError(err) {
				apiErrorCount++
				lastAPIError = err.Error()
			}
			continue
		}

		input := FulfilmentInput{
			FulfillmentOrderID: foID,
			TrackingNumber:     result.TrackingNumber,
			Carrier:            result.Carrier,
		}
		fulfilmentID, err := s.pusher.CreateFulfillment(ctx, input)
		if err != nil {
			s.logger.Warn("creating Shopify fulfilment",
				"order_id", order.ID,
				"fulfillment_order_id", foID,
				"error", err,
			)
			if isShopifyAPIError(err) {
				apiErrorCount++
				lastAPIError = err.Error()
			}
			continue
		}

		if err := s.store.UpdateOrderFulfilled(ctx, order.ID, fulfilmentID); err != nil {
			s.logger.Error("updating order fulfilled status",
				"order_id", order.ID,
				"fulfilment_id", fulfilmentID,
				"error", err,
			)
			continue
		}

		s.logger.Info("order fulfilled",
			"order_id", order.ID,
			"syspro_order", order.SysproOrderNumber,
			"fulfilment_id", fulfilmentID,
		)
		fulfilled++
	}

	s.logger.Info("fulfilment sync complete",
		"fulfilled", fulfilled,
		"submitted", len(orders),
	)

	s.handleAPIErrorEscalation(apiErrorCount, lastAPIError, len(orders))
}

// isShopifyAPIError distinguishes systemic Shopify-side failures
// (graphql errors, network failures, parsing failures, missing scope)
// from per-order benign conditions like "no open fulfillment orders
// found for order N", which fires legitimately when an order has
// already been fulfilled in Shopify and shouldn't page anyone.
func isShopifyAPIError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "no open fulfillment orders found") {
		return false
	}
	return true
}

// handleAPIErrorEscalation tracks consecutive cycles that hit Shopify
// API errors and fires a rate-limited ntfy ping when the failure
// persists across multiple cycles. Reset on a clean cycle.
func (s *FulfilmentSyncer) handleAPIErrorEscalation(apiErrorCount int, lastError string, totalOrders int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if apiErrorCount == 0 {
		s.consecutiveFailureCycles = 0
		s.lastShopifyErrorMessage = ""
		return
	}

	s.consecutiveFailureCycles++
	s.lastShopifyErrorMessage = lastError

	// Require >= 2 consecutive failed cycles so a one-off blip doesn't
	// page (a transient 5xx from Shopify resolves itself on the next
	// 30-min tick). Then rate-limit subsequent pings to once per hour.
	if s.consecutiveFailureCycles < 2 {
		return
	}
	if !s.lastNotifiedAt.IsZero() && time.Since(s.lastNotifiedAt) < time.Hour {
		return
	}

	topic := s.ntfyTopic
	if topic == "" {
		return
	}
	s.lastNotifiedAt = time.Now()

	body := fmt.Sprintf(
		"Fulfilment write-back to Shopify is failing.\n"+
			"  affected this cycle: %d / %d\n"+
			"  consecutive bad cycles: %d\n"+
			"  last error: %s\n\n"+
			"Most likely: missing/expired Shopify access scope or token. "+
			"Check /admin/oauth/access_scopes.json and rotate the token if needed.",
		apiErrorCount, totalOrders, s.consecutiveFailureCycles, lastError,
	)
	pingNtfyEvent(topic, "Rectella fulfilment write-back blocked", body)
}
