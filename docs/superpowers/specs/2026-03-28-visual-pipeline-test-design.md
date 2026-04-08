# Visual Pipeline Test — Design Spec

Date: 2026-03-28
Status: Draft

## Summary

A standalone Go binary (`cmd/pipeline-test/main.go`) that sends synthetic Shopify webhooks into the running middleware and watches orders flow through every stage of the pipeline in real time. Terminal output shows each order progressing through stages with clear visual markers, durations, and pass/fail results. Think of it as a flight-check for the full system — you run it, watch orders move, and know the pipeline works.

Two modes: **mock** (default, no VPN needed, uses a local fake SYSPRO server) and **live** (hits real SYSPRO via VPN, for pre-go-live confidence).

## Why This Exists

The integration tests validate correctness but run silently inside `go test`. This tool is for the developer sitting at the terminal who wants to *see* orders flow — webhook receipt, HMAC verification, DB persistence, batch pickup, SYSPRO session, SORTOI submission, order number assignment, final status. It is a confidence/demo tool, not a production feature.

## What It Exercises

The full Phase 1 order pipeline end-to-end:

```
Shopify webhook (simulated)
  -> HMAC-SHA256 verification
  -> Idempotency check (webhook_id dedup)
  -> Payload validation
  -> DB persist (webhook_events + orders + order_lines)
  -> Batch processor pickup (pending -> processing)
  -> SYSPRO session open (logon)
  -> SORTOI XML build + submit
  -> Response parse (order number extraction)
  -> DB status update (processing -> submitted)
  -> SYSPRO session close (logoff)
```

## How It Works

### Architecture

The tool does NOT start its own HTTP server or database. It connects to the **already-running service** (started via `./scripts/run.sh`) and its database. This keeps the test realistic — it exercises the actual running code, not a test harness.

```
pipeline-test binary
  |
  |-- 1. Connects to PostgreSQL (same DATABASE_URL)
  |-- 2. Sends POST /webhooks/orders/create (with valid HMAC)
  |-- 3. Polls DB for status changes (orders table)
  |-- 4. Prints stage transitions as they happen
  |-- 5. Validates expectations at each stage
  |-- 6. Prints final summary
```

### Flow Per Order

1. **Generate** a synthetic Shopify order payload with unique `id`, `name` (e.g. `#PIPE-001`), and realistic line items using known SKUs.
2. **Sign** the payload with the configured `SHOPIFY_WEBHOOK_SECRET` (HMAC-SHA256).
3. **POST** to `http://localhost:{PORT}/webhooks/orders/create` with correct headers (`X-Shopify-Webhook-Id`, `X-Shopify-Hmac-Sha256`, `Content-Type`).
4. **Verify** HTTP 200 response.
5. **Poll** the `orders` table (via direct DB connection, not the API) every 250ms, watching for status transitions: `pending` -> `processing` -> `submitted` (or `failed`/`dead_letter`).
6. **Print** each transition as it happens, with elapsed time since webhook send.
7. **Validate** the final state: status is `submitted`, `syspro_order_number` is populated, `customer_account` is `WEBS01`.

### Test Scenarios

The tool sends a batch of orders that exercise different paths:

| # | Order Name | Scenario | Expected Final Status |
|---|-----------|----------|----------------------|
| 1 | `#PIPE-001` | Happy path — single line, stocked SKU | `submitted` |
| 2 | `#PIPE-002` | Multi-line order — two stocked SKUs | `submitted` |
| 3 | `#PIPE-003` | Duplicate webhook — same webhook ID as #1 | HTTP 200 (idempotent), no new DB row |
| 4 | `#PIPE-004` | Invalid HMAC — bad signature | HTTP 401, no DB row |
| 5 | `#PIPE-005` | Missing SKU — empty SKU on a line item | HTTP 422, no DB row |

In **mock mode**, the fake SYSPRO always returns success with a generated order number. In **live mode**, all orders hit real SYSPRO (company `RILT`), so only scenarios 1-2 proceed to SORTOI — the others are rejected at the webhook handler before reaching the batch processor.

### Mock SYSPRO Server

In mock mode, the tool starts a local HTTP server that mimics the e.net REST API on a high port (default 19100):

