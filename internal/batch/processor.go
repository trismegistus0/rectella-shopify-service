package batch

import (
	"context"
	"errors"
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
	MarkOrderProcessing(ctx context.Context, orderID int64) (bool, error)
	UpdateOrderStatus(ctx context.Context, orderID int64, status model.OrderStatus, attempts int, lastError string) error
	UpdateOrderSubmitted(ctx context.Context, orderID int64, sysproOrderNumber string, attempts int) error
}

// Processor polls for pending orders and submits them to SYSPRO.
type Processor struct {
	store    Store
	client   syspro.Client
	interval time.Duration
	logger   *slog.Logger

	mu sync.Mutex
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

	// Per-batch timeout prevents a hung SYSPRO from blocking all future batches.
	batchCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := p.ProcessBatch(batchCtx); err != nil {
		p.logger.Error("batch processing error", "error", err)
	}
}

// ProcessBatch runs a single batch cycle: fetch pending orders, open a SYSPRO
// session, submit each order, update statuses.
func (p *Processor) ProcessBatch(ctx context.Context) error {
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
	defer func() {
		// Use a fresh context for logoff — the batch context may be cancelled
		// during shutdown, but we still want SYSPRO to release the session.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		session.Close(closeCtx) //nolint:errcheck // best-effort cleanup
	}()

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

	// Mark as processing BEFORE calling SYSPRO. This prevents duplicate
	// submissions if the service crashes after SYSPRO accepts but before
	// we update the status. Orders stuck in 'processing' after a crash
	// are identifiable and can be investigated.
	ok, err := p.store.MarkOrderProcessing(ctx, order.ID)
	if err != nil {
		p.logger.Error("marking order processing", "order_id", order.ID, "error", err)
		return errInfra
	}
	if !ok {
		// Order is no longer pending — skip it (already picked up or cancelled).
		p.logger.Debug("order no longer pending, skipping", "order_id", order.ID)
		return nil
	}

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
			"shopify_order_id", order.ShopifyOrderID,
			"order_number", order.OrderNumber,
			"attempts", newAttempts,
			"error", err,
		)

		return errInfra
	}

	if !result.Success {
		// Business error — mark failed, continue batch. Don't increment attempts
		// (attempts tracks infra retries only).
		if uerr := p.store.UpdateOrderStatus(ctx, order.ID, model.OrderStatusFailed, order.Attempts, result.ErrorMessage); uerr != nil {
			p.logger.Error("updating order after business error",
				"order_id", order.ID,
				"error", uerr,
			)
		}

		p.logger.Warn("SYSPRO rejected order",
			"order_id", order.ID,
			"shopify_order_id", order.ShopifyOrderID,
			"order_number", order.OrderNumber,
			"error", result.ErrorMessage,
		)

		return nil
	}

	// Success — store the SYSPRO order number.
	if uerr := p.store.UpdateOrderSubmitted(ctx, order.ID, result.SysproOrderNumber, order.Attempts+1); uerr != nil {
		p.logger.Error("updating order after success",
			"order_id", order.ID,
			"error", uerr,
		)
	}

	p.logger.Info("order submitted to SYSPRO",
		"order_id", order.ID,
		"shopify_order_id", order.ShopifyOrderID,
		"order_number", order.OrderNumber,
		"syspro_order", result.SysproOrderNumber,
	)

	return nil
}
