# CLAUDE.md

## Working Style

Sebastian is building this service and learning Go, testing patterns, and production engineering as he goes. The role here is **mentor first, tool second**:

- **Teach the why, not just the what.** When choosing an approach, explain the reasoning. When a Go idiom or testing pattern applies, name it and show how it connects to the bigger picture.
- **Be direct and concise.** Lead with the answer or the action. Don't pad with preamble or restate what Sebastian just said. If it fits in one sentence, use one sentence.
- **Surface better ways proactively.** If there's a cleaner pattern, a better tool, or a modern best practice — say so. Don't wait to be asked.
- **Quality from the start.** Write production-grade code. Use code review after implementation. Catch issues early, not in production.
- **Guide the next step.** After completing work, suggest what Sebastian should look at, try, or learn next. Don't just stop — point the way forward.

## Project Overview

**Rectella Shopify Service** — middleware bridging Shopify with SYSPRO 8 ERP for Rectella International. Go + PostgreSQL.

Rectella (Burnley, Lancashire) manufactures BBQ/grilling products under the **Bar-Be-Quick** brand. B2C Shopify site integrating with SYSPRO ERP.

- 13 simple stocked SKUs at launch
- All orders post to single SYSPRO customer account `WEBS01`
- Single warehouse (TBD), Shopify controls pricing in Phase 1

### Data Flows (Phase 1)

1. **Orders IN** (Shopify → Service → SYSPRO): Webhook-driven, staged in PostgreSQL, batch-submitted via `SORTOI`
2. **Stock sync OUT** (SYSPRO → Service → Shopify): Scheduled cron sync from single warehouse
3. **Shipment status BACK** (SYSPRO → Service → Shopify): Fulfilment status updates

## Build & Run

```bash
# Start PostgreSQL (uses network_mode: host to avoid Docker iptables issues)
docker compose up -d

# Load env and run
export $(grep -v '^#' .env | xargs)
go run ./cmd/server

# Build / test / lint
go build ./...
go test ./...                              # unit tests only (fast, no Docker)
go test -tags integration ./... -count=1   # unit + integration (spins up Postgres container)
go vet ./...
gofmt -l .

# Helper scripts
./scripts/run.sh         # Start PostgreSQL + load .env + run service
./scripts/check.sh       # Build + vet + fmt + all tests (unit + integration)
./scripts/test.sh        # Manual smoke test against running instance (legacy)
./scripts/reset.sh       # Truncate all tables (keep schema)
./scripts/nuke.sh        # Destroy DB volume + recreate from scratch
./scripts/vpn.sh         # VPN up|down|status|test|fix-hosts (mullvad-exclude + openconnect)
./scripts/vpn-monitor.sh # Self-healing VPN health check (run via cron or manually)
./scripts/probe-enet.sh  # Probe RIL-APP01 for e.net port (run once, VPN required)
```

## Project Layout

