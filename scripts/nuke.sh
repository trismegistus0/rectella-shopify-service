#!/usr/bin/env bash

# Full reset: destroy PostgreSQL volume and recreate from scratch.
# Use this when migrations change or data is corrupted.

cd "$(dirname "$0")/.." || exit 1

echo "Destroying PostgreSQL volume..."
docker compose down -v

echo "Starting fresh..."
docker compose up -d

echo "Waiting for PostgreSQL..."
for i in $(seq 1 30); do
  if docker exec rectella-shopify-service-postgres-1 pg_isready -U rectella -q 2>/dev/null; then
    echo "PostgreSQL ready."
    exit 0
  fi
  sleep 1
done

echo "PostgreSQL did not become ready in time."
exit 1
