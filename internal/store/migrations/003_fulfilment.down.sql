DROP INDEX IF EXISTS idx_orders_fulfillment;
ALTER TABLE orders DROP COLUMN IF EXISTS shopify_fulfillment_id;
ALTER TABLE orders DROP COLUMN IF EXISTS fulfilled_at;