```
cmd/server/main.go                  # Entrypoint: config, DB, migrations, HTTP server
cmd/enettest/main.go                # SYSPRO e.net connectivity test (logon/logoff cycle)
internal/
  batch/
    processor.go                    # Batch processor: polling loop, SYSPRO submission, error handling
    processor_test.go               # 7 tests: empty batch, success, business/infra errors, dead-letter, Run
  model/order.go                    # Domain types: Order, OrderLine, OrderWithLines, WebhookEvent
  store/
    store.go                        # DB connection pool (pgxpool)
    migrate.go                      # Embedded SQL migrations
    order.go                        # WebhookExists, CreateOrder, FetchPendingOrders, UpdateOrderStatus, ListOrdersByStatus
    migrations/
      001_initial_schema.up.sql     # webhook_events, orders, order_lines tables
      001_initial_schema.down.sql   # Drop tables
  syspro/
    client.go                       # Client interface + enetClient (logon/transaction/logoff)
    session.go                      # Session interface + enetSession (batched order submission)
    xml.go                          # SORTOI XML builder (sortoiParams, sortoiDocument)
    client_test.go                  # httptest-based client tests
    session_test.go                 # Session lifecycle tests (open/submit/reuse/close)
    xml_test.go                     # XML builder unit tests
  webhook/
    handler.go                      # POST /webhooks/orders/create — OrderStore interface
    payload.go                      # Unexported Shopify JSON DTOs
    verify.go                       # HMAC-SHA256 verification
    handler_test.go                 # 11 table-driven handler tests (mock store)
    verify_test.go                  # 5 table-driven HMAC tests
    testdata/order_create.json      # Realistic BBQ order fixture
  integration/
    testhelper_test.go              # Shared test setup: Postgres container, mock SYSPRO, HTTP server
    pipeline_test.go                # 16 integration tests: webhook, pipeline, orders, health
config/config.go                    # Env var loading + validation
scripts/
  run.sh                            # Start PostgreSQL + service
  check.sh                          # Build + vet + fmt + all tests (unit + integration)
  test.sh                           # Manual smoke test against running instance (legacy)
  reset.sh                          # Truncate tables
  nuke.sh                           # Destroy + recreate DB
  vpn.sh                            # Rectella VPN connect/disconnect (mullvad-exclude + openconnect)
  vpn-monitor.sh                    # Self-healing VPN health monitor (6 checks, auto-heals 4)
  probe-enet.sh                     # e.net port discovery (candidate port probing)
  run-history/                      # Timestamped test run logs (gitignored)
docs/                               # Reference docs: emails, SOW, SYSPRO training (not code)
Dockerfile                          # Multi-stage Go build (non-root, Alpine)
docker-compose.yml                  # PostgreSQL 16 (network_mode: host)
.env                                # Local config (gitignored)
.env.example                        # Template
```

## What's Built

- **Webhook handler**: Receives `orders/create` webhooks, verifies HMAC-SHA256, deduplicates via `X-Shopify-Webhook-Id`, validates, maps to domain types, persists in single transaction
- **Idempotency**: Two layers — `WebhookExists` check + `ErrDuplicateWebhook` sentinel on PG unique violation (handles race conditions)
- **Database**: PostgreSQL with embedded migrations, connection pooling (pgx/v5)
- **Health endpoints**: `GET /health` (DB ping, no error leak), `GET /ready`
- **SYSPRO e.net client** (`internal/syspro/`): `Client` interface, `enetClient` (logon/transaction/logoff), SORTOI XML builder; 13 tests
- **VPN tooling** (`scripts/`): `vpn.sh` (connect/disconnect/test with Mullvad coexistence), `vpn-monitor.sh` (self-healing health monitor), DNS routing fix, managed `/etc/hosts` entries for RIL-APP01/RIL-DB01
- **Batch processor** (`internal/batch/`): Polls for pending orders, opens single SYSPRO session per batch, submits sequentially. Business errors mark `failed` and continue; infra errors stop batch. Dead-letters after 3 attempts. Single-flight guard prevents overlapping batches. Per-batch 5-minute timeout. Graceful 10s drain on shutdown.
- **Duplicate prevention**: Atomic `pending → processing` status transition before SYSPRO call + `syspro_order_number` stored on success for reconciliation
- **GET /orders?status=** endpoint: Operations visibility into order statuses (admin-token protected)
- **POST /orders/{id}/retry** endpoint: Re-queue failed/dead-lettered orders (admin-token protected)
- **Middleware**: Panic recovery, security headers (`X-Content-Type-Options`, `X-Frame-Options`, `Cache-Control`), request logging (method, path, status, duration, webhook_id)
- **Dockerfile**: Multi-stage Go build, non-root user, Alpine-based
- **Tests**: 43 unit tests (webhook handler + HMAC + SYSPRO client + XML builder + session + batch processor) + 16 Go integration tests (`internal/integration/`, `//go:build integration`) covering full pipeline: webhook → DB → batch → orders endpoint. Uses `testcontainers-go` with real Postgres. Run with `go test -tags integration ./...`

