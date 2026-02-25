CREATE TABLE IF NOT EXISTS webhook_events (
    id          BIGSERIAL PRIMARY KEY,
    webhook_id  TEXT NOT NULL UNIQUE,
    topic       TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS orders (
    id                BIGSERIAL PRIMARY KEY,
    shopify_order_id  BIGINT NOT NULL UNIQUE,
    order_number      TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'processing', 'submitted', 'failed', 'dead_letter', 'cancelled')),
    customer_account  TEXT NOT NULL DEFAULT 'WEBS01',

    ship_first_name   TEXT NOT NULL DEFAULT '',
    ship_last_name    TEXT NOT NULL DEFAULT '',
    ship_address1     TEXT NOT NULL DEFAULT '',
    ship_address2     TEXT NOT NULL DEFAULT '',
    ship_city         TEXT NOT NULL DEFAULT '',
    ship_province     TEXT NOT NULL DEFAULT '',
    ship_postcode     TEXT NOT NULL DEFAULT '',
    ship_country      TEXT NOT NULL DEFAULT '',
    ship_phone        TEXT NOT NULL DEFAULT '',
    ship_email        TEXT NOT NULL DEFAULT '',

    payment_reference TEXT NOT NULL DEFAULT '',
    payment_amount    NUMERIC(12,2) NOT NULL DEFAULT 0,

    raw_payload       JSONB,

    attempts          INT NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT '',

    order_date        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_orders_status ON orders (status);
CREATE INDEX IF NOT EXISTS idx_orders_created_at ON orders (created_at);

CREATE TABLE IF NOT EXISTS order_lines (
    id         BIGSERIAL PRIMARY KEY,
    order_id   BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    sku        TEXT NOT NULL,
    quantity   INT NOT NULL,
    unit_price NUMERIC(12,2) NOT NULL DEFAULT 0,
    discount   NUMERIC(12,2) NOT NULL DEFAULT 0,
    tax        NUMERIC(12,2) NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_order_lines_order_id ON order_lines (order_id);
