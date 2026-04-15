package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrDuplicateCancellation is returned when a cancellation webhook with
// the same webhook_id has already been persisted. Idempotency guard so
// Shopify retries don't create multiple rows.
var ErrDuplicateCancellation = errors.New("duplicate cancellation webhook")

// OrderCancellation mirrors a row in the order_cancellations table.
// Populated by the webhook handler + classifier.
type OrderCancellation struct {
	ID                  int64
	OrderID             *int64
	ShopifyOrderID      int64
	ShopifyOrderNumber  string
	SysproOrderNumber   string
	ShopifyCancelReason string
	ShopifyCancelledAt  *time.Time
	SysproOrderStatus   string
	Disposition         string
	WebhookID           string
	RawPayload          []byte
	CreatedAt           time.Time
}

// CreateCancellation inserts a new order_cancellations row. Returns
// ErrDuplicateCancellation on unique-constraint violation of webhook_id
// so the caller can treat the webhook as a benign retry.
func (db *DB) CreateCancellation(ctx context.Context, c OrderCancellation) (int64, error) {
	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO order_cancellations (
			order_id, shopify_order_id, shopify_order_number,
			syspro_order_number, shopify_cancel_reason,
			shopify_cancelled_at, syspro_order_status,
			disposition, webhook_id, raw_payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`,
		c.OrderID, c.ShopifyOrderID, c.ShopifyOrderNumber,
		c.SysproOrderNumber, c.ShopifyCancelReason,
		c.ShopifyCancelledAt, c.SysproOrderStatus,
		c.Disposition, c.WebhookID, c.RawPayload,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, ErrDuplicateCancellation
		}
		return 0, fmt.Errorf("inserting order cancellation: %w", err)
	}
	return id, nil
}

// CancellationExists checks whether a cancellation webhook has already
// been persisted. Cheap existence check for the handler's idempotency
// guard before it does any SORQRY work.
func (db *DB) CancellationExists(ctx context.Context, webhookID string) (bool, error) {
	var exists bool
	err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM order_cancellations WHERE webhook_id = $1)`,
		webhookID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking cancellation existence: %w", err)
	}
	return exists, nil
}
