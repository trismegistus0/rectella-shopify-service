#!/usr/bin/env bash

# Start PostgreSQL and run the service. Handles env loading and DB readiness.

cd "$(dirname "$0")/.." || exit 1

# Ensure PostgreSQL is running.
if ! docker compose ps --format '{{.State}}' 2>/dev/null | grep -q running; then
  echo "Starting PostgreSQL..."
  docker compose up -d
fi

# Wait for PostgreSQL to accept connections.
echo "Waiting for PostgreSQL..."
for i in $(seq 1 30); do
  if docker exec rectella-shopify-service-postgres-1 pg_isready -U rectella -q 2>/dev/null; then
    break
  fi
  sleep 1
done

# Load env vars.
if [ -f .env ]; then
  set -a
  . ./.env
  set +a
else
  echo "No .env file found — copy .env.example to .env first"
  exit 1
fi

echo "Starting service..."
exec go run ./cmd/server
