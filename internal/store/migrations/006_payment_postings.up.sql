CREATE TABLE IF NOT EXISTS payment_postings (
    id                      BIGSERIAL PRIMARY KEY,
    shopify_order_id        BIGINT NOT NULL,
    shopify_transaction_id  BIGINT NOT NULL UNIQUE,
    order_number            TEXT NOT NULL DEFAULT '',
    customer_email          TEXT NOT NULL DEFAULT '',
    gross_amount            NUMERIC(12,2) NOT NULL,
    fee_amount              NUMERIC(12,2) NOT NULL DEFAULT 0,
    net_amount              NUMERIC(12,2) NOT NULL,
    currency                TEXT NOT NULL DEFAULT 'GBP',
    payment_gateway         TEXT NOT NULL DEFAULT '',
    processed_at            TIMESTAMPTZ NOT NULL,

    status                  TEXT NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'posted', 'failed', 'dead_letter')),
    posted_at               TIMESTAMPTZ NULL,
    syspro_receipt_ref      TEXT NOT NULL DEFAULT '',
    last_error              TEXT NOT NULL DEFAULT '',
    attempts                INT NOT NULL DEFAULT 0,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_payment_postings_status_created
    ON payment_postings (status, created_at);

CREATE INDEX IF NOT EXISTS idx_payment_postings_shopify_order
    ON payment_postings (shopify_order_id);
