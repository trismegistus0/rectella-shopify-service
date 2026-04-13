#!/bin/bash

# Self-contained pipeline test. Starts Postgres, service (with mocks), runs tests, cleans up.
# Usage: ./scripts/pipeline.sh [pipeline-test flags]

set -e
cd "$(dirname "$0")/.."

# Ensure PostgreSQL is running.
if ! docker compose ps --format '{{.State}}' 2>/dev/null | grep -q running; then
  echo "Starting PostgreSQL..."
  docker compose up -d
fi

echo "Waiting for PostgreSQL..."
for i in $(seq 1 30); do
  if docker exec rectella-shopify-service-postgres-1 pg_isready -U rectella -q 2>/dev/null; then
    break
  fi
  sleep 1
done

# Load base env vars.
if [ -f .env ]; then
  set -a
  . ./.env
  set +a
else
  echo "No .env file found — copy .env.example to .env first"
  exit 1
fi

# Override for mock targets.
export SYSPRO_ENET_URL="http://localhost:19100/SYSPROWCFService/Rest"
export SHOPIFY_BASE_URL="http://localhost:19200/admin/api/2025-04/graphql.json"
export SHOPIFY_ACCESS_TOKEN="${SHOPIFY_ACCESS_TOKEN:-mock-token}"
export SYSPRO_WAREHOUSE="${SYSPRO_WAREHOUSE:-WH01}"
export SYSPRO_SKUS="${SYSPRO_SKUS:-CBBQ0001,MBBQ0159}"
export SHOPIFY_LOCATION_ID=""

# Fast intervals for testing.
export BATCH_INTERVAL=3s
export STOCK_SYNC_INTERVAL=5s
export FULFILMENT_SYNC_INTERVAL=5s

echo "Starting service (mock mode)..."
go run ./cmd/server &
SERVICE_PID=$!

# Cleanup on exit.
cleanup() {
  if kill -0 $SERVICE_PID 2>/dev/null; then
    kill $SERVICE_PID
    wait $SERVICE_PID 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Wait for health check.
echo "Waiting for service health..."
for i in $(seq 1 30); do
  if curl -sf "http://localhost:${PORT:-8080}/health" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 $SERVICE_PID 2>/dev/null; then
    echo "Service died during startup"
    exit 1
  fi
  sleep 1
done

echo "Running pipeline test..."
go run ./cmd/pipeline-test "$@"
EXIT_CODE=$?

exit $EXIT_CODE
