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

- Live store: **barbequick.co.uk** (Shopify: `h0snak-s5.myshopify.com`)
- ~40 stocked SKUs at launch (confirmed by Clare, up from initial estimate of 13)
- All orders post to single SYSPRO customer account `WEBS01`
- Single warehouse `WEBS`, Shopify controls pricing

### Data Flows

1. **Orders IN** (Shopify → Service → SYSPRO): Webhook-driven, staged in PostgreSQL, batch-submitted via `SORTOI`
2. **Stock sync OUT** (SYSPRO → Service → Shopify): Scheduled sync from WEBS warehouse every 15m
3. **Shipment status BACK** (SYSPRO → Service → Shopify): Fulfilment status updates every 30m

## Build & Run

```bash
# Start PostgreSQL (network_mode: host, listen_addresses=localhost for security)
docker compose up -d

# Build and run (production — compiled binary)
go build -o ./rectella-service ./cmd/server
./rectella-service      # requires env vars loaded

# Or use the helper script (loads env, builds, runs)
./scripts/run.sh

# systemd (production — auto-restart on crash)
systemctl --user start rectella     # start
systemctl --user stop rectella      # stop
systemctl --user status rectella    # check
journalctl --user -u rectella -f   # tail logs

# Build / test / lint
go build ./...
go test ./...                              # unit tests only (fast, no Docker)
go test -tags integration ./... -count=1   # unit + integration (spins up Postgres container)
go vet ./...
gofmt -l .

# Self-contained pipeline test (mock SYSPRO + mock Shopify + real Postgres)
./scripts/pipeline.sh               # Starts service, mocks, runs 7 scenarios, cleans up
./scripts/pipeline.sh --live        # Real SYSPRO over VPN, mock Shopify

# Helper scripts
./scripts/run.sh         # Start PostgreSQL + build binary + load env + run service
./scripts/check.sh       # Build + vet + fmt + all tests (unit + integration)
./scripts/test.sh        # Manual smoke test against running instance (legacy)
./scripts/reset.sh       # Truncate all tables (keep schema)
./scripts/nuke.sh        # Destroy DB volume + recreate from scratch (requires confirmation)
./scripts/backup.sh      # pg_dump to ~/backups/rectella/ (30-day retention)
./scripts/vpn.sh         # VPN up|down|status|test|fix-hosts (mullvad-exclude + openconnect)
./scripts/vpn-monitor.sh # Self-healing VPN health check (run via cron or manually)
./scripts/probe-enet.sh  # Probe RIL-APP01 for e.net port (run once, VPN required)
```

## Project Layout

