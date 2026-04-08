ALTER TABLE orders ADD COLUMN IF NOT EXISTS fulfilled_at TIMESTAMPTZ;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS shopify_fulfillment_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_orders_fulfillment ON orders (status, fulfilled_at) WHERE status = 'submitted';
