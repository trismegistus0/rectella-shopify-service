package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrDuplicatePayment is returned when a payment with the same
// shopify_transaction_id already exists. Callers should treat this as a
// benign idempotency hit, not a failure.
var ErrDuplicatePayment = errors.New("duplicate shopify transaction")

// PaymentPosting mirrors a row in the payment_postings table. The schema
// is designed for the ARSPAY cash-receipt flow: gross/fee/net amounts
// line up with SYSPRO's Amount + BankCharges fields (fee = gross − net).
type PaymentPosting struct {
	ID                   int64
	ShopifyOrderID       int64
	ShopifyTransactionID int64
	OrderNumber          string
	CustomerEmail        string
	GrossAmount          float64
	FeeAmount            float64
	NetAmount            float64
	Currency             string
	PaymentGateway       string
	ProcessedAt          time.Time

	Status           string
	PostedAt         *time.Time
	SysproReceiptRef string
	LastError        string
	Attempts         int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreatePayment inserts a new payment_postings row. Duplicate
// shopify_transaction_id returns ErrDuplicatePayment so the syncer can
// skip it without logging an error.
func (db *DB) CreatePayment(ctx context.Context, p PaymentPosting) (int64, error) {
	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO payment_postings (
			shopify_order_id, shopify_transaction_id, order_number,
			customer_email, gross_amount, fee_amount, net_amount,
			currency, payment_gateway, processed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`,
		p.ShopifyOrderID, p.ShopifyTransactionID, p.OrderNumber,
		p.CustomerEmail, p.GrossAmount, p.FeeAmount, p.NetAmount,
		p.Currency, p.PaymentGateway, p.ProcessedAt,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, ErrDuplicatePayment
		}
		return 0, fmt.Errorf("inserting payment posting: %w", err)
	}
	return id, nil
}

// FetchUnpostedPayments returns payments in `pending` status, oldest
// first, up to `limit`. Used by the payments syncer's polling loop.
func (db *DB) FetchUnpostedPayments(ctx context.Context, limit int) ([]PaymentPosting, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, shopify_order_id, shopify_transaction_id, order_number,
		       customer_email, gross_amount, fee_amount, net_amount,
		       currency, payment_gateway, processed_at,
		       status, posted_at, syspro_receipt_ref, last_error, attempts,
		       created_at, updated_at
		FROM payment_postings
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("querying unposted payments: %w", err)
	}
	defer rows.Close()

	var out []PaymentPosting
	for rows.Next() {
		var p PaymentPosting
		if err := rows.Scan(
			&p.ID, &p.ShopifyOrderID, &p.ShopifyTransactionID, &p.OrderNumber,
			&p.CustomerEmail, &p.GrossAmount, &p.FeeAmount, &p.NetAmount,
			&p.Currency, &p.PaymentGateway, &p.ProcessedAt,
			&p.Status, &p.PostedAt, &p.SysproReceiptRef, &p.LastError, &p.Attempts,
			&p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning payment row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating payment rows: %w", err)
	}
	return out, nil
}

// MarkPaymentPosted flips a payment to `posted` and records the SYSPRO
// receipt reference returned by ARSPAY. Called after a successful post.
func (db *DB) MarkPaymentPosted(ctx context.Context, id int64, sysproRef string) error {
	ct, err := db.Pool.Exec(ctx, `
		UPDATE payment_postings
		SET status = 'posted',
		    posted_at = NOW(),
		    syspro_receipt_ref = $2,
		    last_error = '',
		    updated_at = NOW()
		WHERE id = $1
	`, id, sysproRef)
	if err != nil {
		return fmt.Errorf("marking payment posted: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("payment %d not found", id)
	}
	return nil
}

// MarkPaymentFailed records an error and increments the attempt counter.
// After 3 attempts the row is moved to dead_letter.
func (db *DB) MarkPaymentFailed(ctx context.Context, id int64, errMsg string) error {
	const maxAttempts = 3
	ct, err := db.Pool.Exec(ctx, `
		UPDATE payment_postings
		SET status = CASE WHEN attempts + 1 >= $3 THEN 'dead_letter' ELSE 'failed' END,
		    attempts = attempts + 1,
		    last_error = $2,
		    updated_at = NOW()
		WHERE id = $1
	`, id, errMsg, maxAttempts)
	if err != nil {
		return fmt.Errorf("marking payment failed: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("payment %d not found", id)
	}
	return nil
}

// RequeuePayment flips a failed payment back to pending so the syncer
// will retry it. Admin-triggered equivalent of RetryOrder.
func (db *DB) RequeuePayment(ctx context.Context, id int64) error {
	ct, err := db.Pool.Exec(ctx, `
		UPDATE payment_postings
		SET status = 'pending',
		    attempts = 0,
		    last_error = '',
		    updated_at = NOW()
		WHERE id = $1 AND status IN ('failed', 'dead_letter')
	`, id)
	if err != nil {
		return fmt.Errorf("requeuing payment: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("payment %d not in failed state", id)
	}
	return nil
}