```
cmd/server/main.go                  # Entrypoint: config, DB, migrations, HTTP server
cmd/enettest/main.go                # SYSPRO e.net connectivity test (logon/logoff cycle)
cmd/sortoitest/main.go              # SORTOI test tool — submit test order to SYSPRO, dump raw response
cmd/sorqrytest/main.go              # SORQRY/INVQRY test tool — query order/stock, dump raw response
cmd/pipeline-test/                   # Visual pipeline test (mock SYSPRO + mock Shopify, fully self-contained)
cmd/sku-parity/                      # SKU parity audit: Shopify variants vs SYSPRO stock codes
cmd/benchmark/                       # Load testing tool
cmd/invbrwtest/                      # INVBRW business object probe (ruled out for stock sync)
internal/
  batch/
    processor.go                    # Batch processor: polling loop, SYSPRO submission, error handling
    processor_test.go               # 7 tests: empty batch, success, business/infra errors, dead-letter, Run
  model/order.go                    # Domain types: Order, OrderLine, OrderWithLines, WebhookEvent
  store/
    store.go                        # DB connection pool (pgxpool)
    migrate.go                      # Embedded SQL migrations (advisory lock, idempotent)
    order.go                        # WebhookExists, CreateOrder, FetchPendingOrders, UpdateOrderStatus, etc.
    migrations/                     # 7 migration pairs (001-007)
  syspro/
    client.go                       # Client interface + EnetClient (logon/transaction/query/logoff, 10MB cap)
    session.go                      # Session interface + enetSession (batched order submission)
    inventory.go                    # INVQRY XML builder + response parser + QueryStock()
    sorqry.go                       # SORQRY dispatch status query
    xml.go                          # SORTOI XML builder (VAT strip, StockTaxCode, freight, truncation)
    cash_receipt.go                 # ARSPAY cash receipt types + stub
    *_test.go                       # Full test coverage for all above
  inventory/
    syncer.go                       # Stock sync orchestrator: polling, debounce, order-aware adjustments
    shopify.go                      # Shopify GraphQL client: location/SKU discovery, SetInventoryLevels
    sqlserver_lister.go             # SQL Server SKU lister (Sarah's bq_WEBS_Whs_QoH view)
    *_test.go                       # 17 tests across syncer + shopify client
  fulfilment/
    syncer.go                       # Fulfilment sync: polls SORQRY, creates Shopify fulfilments
    shopify.go                      # Shopify GraphQL client: GetFulfillmentOrderID, CreateFulfillment
    *_test.go                       # 19 tests across syncer + shopify client
  cancellation/
    classifier.go                   # 6-disposition classification based on SORQRY state
  webhook/
    handler.go                      # POST /webhooks/orders/create — OrderStore interface + stock sync trigger
    cancel_handler.go               # POST /webhooks/orders/cancelled — classify-only gate
    payload.go                      # Unexported Shopify JSON DTOs
    verify.go                       # HMAC-SHA256 verification
    *_test.go                       # 16 tests: handler + HMAC + financial-status gate
    testdata/order_create.json      # Realistic BBQ order fixture
  reconcile/
    sweeper.go                      # Shopify Admin REST reconciliation (48h lookback, taxes_included preserved)
    sweeper_test.go                 # 7 tests: stage missing, skip existing/unpaid, VAT preservation, errors
  payments/
    syncer.go                       # ARSPAY polling syncer (scaffold, no-ops until XML builder lands)
    shopify_transactions.go         # Shopify Admin REST transaction fetcher
    report.go                       # Daily cash-receipt CSV report
    mailer.go                       # SMTP + STARTTLS mailer with MIME attachments
    daily_report.go                 # Scheduled daily report job
  integration/
    testhelper_test.go              # Shared test setup: Postgres container, mock SYSPRO, HTTP server
    pipeline_test.go                # 16 integration tests: webhook, pipeline, orders, health
config/config.go                    # Env var loading + validation (PLACEHOLDER guard, tax code map)
scripts/
  run.sh                            # Start PostgreSQL + build binary + run
  check.sh                          # Build + vet + fmt + all tests (unit + integration)
  test.sh                           # Manual smoke test against running instance (legacy)
  reset.sh                          # Truncate tables
  nuke.sh                           # Destroy + recreate DB (requires confirmation)
  backup.sh                         # pg_dump to ~/backups/rectella/ (30-day retention)
  wait-postgres.sh                  # PostgreSQL readiness check (for systemd)
  pipeline.sh                       # Self-contained pipeline test (mock SYSPRO + real Postgres)
  vpn.sh                            # Rectella VPN connect/disconnect (mullvad-exclude + openconnect)
  vpn-monitor.sh                    # Self-healing VPN health monitor (6 checks, auto-heals 4)
  probe-enet.sh                     # e.net port discovery (candidate port probing)
  rectella.service                  # systemd user unit (Restart=on-failure)
  rectella-backup.service           # systemd oneshot for pg_dump
  rectella-backup.timer             # systemd timer (every 6 hours)
  run-history/                      # Timestamped test run logs (gitignored)
docs/                               # Reference docs: emails, SOW, SYSPRO training (not code)
Dockerfile                          # Multi-stage Go build (non-root, Alpine)
docker-compose.yml                  # PostgreSQL 16 (network_mode: host, listen_addresses=localhost)
```

## What's Built

