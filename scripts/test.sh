#!/usr/bin/env bash

BASE_URL="${BASE_URL:-http://localhost:8080}"
SECRET="${SHOPIFY_WEBHOOK_SECRET:-local-test-secret}"

# Run history — each run gets a timestamped file.
HISTORY_DIR="scripts/run-history"
mkdir -p "$HISTORY_DIR"
RUN_FILE="$HISTORY_DIR/$(date +%Y%m%d-%H%M%S).log"

# Tee all output to both terminal and the run file.
exec > >(tee "$RUN_FILE") 2>&1

pass=0
fail=0

check() {
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    echo "  PASS  $name (HTTP $actual)"
    pass=$((pass + 1))
  else
    echo "  FAIL  $name — expected $expected, got $actual"
    fail=$((fail + 1))
  fi
}

sign() {
  echo -n "$1" | openssl dgst -sha256 -hmac "$SECRET" -binary | base64
}

send() {
  local webhook_id="$1" body="$2" hmac="${3:-}"
  local args=(-s -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/webhooks/orders/create")
  args+=(-H "Content-Type: application/json")
  if [ -n "$webhook_id" ]; then
    args+=(-H "X-Shopify-Webhook-Id: $webhook_id")
  fi
  if [ -n "$hmac" ]; then
    args+=(-H "X-Shopify-Hmac-Sha256: $hmac")
  fi
  args+=(-d "$body")
  curl "${args[@]}"
}

db() {
  docker exec rectella-shopify-service-postgres-1 psql -U rectella -d rectella -c "$1" 2>/dev/null \
    || psql -h localhost -U rectella -d rectella -c "$1" 2>/dev/null \
    || echo "(could not query database)"
}

PAYLOAD=$(cat internal/webhook/testdata/order_create.json)

echo ""
echo "=== Webhook Handler Integration Tests ==="
echo "Run: $(date '+%Y-%m-%d %H:%M:%S')"
echo "Target: $BASE_URL"
echo ""

# 1. Valid order
echo "--- Happy path ---"
hmac=$(sign "$PAYLOAD")
status=$(send "test-wh-001" "$PAYLOAD" "$hmac")
check "Valid order creates successfully" 200 "$status"

# 2. Duplicate webhook (same ID again)
status=$(send "test-wh-001" "$PAYLOAD" "$hmac")
check "Duplicate webhook returns 200 (idempotent)" 200 "$status"

# 3. Invalid HMAC signature
status=$(send "test-wh-002" "$PAYLOAD" "aW52YWxpZHNpZw==")
check "Invalid HMAC rejected" 401 "$status"

# 4. Missing HMAC header entirely
status=$(send "test-wh-003" "$PAYLOAD" "")
check "Missing HMAC rejected" 401 "$status"

# 5. Missing webhook ID header
hmac=$(sign "$PAYLOAD")
status=$(send "" "$PAYLOAD" "$hmac")
check "Missing webhook ID rejected" 400 "$status"

# 6. Malformed JSON
bad_json='{not valid json at all'
hmac=$(sign "$bad_json")
status=$(send "test-wh-004" "$bad_json" "$hmac")
check "Malformed JSON rejected" 400 "$status"

# 7. Empty line items
no_lines='{"id":999,"name":"#BBQ9999","line_items":[]}'
hmac=$(sign "$no_lines")
status=$(send "test-wh-005" "$no_lines" "$hmac")
check "Empty line_items rejected" 422 "$status"

# 8. Zero order ID
zero_id='{"id":0,"name":"#BBQ0000","line_items":[{"sku":"X","quantity":1,"price":"10.00"}]}'
hmac=$(sign "$zero_id")
status=$(send "test-wh-006" "$zero_id" "$hmac")
check "Zero order ID rejected" 422 "$status"

# 9. Second valid order (different Shopify order ID + webhook ID)
second_order='{"id":5559999000001,"name":"#BBQ1002","email":"jane@example.com","created_at":"2026-02-25T15:00:00Z","total_price":"149.00","gateway":"shopify_payments","shipping_address":{"first_name":"Jane","last_name":"Doe","address1":"10 High Street","address2":"","city":"Manchester","province":"Greater Manchester","zip":"M1 1AA","country":"United Kingdom","phone":"+441234567890"},"line_items":[{"sku":"MBBQ0159","quantity":1,"price":"149.00","total_discount":"0.00","tax_lines":[{"price":"29.80","rate":0.2,"title":"VAT"}]}]}'
status=$(send "test-wh-007" "$second_order" "$(sign "$second_order")")
check "Second order with new webhook ID succeeds" 200 "$status"

# 10. Nil shipping address
no_addr='{"id":5559876,"name":"#BBQ2000","email":"test@example.com","created_at":"2026-02-25T10:00:00Z","total_price":"149.00","gateway":"shopify_payments","shipping_address":null,"line_items":[{"sku":"MBBQ0159","quantity":1,"price":"149.00","total_discount":"0.00","tax_lines":[{"price":"29.80","rate":0.2,"title":"VAT"}]}]}'
hmac=$(sign "$no_addr")
status=$(send "test-wh-008" "$no_addr" "$hmac")
check "Nil shipping address accepted" 200 "$status"

echo ""
echo "=== Database verification ==="
echo ""
echo "Orders stored:"
db "SELECT id, order_number, status, customer_account, ship_city, payment_amount FROM orders ORDER BY id;"
echo ""
echo "Order lines:"
db "SELECT ol.id, ol.order_id, ol.sku, ol.quantity, ol.unit_price, ol.tax FROM order_lines ol ORDER BY ol.id;"
echo ""
echo "Webhook events:"
db "SELECT webhook_id, topic, received_at FROM webhook_events ORDER BY id;"

echo ""
echo "=== Results: $pass passed, $fail failed ==="
echo "Log saved to: $RUN_FILE"
[ "$fail" -eq 0 ] && exit 0 || exit 1
