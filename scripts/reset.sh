#!/usr/bin/env bash

# Reset the database: truncate all tables, keeping the schema intact.

cd "$(dirname "$0")/.." || exit 1

echo "Truncating orders, order_lines, webhook_events..."
docker exec rectella-shopify-service-postgres-1 psql -U rectella -d rectella \
  -c "TRUNCATE orders, order_lines, webhook_events CASCADE;" 2>/dev/null \
  || psql -h localhost -U rectella -d rectella \
  -c "TRUNCATE orders, order_lines, webhook_events CASCADE;" 2>/dev/null

echo "Done."