- **Webhook handler**: Receives `orders/create` webhooks, verifies HMAC-SHA256, deduplicates via `X-Shopify-Webhook-Id`, validates, maps to domain types, persists in single transaction. Financial-status gate rejects unpaid / pending / authorized / refunded / voided orders at the door (returns HTTP 200 with `skipped_unpaid` so Shopify doesn't retry).
- **Idempotency**: Two layers — `WebhookExists` check + `ErrDuplicateWebhook` sentinel on PG unique violation. Additional `ErrDuplicateOrder` on `shopify_order_id` collision.
- **Database**: PostgreSQL with embedded migrations (advisory-lock protected), connection pooling (pgx/v5), 7 migration files
- **Health endpoints**: `GET /health` (DB ping, no error leak), `GET /ready`
- **HTTP server hardening**: `ReadHeaderTimeout` (slowloris prevention), `ReadTimeout`, `WriteTimeout`, `IdleTimeout`. Panic recovery middleware, security headers (`X-Content-Type-Options`, `X-Frame-Options`, `Cache-Control`), request logging (method, path, status, duration, webhook_id). ADMIN_TOKEN empty warning at boot.
- **SYSPRO e.net client** (`internal/syspro/`): `Client` interface, `EnetClient` (GET-based logon/transaction/query/logoff on port 31002), SORTOI XML builder with VAT strip + per-line StockTaxCode override + net price calculation + canonical param set (AllocationAction, AcceptEarlierShipDate, ShipFromDefaultBin, AllowDuplicateOrderNumbers, OrderStatus, RequestedShipDate), INVQRY XML builder + response parser + `QueryStock()`, SORQRY dispatch status query, Windows-1252 response handling, session mutex for single-operator concurrency, optional `CompanyPassword` for live companies, 10MB response body size cap. Operator login rejects any GUID prefixed `ERROR`.
- **VAT handling** (`internal/syspro/xml.go`): Per-line VAT strip + StockTaxCode override. When Shopify sends `taxes_included=true` (UK standard), the middleware subtracts the absolute per-line tax from the gross price before emitting `<Price>` to SORTOI. Each line also gets a `<StockTaxCode>` override (A=20% standard, B=5% reduced/domestic fuel, Z=0% zero-rated) derived from Shopify's `tax_lines[].rate`. Same treatment for freight: `<FreightValue>` is net of shipping tax. Configurable via `SYSPRO_TAX_CODE_MAP` env var. Requires "Allow changes to tax code for stocked items" enabled in SYSPRO Sales Order Setup > Tax/Um tab. Verified end-to-end via 3 real Playwright checkout orders (orders 016031-016033).
- **Field truncation** (`internal/syspro/xml.go`): `truncate()` helper enforces SYSPRO XSD byte limits on CustomerPoNumber (30), address lines (40), postcode (15), email (80).
- **VPN tooling** (`scripts/`): `vpn.sh` (connect/disconnect/test with Mullvad coexistence), `vpn-monitor.sh` (self-healing health monitor), DNS routing fix, managed `/etc/hosts` entries for RIL-APP01/RIL-DB01
- **Batch processor** (`internal/batch/`): Polls for pending orders, opens single SYSPRO session per batch, submits sequentially. Business errors mark `failed` and continue; infra errors stop batch. Dead-letters after 3 attempts. Single-flight guard. Per-batch 5-minute timeout. Graceful 10s drain on shutdown.
- **Boot-time crash recovery**: `ResetStaleProcessing` sweep on startup flips orders stuck in `processing` for >10 minutes back to `pending`.
- **Duplicate prevention**: Atomic `pending -> processing` status transition before SYSPRO call + `syspro_order_number` stored on success
- **GET /orders?status=** endpoint: Operations visibility into order statuses (admin-token protected)
- **POST /orders/{id}/retry** endpoint: Re-queue failed/dead-lettered orders (admin-token protected)
- **Dockerfile**: Multi-stage Go build, non-root user, Alpine-based
- **Stock sync** (`internal/inventory/`): One-way SYSPRO -> Shopify inventory sync. Polls SYSPRO INVQRY every 15m, subtracts pending/processing order quantities, clamps to 0, pushes via Shopify GraphQL `inventorySetQuantities`. Webhook-triggered 2-second debounced sync. Lazy Shopify location/SKU discovery with caching. Single-flight guard, 3-minute per-cycle timeout. Zero-push rule: missing warehouse items pushed as 0 stock.
- **Fulfilment sync** (`internal/fulfilment/`): Polls SYSPRO SORQRY every 30m for submitted orders with status "9" (complete). Creates Shopify fulfilments via GraphQL `fulfillmentCreate` with tracking info (carrier from SYSPRO `ShippingInstrs`). Handles already-fulfilled idempotently. Single-flight guard, graceful drain.
- **Reconciliation sweeper** (`internal/reconcile/`): Polls Shopify Admin REST API to catch orders missed by webhook delivery (48h lookback). Preserves `taxes_included`, per-line `tax_lines`, and shipping `tax_lines` in the re-marshalled `RawPayload` so downstream VAT strip and StockTaxCode override work correctly on reconciled orders. First sweep fires immediately on startup.
- **Secret validation**: `config.Load()` refuses to boot on empty required vars OR values starting with `PLACEHOLDER`.
- **Graceful drain invariant**: Batch processor checks `ctx.Err()` between orders, so SIGTERM never leaves an order stuck in `processing`.
- **SKU parity audit tool** (`cmd/sku-parity/`): CLI that compares Shopify product variants against SYSPRO stock codes. Pre-go-live sanity check.
- **Operator runbook** (`docs/runbook.md`): Single-page playbook for ops handover.
- **Shipping/freight**: SORTOI `<FreightLine>` with `<FreightValue>` and `<FreightCost>` (net of shipping VAT when `taxes_included=true`). Zero shipping = no freight line emitted.
- **Cancellation classify-gate** (`internal/cancellation/`, `internal/webhook/cancel_handler.go`, migration 007): Shopify `orders/cancelled` webhooks classified into 6 dispositions based on SORQRY state. Phase 1 is classify-only. End-to-end verified against 5 dispositions live.
- **SORTOI param clean-up + live RIL support**: Canonical param set, `CompanyPassword` support, operator login error detection.
- **Zero-stock rule** (Sarah's rule): Missing WEBS warehouse items pushed as 0 to Shopify. Distinct from query errors (which preserve last-known level).
- **Dynamic SKU discovery**: Paginated Shopify GraphQL `productVariants` query. Lister precedence: SQL Server -> Shopify -> static slice.
- **SQL Server lister**: Sarah's `bq_WEBS_Whs_QoH` view on RIL-DB01. Blocks on RIL-DB01 credentials.
- **Payments scaffold — ARSPAY**: Polling-cycle syncer with stubbed `PostCashReceipt`. Disabled unless `PAYMENTS_SYNC_INTERVAL` set.
- **Graph API mailer** (`internal/payments/mailer.go`): Microsoft Graph `sendMail` via client-credentials OAuth. Token cache with 5-min safety margin + 401 refresh. Scoped to the `shopify-service@rectella.com` service mailbox via Entra `ApplicationAccessPolicy` (Andrew/NCS, 2026-04-23).
- **Daily cash-receipt email**: Scheduled CSV email to credit control (01:00 UTC). Uses Graph mailer. Disabled unless `GRAPH_*` + `CREDIT_CONTROL_TO` + `SHOPIFY_ACCESS_TOKEN` configured.
- **Daily order-intake email** (`internal/payments/intake.go`): Morning summary (06:00 UTC = 07:00 BST) of yesterday's orders — count, gross total, status breakdown, stuck-row (BBQ1026 fingerprint) count. HTML body + per-order CSV attachment. Disabled unless `GRAPH_*` + `ORDER_INTAKE_TO` configured.
- **Tests**: ~183 unit tests + 16 Go integration tests. All race-clean. Uses `testcontainers-go` with real Postgres.
- **End-to-end verified**: 3 real Playwright checkout orders through live Barbequick store -> Cloudflare tunnel -> local service -> live SYSPRO RIL, with correct VAT and StockTaxCode overrides confirmed via SORQRY.

### Not Yet Built

- Runtime verification of SQL Server lister (code done, blocks on RIL-DB01 creds)
- ARSPAY XML wire format + automated cash-receipt posting (scaffold done, blocks on Sarah's spec + Liz sign-off)
- Gift card handling (non-stocked lines in SORTOI — pending Liz Buckley finance approval)
- Auto-propagating `cancellable_in_syspro` dispositions to SYSPRO (Phase 2)
- GDPR data retention policy for `raw_payload` (NULLing after 90 days — needs Liz sign-off)

## Tech Stack

- **Go 1.26.0** (mise, `~/Work/.mise.toml`)
- **PostgreSQL 16** (Docker, network_mode: host, listen_addresses=localhost)
- **pgx/v5** — only external dependency
- **SYSPRO 8**: e.net REST on port 31002 (`http://192.168.3.150:31002/SYSPROWCFService/Rest`), GET-based API, live company `RIL` (with `CompanyPassword=LIVE`) — test company `RILT` still supported by omitting `SYSPRO_COMPANY_PASSWORD`
- **Shopify**: Admin API + webhooks, store `h0snak-s5.myshopify.com` (barbequick.co.uk)

## Key Design Rules

- **SORTOI batching**: Send one order at a time, but reuse the same login session. Log in once, send all orders one after another, log off once.
- **Gift cards**: Multi-purpose gift cards, zero VAT. Purchase: non-stocked line, positive amount, Gift Card Liability GL code. Redemption: non-stocked line, negative amount, same GL code. Uses `<NonStockedLine>` in SORTOI. (Pending Liz approval.)
- **Stage-then-process**: Never call SYSPRO from a webhook handler. Persist first, process async.
- **Single customer**: All orders -> `WEBS01`. No multi-customer logic.
- **Batch processing**: Orders submitted to SYSPRO on a schedule, not per-webhook. Business object is **SORTOI**.
- **HMAC verification**: All webhooks verified via HMAC-SHA256. Reject unverified.
- **Idempotency**: Deduplicate on `X-Shopify-Webhook-Id`.
- **Graceful shutdown**: Drain in-flight requests before stopping.
- **Doc sync**: After implementing a significant feature, update CLAUDE.md — "What's Built", layout, and any affected design rules.
- **Stock sync design**: SYSPRO `INVQRY` (one call per SKU, `QtyAvailable` field) -> Shopify GraphQL `inventorySetQuantities` (batch). Polls every 15m. Order-aware: subtracts pending/processing order quantities. Triggered sync on webhook receipt. Never zeros Shopify on SYSPRO failure. Clamps negatives to 0. Single-flight guard. Zero-push rule: missing warehouse items -> 0 stock pushed.
- **Pricing + VAT**: Shopify owns all deals/discounts. Prices sent to SYSPRO net of VAT via absolute subtraction (`line_items[].tax_lines[].price`), not rate-based division — avoids rounding drift. StockTaxCode override per line from Shopify's `tax_lines[].rate` (A=20%, B=5%, Z=0%). `<AlwaysUsePriceEntered>Y`. Gated on `taxes_included` — exclusive-pricing orders left untouched.
- **Discount handling**: Two channels and BOTH must be summed before VAT-stripping. (1) `line_items[].total_discount` is set for direct line-level discounts (rare, manual line edits in admin). (2) `line_items[].discount_allocations[].amount` is set for order-level discount codes (e.g. customer typing "BBQ40" at checkout) — Shopify writes `total_discount=0` for these and allocates the cut across lines. Per-unit discount = (sum of both) / quantity, subtracted from net price before SORTOI. Net-down per line, NOT a separate non-stocked discount line (Sarah's preference, 2026-04-27). Bug-fix point: the BBQ1025 over-billing incident exposed that `discount_allocations` was missing from both the webhook DTO and the reconciliation sweeper DTO, causing 25 orders to be billed at pre-discount price.
- **Session mutex**: SYSPRO allows only one session per operator (second logon kills the first). All SYSPRO callers share one `EnetClient` with a `sessionMu` mutex. Never create a second `EnetClient` with the same operator.
- **SORTOI silent drops**: SYSPRO silently ignores unknown XML elements. Empirically confirmed for `<Telephone>`, `<ProductTaxCode>`, `<TaxCode>`, `<MProductTaxCode>`. The correct per-line tax element is `<StockTaxCode>`, gated by a SYSPRO setup option.

## Data Mapping — Shopify to SYSPRO

| Shopify | SYSPRO | Notes |
|---|---|---|
| `order.name` | Purchase Order Ref | e.g. `#BBQ1001` |
| `created_at` | Order Date + RequestedShipDate | RFC3339 |
| `shipping_address.*` | Ship-To fields | Nil-safe, truncated to XSD limits |
| `gateway` | Payment Ref | Fallback: `payment_gateway_names` joined |
| `total_price` | Payment Amount | String -> float64 |
| `line_items[].sku` | Stock Code | Must match SYSPRO exactly |
| `line_items[].price` | Unit Price | Gross; net = price - (tax/qty) when taxes_included |
| `line_items[].tax_lines[].price` | Tax Amount | Summed per line, used for VAT strip |
| `line_items[].tax_lines[].rate` | StockTaxCode | 0.20->A, 0.05->B, 0.00->Z |
| `shipping_lines[].price` | FreightValue | Net of shipping tax when taxes_included |

**Fixed values**: Customer `WEBS01`, Business Object `SORTOI`, Company ID from env.

## Environment Variables

```
SHOPIFY_WEBHOOK_SECRET    # HMAC secret for webhooks
SHOPIFY_API_KEY           # Shopify app API key
SHOPIFY_API_SECRET        # Shopify app secret
SHOPIFY_STORE_URL         # h0snak-s5.myshopify.com (NOT rectella.myshopify.com — dead dev store)
SYSPRO_ENET_URL           # e.net endpoint on RIL-APP01
SYSPRO_OPERATOR           # SYSPRO operator
SYSPRO_PASSWORD           # SYSPRO password
SYSPRO_COMPANY_ID         # SYSPRO company ID (RILT=test, RIL=live)
SYSPRO_COMPANY_PASSWORD   # Required for live RIL (value: LIVE). Omit for RILT.
SYSPRO_ALLOCATION_ACTION  # SORTOI allocation mode — F=force / B=back-order / A=auto (default "A")
SYSPRO_TAX_CODE_MAP       # Rate-to-code mapping, default "0.20:A,0.05:B,0.00:Z" (Rectella confirmed)
DATABASE_URL              # PostgreSQL connection string
PORT                      # HTTP listen port (default 8080; use 9080 on NUC to avoid SearXNG)
ADMIN_TOKEN               # Shared secret for /orders and /orders/{id}/retry — REQUIRED in production
SHOPIFY_ACCESS_TOKEN      # shpat_... from custom app (required for stock sync, fulfilment, reconciliation)
SHOPIFY_LOCATION_ID       # Shopify location GID (optional, auto-discovered if unset)
SYSPRO_WAREHOUSE          # Warehouse code: WEBS (required for stock sync)
SYSPRO_SKUS               # Comma-separated SKUs (optional; empty triggers dynamic discovery)
SQLSERVER_DSN             # SQL Server DSN for primary SKU lister (optional)
STOCK_SYNC_INTERVAL       # Default 15m
BATCH_INTERVAL            # Default 5m
FULFILMENT_SYNC_INTERVAL  # Default 30m
RECONCILIATION_INTERVAL   # 0 = disabled; recommended 15m in production
PAYMENTS_SYNC_INTERVAL    # 0 = disabled; enables the ARSPAY syncer
LOG_LEVEL                 # debug/info/warn/error

# Outbound email via Microsoft Graph (Entra app "SysPro Shopify Graph API App")
# All four GRAPH_* vars required — reports disabled gracefully if any are missing.
# App has Mail.Send application permission scoped via ApplicationAccessPolicy to
# a single mailbox (shopify-service@rectella.com).
GRAPH_TENANT_ID           # Rectella Entra tenant GUID
GRAPH_CLIENT_ID           # App registration (client) ID
GRAPH_CLIENT_SECRET       # Client secret value — NEVER commit
GRAPH_SENDER_MAILBOX      # shopify-service@rectella.com

# Cash-receipt report (disabled unless CREDIT_CONTROL_TO set + SHOPIFY_ACCESS_TOKEN present)
CREDIT_CONTROL_TO         # comma-separated recipients
DAILY_REPORT_HOUR         # UTC hour (0-23), default 1 (= 01:00 GMT / 02:00 BST)

# Order-intake report (disabled unless ORDER_INTAKE_TO set)
ORDER_INTAKE_TO           # comma-separated recipients (ops/finance)
ORDER_INTAKE_HOUR         # UTC hour (0-23), default 6 (= 07:00 BST / 06:00 GMT)

# Operator-only (not consumed by service, documented for setup)
VPN_HOST                  # Cisco AnyConnect host
VPN_USERNAME              # VPN username
VPN_PASSWORD              # VPN password
```

## Current Deployment (NUC — Phase 1)

The service runs live on a **GMKtec K8 Plus NUC** (Arch Linux / Omarchy), processing real Barbequick customer orders.

- **Process management**: systemd user unit (`rectella.service`) with `Restart=on-failure`
- **Database**: PostgreSQL 16 in Docker, `listen_addresses=localhost` (no network exposure)
- **Database backups**: systemd timer (`rectella-backup.timer`) runs `pg_dump` every 6 hours to `~/backups/rectella/`, 30-day retention
- **VPN**: openconnect to Rectella's Cisco AnyConnect for SYSPRO access
- **Webhook delivery**: Cloudflare quick tunnel (`cloudflared tunnel --url http://localhost:9080`). URL is ephemeral — changes on restart, webhook URL in Shopify must be updated via Admin API. Both `orders/create` and `orders/cancelled` webhooks configured.
- **Logs**: stdout JSON via `slog`, viewable with `journalctl --user -u rectella -f`

### Known limitations of NUC deployment

- Cloudflare tunnel URL is ephemeral — NUC reboot requires tunnel restart + Shopify webhook URL update
- VPN is a bare openconnect process — not yet managed by systemd
- No monitoring/alerting beyond manual log inspection
- Reconciliation sweeper is the safety net for missed webhooks (15m interval, 48h lookback)

## Future Deployment (Azure — Phase 2/3)

- **Phase 2**: ctrlaltinsight Azure subscription ($200 free credit) — test deployment with mock SYSPRO
- **Phase 3**: Rectella Azure subscription — production with Azure VPN Gateway -> Meraki site-to-site
- **Platform**: Azure App Service (B1 Linux) or Container Apps
- **Database**: Azure Database for PostgreSQL Flexible Server
- **Cost estimate**: ~55-75 GBP/month (Rectella's subscription)
- **Constraints doc**: See `docs/project-constraints.md`

## Phase 1 Scope Boundaries

**Out of scope** — do NOT build: returns/refunds, multi-warehouse, ERP pricing sync, automated payment posting, 3PL dashboard, carrier integrations, subscription products.

## Infrastructure

- **VPN**: Cisco AnyConnect (`rectella-internationa-wireless-w-tqngtmvdtj.dynamic-m.com`)
- **App Server**: `RIL-APP01` (e.net SOAP, port 31002)
- **DB Server**: `RIL-DB01` (SQL Server)
- **Managed IT**: NCS (`helpdesk@ncs.cloud`, ticket #44257)

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

## Status

**Phase 1 LIVE** — service running on NUC, processing real customer orders from barbequick.co.uk through to live SYSPRO RIL. Hardened 2026-04-16 (Postgres lockdown, systemd, backups, reconciliation VAT fix, ReadHeaderTimeout).

Phase 2 Azure deployment is next. The go-live gaps live in the Claude memory file `project_golive_gaps.md`.

## SYSPRO Reference Docs

Local path: `~/Documents/Syspro/` — not committed to repo (proprietary, large PDFs).

Key docs for this project:
- `sales-orders-reference-guide.pdf` — Sales Order Entry, line types, SORTOI fields
- `SYSPRO e.net Solutions Support Training Guide - SYSPRO 8.pdf` — e.net architecture, business objects, logon process, XML structure
- `trade-promotions-reference-guide.pdf` — Trade Promotions pricing (Sarah's approach)
- `inventory-control-reference-guide.pdf` — Stock codes, warehouses (stock sync)
- `general-ledger-integration-reference-guide.pdf` — GL codes (gift card liability)

### SORTOI XML Notes

- Stocked lines: `<StockLine>` with `<StockCode>`, `<OrderQty>`, `<Price>`, `<StockTaxCode>` (per-line override, requires setup option)
- Non-stocked lines: `<NonStockedLine>` with `<NStockCode>`, `<NStockDes>`, `<NOrderQty>`, `<NPrice>`, `<NProductClass>`
- Freight: `<FreightLine>` with `<FreightValue>`, `<FreightCost>` (net of shipping tax when taxes_included)
- Parameters: `<Process>Import</Process>`, `<StatusInProcess>Y</StatusInProcess>`, `<ValidateOnly>N</ValidateOnly>`, `<IgnoreWarnings>W</IgnoreWarnings>`, `<AlwaysUsePriceEntered>Y</AlwaysUsePriceEntered>`, `<AllowZeroPrice>Y</AllowZeroPrice>`, `<AllocationAction>A</AllocationAction>`, `<AllowDuplicateOrderNumbers>Y</AllowDuplicateOrderNumbers>`
- Ship-to address: `<ShipAddress1>` through `<ShipAddress5>` + `<ShipPostalCode>` (NOT `Ship2Address` — SYSPRO silently ignores unknown elements)
- SORTOI Import response: clean success returns only `<StatusOfItems>`. Success with warnings returns `<Order><SalesOrder>XXXXXX</SalesOrder></Order>`. Failure returns `<Order><SalesOrder/></Order>` (empty).
- Session GUID from `/Logon` must be supplied as `UserId` on every `/Transaction` call
- SYSPRO silently drops unknown XML elements — never throws an error. Empirically confirmed: `<Telephone>`, `<ProductTaxCode>`, `<TaxCode>`, `<MProductTaxCode>` all accepted but ignored.
- `<StockTaxCode>` requires "Allow changes to tax code for stocked items" in Sales Order Setup > Tax/Um tab (Sarah enabled this on RIL).

## Environment Notes

- GMKtec K8 Plus NUC, Arch Linux (Omarchy) + Hyprland
- Git default branch `master`, remote: `github.com/trismegistus0/rectella-shopify-service`
- Docker `network_mode: host` required — Docker bridge port mapping broken on kernel 6.18+ with nftables
- SearXNG runs on port 8080 on NUC — use `PORT=9080` for the service locally
- Git remote: `github.com/trismegistus0/rectella-shopify-service` (public — .gitignore hardened)
- `rectella.myshopify.com` is a dead/expired dev store — do NOT use. Live store is `h0snak-s5.myshopify.com`

## MCP Servers

```bash
# Project-scoped (stored in ~/.claude.json):
claude mcp add shopify-dev -- npx -y @shopify/dev-mcp@latest

# Installed as official plugins (stored in ~/.claude/settings.json):
# context7@claude-plugins-official  — docs lookup
# gopls-lsp@claude-plugins-official — Go LSP
# linear — task management (HTTP MCP, OAuth)
```
