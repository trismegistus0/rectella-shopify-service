package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
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
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

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
			payment_reference, payment_amount, shipping_amount,
			raw_payload, order_date
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14,
			$15, $16, $17,
			$18, $19
		) RETURNING id`,
		order.ShopifyOrderID, order.OrderNumber, order.Status, order.CustomerAccount,
		order.ShipFirstName, order.ShipLastName, order.ShipAddress1, order.ShipAddress2,
		order.ShipCity, order.ShipProvince, order.ShipPostcode, order.ShipCountry,
		order.ShipPhone, order.ShipEmail,
		order.PaymentReference, order.PaymentAmount, order.ShippingAmount,
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
				_ = br.Close()
				return fmt.Errorf("inserting order line: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("closing batch insert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// MarkOrderProcessing atomically transitions an order from 'pending' to 'processing'.
// Returns false if the order is no longer pending (already picked up by another batch).
func (db *DB) MarkOrderProcessing(ctx context.Context, orderID int64) (bool, error) {
	tag, err := db.Pool.Exec(ctx,
		`UPDATE orders SET status = 'processing', updated_at = NOW()
		WHERE id = $1 AND status = 'pending'`,
		orderID,
	)
	if err != nil {
		return false, fmt.Errorf("marking order %d processing: %w", orderID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateOrderSubmitted records a successful SYSPRO submission with the order number.
func (db *DB) UpdateOrderSubmitted(ctx context.Context, orderID int64, sysproOrderNumber string, attempts int) error {
	tag, err := db.Pool.Exec(ctx,
		`UPDATE orders
		SET status = $2, syspro_order_number = $3, attempts = $4, last_error = '', updated_at = NOW()
		WHERE id = $1`,
		orderID, string(model.OrderStatusSubmitted), sysproOrderNumber, attempts,
	)
	if err != nil {
		return fmt.Errorf("updating order %d submitted: %w", orderID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("order %d not found", orderID)
	}
	return nil
}

// RetryOrder moves a failed or dead-lettered order back to pending, resetting attempts.
func (db *DB) RetryOrder(ctx context.Context, orderID int64) error {
	tag, err := db.Pool.Exec(ctx,
		`UPDATE orders SET status = 'pending', attempts = 0, last_error = '', updated_at = NOW()
		WHERE id = $1 AND status IN ('failed', 'dead_letter')`,
		orderID,
	)
	if err != nil {
		return fmt.Errorf("retrying order %d: %w", orderID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("order %d not found or not in retryable status", orderID)
	}
	return nil
}

// FetchPendingOrders returns up to limit orders with status 'pending', oldest first,
// along with their line items.
func (db *DB) FetchPendingOrders(ctx context.Context, limit int) ([]model.OrderWithLines, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, shopify_order_id, order_number, status, customer_account,
			ship_first_name, ship_last_name, ship_address1, ship_address2,
			ship_city, ship_province, ship_postcode, ship_country,
			ship_phone, ship_email,
			payment_reference, payment_amount, shipping_amount,
			raw_payload, syspro_order_number, attempts, last_error,
			order_date, created_at, updated_at,
			fulfilled_at, shopify_fulfillment_id
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
			&o.PaymentReference, &o.PaymentAmount, &o.ShippingAmount,
			&o.RawPayload, &o.SysproOrderNumber, &o.Attempts, &o.LastError,
			&o.OrderDate, &o.CreatedAt, &o.UpdatedAt,
			&o.FulfilledAt, &o.ShopifyFulfilmentID,
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

// UpdateOrderStatus sets the status, attempts count, and last error for an order.
func (db *DB) UpdateOrderStatus(ctx context.Context, orderID int64, status model.OrderStatus, attempts int, lastError string) error {
	tag, err := db.Pool.Exec(ctx,
		`UPDATE orders
		SET status = $2, attempts = $3, last_error = $4, updated_at = NOW()
		WHERE id = $1`,
		orderID, string(status), attempts, lastError,
	)
	if err != nil {
		return fmt.Errorf("updating order %d status: %w", orderID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("order %d not found", orderID)
	}
	return nil
}

// ListOrdersByStatus returns orders matching the given status, newest first.
func (db *DB) ListOrdersByStatus(ctx context.Context, status model.OrderStatus) ([]model.Order, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, shopify_order_id, order_number, status, customer_account,
			ship_first_name, ship_last_name, ship_address1, ship_address2,
			ship_city, ship_province, ship_postcode, ship_country,
			ship_phone, ship_email,
			payment_reference, payment_amount, shipping_amount,
			raw_payload, syspro_order_number, attempts, last_error,
			order_date, created_at, updated_at,
			fulfilled_at, shopify_fulfillment_id
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
			&o.PaymentReference, &o.PaymentAmount, &o.ShippingAmount,
			&o.RawPayload, &o.SysproOrderNumber, &o.Attempts, &o.LastError,
			&o.OrderDate, &o.CreatedAt, &o.UpdatedAt,
			&o.FulfilledAt, &o.ShopifyFulfilmentID,
		); err != nil {
			return nil, fmt.Errorf("scanning order: %w", err)
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// FetchSubmittedOrders returns orders that have been submitted to SYSPRO
// but not yet fulfilled in Shopify, ordered oldest first.
func (db *DB) FetchSubmittedOrders(ctx context.Context) ([]model.Order, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, shopify_order_id, order_number, status, customer_account,
			ship_first_name, ship_last_name, ship_address1, ship_address2,
			ship_city, ship_province, ship_postcode, ship_country,
			ship_phone, ship_email,
			payment_reference, payment_amount, shipping_amount,
			raw_payload, syspro_order_number, attempts, last_error,
			order_date, created_at, updated_at,
			fulfilled_at, shopify_fulfillment_id
		FROM orders
		WHERE status = 'submitted'
		  AND fulfilled_at IS NULL
		  AND syspro_order_number != ''
		ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying submitted orders: %w", err)
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
			&o.PaymentReference, &o.PaymentAmount, &o.ShippingAmount,
			&o.RawPayload, &o.SysproOrderNumber, &o.Attempts, &o.LastError,
			&o.OrderDate, &o.CreatedAt, &o.UpdatedAt,
			&o.FulfilledAt, &o.ShopifyFulfilmentID,
		); err != nil {
			return nil, fmt.Errorf("scanning submitted order: %w", err)
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// UpdateOrderFulfilled marks an order as fulfilled with the Shopify fulfilment GID.
func (db *DB) UpdateOrderFulfilled(ctx context.Context, orderID int64, shopifyFulfilmentID string) error {
	tag, err := db.Pool.Exec(ctx,
		`UPDATE orders SET status = 'fulfilled', fulfilled_at = NOW(), shopify_fulfillment_id = $2, updated_at = NOW()
		WHERE id = $1`,
		orderID, shopifyFulfilmentID,
	)
	if err != nil {
		return fmt.Errorf("updating order %d fulfilled: %w", orderID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("order %d not found", orderID)
	}
	return nil
}

// FetchReservedQuantities returns the total quantity of each SKU in
// pending or processing orders. These orders have NOT been submitted to
// SYSPRO yet, so their quantities must be subtracted from SYSPRO's
// QtyAvailable to avoid overselling.
func (db *DB) FetchReservedQuantities(ctx context.Context) (map[string]int, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT ol.sku, COALESCE(SUM(ol.quantity), 0)::int AS reserved
		FROM order_lines ol
		JOIN orders o ON o.id = ol.order_id
		WHERE o.status IN ('pending', 'processing')
		GROUP BY ol.sku`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying reserved quantities: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var sku string
		var qty int
		if err := rows.Scan(&sku, &qty); err != nil {
			return nil, fmt.Errorf("scanning reserved quantity: %w", err)
		}
		result[sku] = qty
	}
	return result, rows.Err()
}
