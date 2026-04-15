CREATE TABLE IF NOT EXISTS order_cancellations (
    id                    BIGSERIAL PRIMARY KEY,
    order_id              BIGINT NULL REFERENCES orders(id) ON DELETE SET NULL,
    shopify_order_id      BIGINT NOT NULL,
    shopify_order_number  TEXT NOT NULL DEFAULT '',
    syspro_order_number   TEXT NOT NULL DEFAULT '',
    shopify_cancel_reason TEXT NOT NULL DEFAULT '',
    shopify_cancelled_at  TIMESTAMPTZ NULL,
    syspro_order_status   TEXT NOT NULL DEFAULT '',
    disposition           TEXT NOT NULL,
    webhook_id            TEXT NOT NULL UNIQUE,
    raw_payload           JSONB,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_order_cancellations_disposition
    ON order_cancellations (disposition, created_at);

CREATE INDEX IF NOT EXISTS idx_order_cancellations_shopify_order
    ON order_cancellations (shopify_order_id);
