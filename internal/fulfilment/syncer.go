package fulfilment

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"codeberg.org/speeder091/rectella-shopify-service/internal/model"
	"codeberg.org/speeder091/rectella-shopify-service/internal/syspro"
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
}