- `GET /Logon` — returns a fake GUID (`"mock-session-001"`)
- `GET /Transaction/Post` — parses the SORTOI XML, extracts the customer PO number, returns a success response with a generated SO number (`SO-MOCK-001`, etc.)
- `GET /Query/Query` — returns a minimal INVQRY response (not exercised by the pipeline test, but present for completeness)
- `GET /Logoff` — returns success

The service must be started with `SYSPRO_ENET_URL` pointing at the mock server. A wrapper script handles this.

## Console Output Format

The output is designed to be read top-to-bottom as orders flow through. Each stage transition prints on its own line with a visual marker, timestamp delta, and context.

### Example Output (Mock Mode)

```
=== Rectella Pipeline Test ===
Mode:     mock (local fake SYSPRO on :19100)
Target:   http://localhost:8080
Database: connected (3 tables, 0 existing orders)
──────────────────────────────────────────────────────

[1/5] #PIPE-001  Single line, happy path
      SEND   webhook -> HTTP 200                          +0ms
      STAGE  pending                                     +12ms
      STAGE  pending -> processing                      +5.3s  (batch pickup)
      STAGE  processing -> submitted  SO-MOCK-001       +5.8s  (SYSPRO accepted)
      PASS

[2/5] #PIPE-002  Multi-line order
      SEND   webhook -> HTTP 200                          +0ms
      STAGE  pending                                     +14ms
      STAGE  pending -> processing                      +5.2s
      STAGE  processing -> submitted  SO-MOCK-002       +5.6s
      PASS

[3/5] #PIPE-003  Duplicate webhook (same ID as #1)
      SEND   webhook -> HTTP 200                          +0ms
      CHECK  no new DB row (idempotent)                   OK
      PASS

[4/5] #PIPE-004  Invalid HMAC
      SEND   webhook -> HTTP 401                          +0ms
      CHECK  no DB row                                    OK
      PASS

[5/5] #PIPE-005  Missing SKU
      SEND   webhook -> HTTP 422                          +0ms
      CHECK  no DB row                                    OK
      PASS

──────────────────────────────────────────────────────
RESULTS  5 passed, 0 failed                     total 11.4s

Order Summary:
  #PIPE-001  submitted  SO-MOCK-001  1 line   (CBBQ0001)
  #PIPE-002  submitted  SO-MOCK-002  2 lines  (CBBQ0001, MBBQ0159)

Pipeline: HEALTHY
```

### Key Output Conventions

- **Indentation**: Order header at left margin, stages indented 6 spaces.
- **SEND**: Webhook HTTP call and response status.
- **STAGE**: DB status transition, with the new status and elapsed time. Shows SYSPRO order number when available.
- **CHECK**: Validation assertion (idempotency, rejection).
- **PASS / FAIL**: Per-order verdict, always on its own line.
- **Timing**: Relative to the webhook send for that order (`+0ms` is the send itself).
- **Color** (when stdout is a TTY): green for PASS/submitted, red for FAIL, yellow for STAGE transitions, cyan for SEND, dim for timing. No color when piped.

### Failure Output

If an order gets stuck or ends in the wrong status:

```
[1/5] #PIPE-001  Single line, happy path
      SEND   webhook -> HTTP 200                          +0ms
      STAGE  pending                                     +12ms
      STAGE  pending -> processing                      +5.3s
      STAGE  processing -> failed                       +5.9s  "Invalid stock code CBBQ0001"
      FAIL   expected submitted, got failed
```

## Timing and Polling

- **Poll interval**: 250ms (DB query for order status).
- **Timeout per order**: 60 seconds from webhook send to expected final status. If the batch processor hasn't picked it up by then, the order is marked as timed out.
- **Batch processor interval**: The tool does NOT control the batch processor — it relies on the running service's `BATCH_INTERVAL`. For fast iteration, start the service with `BATCH_INTERVAL=3s`.
- **Inter-order delay**: Orders 1-2 are sent immediately (they wait for the batch cycle). Orders 3-5 are sent after orders 1-2 reach their final status, since they are rejection/idempotency checks that complete synchronously.

## Validation at Each Stage