### Not Yet Built

- Gift card handling (non-stocked lines in SORTOI — pending Liz Buckley finance approval)
- Stock sync (SYSPRO e.net Query → Shopify inventory API)
- Shipment/fulfilment feedback
- Order cancellation handler

## Tech Stack

- **Go 1.26.0** (mise, `~/Work/.mise.toml`)
- **PostgreSQL 16** (Docker, network_mode: host)
- **pgx/v5** — only external dependency
- **SYSPRO 8**: e.net NetTcp:31001 (read/write) + REST:40000 (read) on `RIL-APP01`
- **Shopify**: Admin API + webhooks

## Key Design Rules

- **SORTOI batching**: Send one order at a time, but reuse the same login session. Log in once, send all orders one after another, log off once.
- **Gift cards**: Multi-purpose gift cards, zero VAT. Purchase: non-stocked line, positive amount, Gift Card Liability GL code. Redemption: non-stocked line, negative amount, same GL code. Uses `<NonStockedLine>` in SORTOI. (Sarah's proposal — pending Liz approval.)
- **Stage-then-process**: Never call SYSPRO from a webhook handler. Persist first, process async.
- **Single customer**: All orders → `WEBS01`. No multi-customer logic.
- **Batch processing**: Orders submitted to SYSPRO on a schedule, not per-webhook. Business object is **SORTOI** (sales order transaction import).
- **HMAC verification**: All webhooks verified via HMAC-SHA256. Reject unverified.
- **Idempotency**: Deduplicate on `X-Shopify-Webhook-Id`.
- **Graceful shutdown**: Drain in-flight requests before stopping.
- **Doc sync**: After implementing a significant feature, update CLAUDE.md — "What's Built", layout, and any affected design rules. Keep it accurate enough to onboard a new developer.

## Data Mapping — Shopify to SYSPRO

| Shopify | SYSPRO | Notes |
|---|---|---|
| `order.name` | Purchase Order Ref | e.g. `#BBQ1001` |
| `created_at` | Order Date | RFC3339 |
| `shipping_address.*` | Ship-To fields | Nil-safe |
| `gateway` | Payment Ref | Fallback: `payment_gateway_names` joined |
| `total_price` | Payment Amount | String → float64 |
| `line_items[].sku` | Stock Code | Must match SYSPRO exactly |
| `line_items[].price` | Unit Price | String → float64 |
| `line_items[].tax_lines[].price` | Tax Amount | Summed per line |

**Fixed values**: Customer `WEBS01`, Business Object `SORTOI`, Company ID from env.

## Environment Variables

```
SHOPIFY_WEBHOOK_SECRET    # HMAC secret for webhooks
SHOPIFY_API_KEY           # Shopify app API key
SHOPIFY_API_SECRET        # Shopify app secret
SHOPIFY_STORE_URL         # e.g. rectella.myshopify.com
SYSPRO_ENET_URL           # e.net endpoint on RIL-APP01
SYSPRO_OPERATOR           # SYSPRO operator
SYSPRO_PASSWORD           # SYSPRO password
SYSPRO_COMPANY_ID         # SYSPRO company ID
DATABASE_URL              # PostgreSQL connection string
PORT                      # HTTP listen port (default 8080)
ADMIN_TOKEN               # Shared secret for /orders and /orders/{id}/retry (optional, open if unset)
STOCK_SYNC_INTERVAL       # Default 15m
BATCH_INTERVAL            # Default 5m
LOG_LEVEL                 # debug/info/warn/error

# Operator-only (not consumed by service, documented for setup)
VPN_HOST                  # Cisco AnyConnect host
VPN_USERNAME              # VPN username
VPN_PASSWORD              # VPN password
```

## Phase 1 Scope Boundaries

**Out of scope** — do NOT build: returns/refunds, multi-warehouse, ERP pricing sync, automated payment posting, 3PL dashboard, carrier integrations, subscription products, hosting infrastructure.

## Infrastructure

- **VPN**: Cisco AnyConnect (`rectella-internationa-wireless-w-tqngtmvdtj.dynamic-m.com`)
- **App Server**: `RIL-APP01` (e.net SOAP)
- **DB Server**: `RIL-DB01` (SQL Server)
- **Managed IT**: NCS (`helpdesk@ncs.cloud`, ticket #44257)

## Deployment (Production)

- **Platform**: Azure Container Apps (single Go binary as Docker container)
- **Database**: Azure Database for PostgreSQL Flexible Server
- **Connectivity**: Azure VPN Gateway (Basic) → Rectella Meraki (site-to-site)
- **Cost estimate**: ~£55–75/month (Rectella's Azure subscription)
- **Constraints doc**: See `docs/project-constraints.md` for full deployment architecture

## Stakeholders

| Name | Role | Email |
|---|---|---|
| Clare Braithwaite | Project Lead (Flexr) | clare@flexr.co.uk |
| Melanie Higgins | SYSPRO/Operations (Rectella) | higginsm@rectella.com |
| Liz Buckley | Finance Director (Rectella) | buckleyl@rectella.com |
| Reece Taylor | SYSPRO Admin (Rectella) | taylorr@rectella.com |
| Ross Tomlinson | IT Support (NCS) | helpdesk@ncs.cloud |
| Sarah Adamo | SYSPRO Consultant (Ctrl Alt Insight) | sarah@ctrlaltinsight.co.uk |
| Sebastian Adamo | Developer (Ctrl Alt Insight) | sebastian@ctrlaltinsight.co.uk |

## Timeline

- **Started**: Late January 2026
- **Target go-live**: 31 March 2026
- **Hypercare**: Four weeks post go-live

## SYSPRO Reference Docs

Local path: `~/Documents/Syspro/` — not committed to repo (proprietary, large PDFs).

Key docs for this project:
- `sales-orders-reference-guide.pdf` — Sales Order Entry, line types (stocked/non-stocked/freight/misc), SORTOI fields
- `SYSPRO e.net Solutions Support Training Guide - SYSPRO 8.pdf` — e.net architecture, business objects, logon process, XML structure
- `trade-promotions-reference-guide.pdf` — Trade Promotions pricing (Sarah's approach)
- `inventory-control-reference-guide.pdf` — Stock codes, warehouses (stock sync)
- `general-ledger-integration-reference-guide.pdf` — GL codes (gift card liability)

### SORTOI XML Notes

- Stocked lines: `<StockLine>` with `<StockCode>`, `<OrderQty>`, `<Price>`
- Non-stocked lines: `<NonStockedLine>` with `<NStockCode>`, `<NStockDes>`, `<NOrderQty>`, `<NPrice>`, `<NProductClass>`
- Parameters: `<SalesOrders><Parameters>` with `<IgnoreWarnings>`, `<AlwaysUsePriceEntered>`, `<AllowZeroPrice>`
- Session GUID from `/Logon` must be supplied as `UserId` on every `/Transaction` call

## Environment Notes

- Arch Linux (Omarchy) + Hyprland
- Git default branch `master`, remote: `github.com/coldwinter1017/rectella-shopify-service` (private)
- Docker `network_mode: host` required — Docker bridge port mapping broken on kernel 6.18+ with nftables

## MCP Servers

```bash
# Project-scoped (stored in ~/.claude.json):
claude mcp add shopify-dev -- npx -y @shopify/dev-mcp@latest

# Installed as official plugins (stored in ~/.claude/settings.json):
# context7@claude-plugins-official  — docs lookup (use mcp__plugin_context7_context7__* tools)
# gopls-lsp@claude-plugins-official — Go LSP
```
