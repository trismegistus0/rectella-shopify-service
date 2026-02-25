package store

import (
	"context"
	"errors"
	"fmt"

	"codeberg.org/speeder091/rectella-shopify-service/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrDuplicateWebhook is returned when a webhook event has already been recorded.
var ErrDuplicateWebhook = errors.New("duplicate webhook")

// WebhookExists checks whether a webhook event with the given ID has already been stored.
func (db *DB) WebhookExists(ctx context.Context, webhookID string) (bool, error) {
	var exists bool
	err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM webhook_events WHERE webhook_id = $1)`,
		webhookID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking webhook existence: %w", err)
	}
	return exists, nil
}

// CreateOrder persists a webhook event, order, and its line items in a single transaction.
// Returns ErrDuplicateWebhook if the webhook_id already exists (unique constraint violation).
func (db *DB) CreateOrder(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Insert webhook event.
	_, err = tx.Exec(ctx,
		`INSERT INTO webhook_events (webhook_id, topic) VALUES ($1, $2)`,
		event.WebhookID, event.Topic,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicateWebhook
		}
		return fmt.Errorf("inserting webhook event: %w", err)
	}

	// Insert order.
	var orderID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO orders (
			shopify_order_id, order_number, status, customer_account,
			ship_first_name, ship_last_name, ship_address1, ship_address2,
			ship_city, ship_province, ship_postcode, ship_country,
			ship_phone, ship_email,
			payment_reference, payment_amount,
			raw_payload, order_date
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14,
			$15, $16,
			$17, $18
		) RETURNING id`,
		order.ShopifyOrderID, order.OrderNumber, order.Status, order.CustomerAccount,
		order.ShipFirstName, order.ShipLastName, order.ShipAddress1, order.ShipAddress2,
		order.ShipCity, order.ShipProvince, order.ShipPostcode, order.ShipCountry,
		order.ShipPhone, order.ShipEmail,
		order.PaymentReference, order.PaymentAmount,
		order.RawPayload, order.OrderDate,
	).Scan(&orderID)
	if err != nil {
		return fmt.Errorf("inserting order: %w", err)
	}

	// Insert order lines.
	if len(lines) > 0 {
		batch := &pgx.Batch{}
		for _, l := range lines {
			batch.Queue(
				`INSERT INTO order_lines (order_id, sku, quantity, unit_price, discount, tax)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				orderID, l.SKU, l.Quantity, l.UnitPrice, l.Discount, l.Tax,
			)
		}
		br := tx.SendBatch(ctx, batch)
		for range lines {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return fmt.Errorf("inserting order line: %w", err)
			}
		}
		br.Close()
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}