| Stage | What's Validated |
|-------|-----------------|
| Webhook send | HTTP status code matches expectation (200, 401, or 422) |
| `pending` | Order exists in DB, `customer_account` = `WEBS01`, `order_number` matches, line count matches, `status` = `pending` |
| `processing` | `status` transitioned to `processing` (batch processor picked it up) |
| `submitted` | `status` = `submitted`, `syspro_order_number` is non-empty |
| `failed` | `status` = `failed`, `last_error` is non-empty (only validated when expected) |
| Idempotency | Second webhook with same ID returns 200 but order count hasn't increased |
| Rejection | No row created in `orders` table for invalid requests |

## Mock vs Live Mode

| Aspect | Mock (default) | Live |
|--------|---------------|------|
| SYSPRO | Local fake on high port | Real e.net on RIL-APP01 via VPN |
| VPN required | No | Yes |
| SYSPRO_ENET_URL | Points to local mock | Points to real e.net (from config) |
| Company | N/A | `RILT` (test) |
| Order numbers | `SO-MOCK-NNN` | Real SYSPRO SO numbers |
| Speed | Fast (~6s total) | Slower (~15-30s, depends on VPN/SYSPRO latency) |
| When to use | Development, CI, quick checks | Pre-go-live confidence, after Sarah fixes SORTOI commit |
| Cleanup | Truncate test orders from DB | Orders persist in SYSPRO RILT (acceptable for test company) |

## How to Run

### Quick Start (Mock Mode)

```bash
# Terminal 1: Start service with mock SYSPRO + fast batch interval
SYSPRO_ENET_URL=http://localhost:19100/SYSPROWCFService/Rest \
BATCH_INTERVAL=3s \
./scripts/run.sh

# Terminal 2: Run the pipeline test
go run ./cmd/pipeline-test
```

### Live Mode

```bash
# Terminal 1: VPN + service with fast batch interval
./scripts/vpn.sh up
BATCH_INTERVAL=3s ./scripts/run.sh

# Terminal 2: Run against real SYSPRO
go run ./cmd/pipeline-test --live
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--live` | `false` | Use real SYSPRO instead of mock |
| `--mock-port` | `19100` | Port for mock SYSPRO server (mock mode only) |
| `--target` | `http://localhost:8080` | Service URL to send webhooks to |
| `--timeout` | `60s` | Per-order timeout |
| `--no-color` | auto-detect | Disable color output |
| `--cleanup` | `true` | Delete test orders from DB after run |

### Environment Variables

The tool reads the same environment as the service for:
- `SHOPIFY_WEBHOOK_SECRET` — required, to sign test webhooks
- `DATABASE_URL` — required, to poll order status
- `PORT` — to construct the target URL (if `--target` not set)

## Project Layout

```
cmd/pipeline-test/
  main.go          # CLI entry point, flag parsing, orchestration
  mock_syspro.go   # Fake e.net REST server
  scenarios.go     # Test scenario definitions + order payload builders
  printer.go       # Console output formatting, color, timing
```

## What This Is NOT

- **Not a test suite.** It does not use `testing.T` or `go test`. It is a standalone binary with `main()`.
- **Not production code.** It lives in `cmd/` but is a developer tool only.
- **Not a load test.** It sends 5 orders. If you want load testing, that is a different tool.
- **Not a replacement for integration tests.** The 21 integration tests in `internal/integration/` remain the source of truth for correctness. This tool is for visual confidence and demos.

## Cleanup

By default (`--cleanup=true`), after printing results the tool deletes all orders it created from the database (matching on the `#PIPE-NNN` order number prefix). This keeps the dev DB clean. In live mode, SYSPRO test orders in `RILT` are not cleaned up (acceptable).

## Open Questions

1. **Should the tool also exercise stock sync?** Could send a webhook and verify that the inventory syncer triggers within its 2-second debounce. Adds complexity — probably a follow-up.
2. **Should there be a `--watch` mode?** Continuous polling that shows any order flowing through the system, not just ones the tool sent. Useful for watching real Shopify webhooks in development. Nice-to-have.
3. **CI integration?** Mock mode could run in CI with `docker compose up -d` + the service + the pipeline test. Worth considering but not blocking.
