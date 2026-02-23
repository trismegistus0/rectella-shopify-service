# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Rectella Shopify Service** is a middleware integration service that bridges Shopify with SYSPRO 8 ERP for Rectella International. Built in Go with PostgreSQL for persistence.

### Business Context

Rectella International (Bancroft Road, Burnley, Lancashire, BB10 2TP) manufactures and distributes BBQ and grilling products under the **Bar-Be-Quick** brand. They are launching a new Shopify B2C website and need a reliable integration with their existing SYSPRO 8 ERP system.

- **Launch catalogue**: 13 simple stocked SKUs (BBQ grills and accessories)
- **Customer model**: B2C only -- all website orders post to a single SYSPRO customer account `WEBS01`
- **Warehouse**: Single nominated warehouse (TBD) for Phase 1
- **Replaces**: Existing BPA-based interface with a purpose-built, maintainable integration
- **Shopify controls pricing** in Phase 1 -- no ERP pricing sync

### What It Does (Phase 1)

Three data flows:

1. **Orders IN** (Shopify -> Service -> SYSPRO): Receives confirmed B2C order webhooks from Shopify, stages them in PostgreSQL, then batch-submits Sales Orders to SYSPRO via the SORTBO Business Object. Supports cancellations prior to fulfilment.
2. **Stock sync OUT** (SYSPRO -> Service -> Shopify): Scheduled synchronisation of available stock levels from a single SYSPRO warehouse to Shopify. Not real-time -- runs on a configurable cron schedule.
3. **Shipment status BACK** (SYSPRO -> Service -> Shopify): When shipment is confirmed in SYSPRO, updates order fulfilment status back in Shopify for customer visibility.

### Deployment Phases

1. **Local development** -- Go service + PostgreSQL (Docker Compose), mock Shopify webhook calls
2. **Customer test environment** -- Deploy against Rectella's test SYSPRO database/app server on their network (via VPN)
3. **Production** -- Managed cloud deployment, subscription held directly by Rectella

## Phase 1 Scope Boundaries

### In Scope

- Shopify-to-SYSPRO order integration (webhook-driven, staged, batch-submitted)
- Order cancellations prior to fulfilment
- Stock level synchronisation from SYSPRO to Shopify (single warehouse, scheduled)
- Shipment/despatch status feedback from SYSPRO to Shopify
- Staging database to protect against order loss
- Queue-based processing with retry and dead-letter handling
- Payment reference and amount passthrough to SYSPRO for finance visibility
- Error logging and clear visibility of failures
- Process and technical documentation

### Explicitly Out of Scope

DO NOT build any of the following in Phase 1:

- Returns and refund workflows
- Multi-warehouse allocation logic
- ERP-driven pricing or promotion synchronisation into Shopify
- Automated posting of payments into SYSPRO Debtors or Cash Book
- 3PL operational dashboard or warehouse user interface
- Carrier integrations beyond basic shipment status
- Subscription products
- Hosting infrastructure, cloud subscriptions, backups, security patching, or infrastructure monitoring

## Data Mapping -- Shopify to SYSPRO

### Order Header Fields

| Shopify Field | SYSPRO Field | Notes |
|---|---|---|
| Order Number | Purchase Order Reference | e.g. `#BBQ1001` |
| Date of Order | Order Date | When payment was completed |
| Expected Dispatch Date | Required Date | Calculated from order date |
| Delivery Address | Ship-To Address | Full address fields |
| Phone | Contact Phone | Customer phone number |
| Email | Contact Email | Customer email address |
| Payment Reference | Payment Ref (custom field) | Shopify payment gateway ref |
| Payment Amount | Payment Amount (custom field) | For finance reconciliation |

### Order Line Fields

| Shopify Field | SYSPRO Field | Notes |
|---|---|---|
| SKU | Stock Code | Must match SYSPRO stock code exactly |
| Quantity | Order Qty | |
| Price | Unit Price | Must match what was paid in Shopify (full or discounted) |
| Discount | Discount Value | If applicable |
| Shipping | Freight Charge | Per-order; may be zero (free shipping) |
| VAT | Tax Amount | |

### SYSPRO Fixed Values

- **Customer Account**: `WEBS01` (all web orders)
- **Warehouse**: Single warehouse, TBD
- **Business Object**: `SORTBO` (Sales Order Transaction Build)
- **Company ID**: Configured via `SYSPRO_COMPANY_ID` env var

### Sample SKUs (Launch Catalogue)

| SKU | Description | Price |
|---|---|---|
| CBBQ0001 | 21" Kamado Egg Charcoal BBQ Grill -- Ceramic-Walled Outdoor Oven | £599 |
| MBBQ0159 | 57cm Heavy-Gauge Charcoal BBQ Grill with 36cm Pizza Stone | £149 |
| MBBQ0025 | Brick Built-In Charcoal Grill & Bake BBQ -- Large 62 x 36cm Cooking Area | £85 |

13 simple stocked items at launch. Full catalogue TBD.

## Build & Run

Project is in early scaffolding -- `go.mod` + `CLAUDE.md` only so far.

```bash
go build ./...                      # Build all packages
go run ./cmd/server                 # Run the service (once cmd/server exists)
go test ./...                       # Run all tests
go test ./internal/webhook/...      # Run tests for a specific package
go test -run TestOrderCreate ./...  # Run a single test by name
go vet ./...                        # Static analysis
gofmt -l .                          # Check formatting
```

## Tech Stack

- **Language**: Go 1.25.7 (managed via mise in ~/Work/)
- **Database**: PostgreSQL (local dev via Docker Compose)
- **ERP**: SYSPRO 8 on SQL Server
  - **e.net Business Objects** (SOAP) on `RIL-APP01` -- used for Sales Order submission via `SORTBO`
  - **SQL Server** on `RIL-DB01` -- used for stock level queries
- **Shopify**: Admin REST/GraphQL API for outbound stock updates and fulfilment; webhooks for inbound orders
- **Source control**: Codeberg (`codeberg.org/speeder091/rectella-shopify-service`)

## Architecture Notes

### Design Philosophy

This service is a **mediator/queue system**. The core principle is: **stage everything in PostgreSQL first, then batch-process to SYSPRO**.

- Shopify webhooks are accepted, validated, and immediately persisted to the staging database
- A separate batch processor picks up staged orders on a configurable schedule and submits them to SYSPRO
- This decoupling means orders are never lost if SYSPRO is down for maintenance
- Minimise load on the SYSPRO app server -- batch rather than per-order API calls where possible

### Planned Layout

```
cmd/server/          # Main entrypoint
internal/
  webhook/           # Shopify webhook handlers + HMAC verification
  shopify/           # Outbound Shopify API calls (stock updates, fulfilment)
  syspro/            # SYSPRO 8 API client (e.net SOAP / REST)
  queue/             # Queue/staging, batch processing, retry, dead-letter logic
  store/             # PostgreSQL data access layer
  model/             # Shared domain types (orders, stock levels, shipments)
config/              # Configuration loading (env vars, config files)
migrations/          # SQL migration files
docker-compose.yml   # Local dev stack (PostgreSQL)
```

## Key Design Considerations

- **Shopify webhook verification**: All incoming webhooks must be verified via HMAC-SHA256 using the app's shared secret. Reject unverified requests.
- **Idempotency**: Shopify may send the same webhook multiple times. Use the `X-Shopify-Webhook-Id` header to deduplicate.
- **SYSPRO sessions**: SYSPRO APIs require a session token (logon/logoff). Handle token lifecycle and expiry.
- **Retry & dead letter**: Failed SYSPRO pushes should be retried with backoff. After max retries, move to a dead letter queue for manual review.
- **Graceful shutdown**: The service must drain in-flight requests before stopping (important for webhook processing).
- **Stage-then-process**: Never call SYSPRO directly from a webhook handler. Always persist to the staging DB first, then process asynchronously. This protects against data loss.
- **Batch processing**: Orders are submitted to SYSPRO on a configurable schedule (not per-webhook). This reduces load on the SYSPRO app server and allows for natural batching.
- **Single customer account**: All orders post to `WEBS01`. Do not implement multi-customer logic.
- **Order cancellation**: Support cancellation only prior to fulfilment. If Shopify indicates an order is cancelled and it has not yet been submitted to SYSPRO, remove it from the queue. If already submitted, flag for manual handling.
- **Stock sync scheduling**: Stock levels sync from SYSPRO to Shopify on a cron schedule (e.g. every 15 minutes). This is not real-time and not event-driven.

## Infrastructure & Access

### Rectella Test Environment

- **VPN**: Cisco AnyConnect, hostname `rectella-internationa-wireless-w-tqngtmvdtj.dynamic-m.com`
- **App Server**: `RIL-APP01` -- SYSPRO application server, e.net Business Objects (SOAP)
- **Database Server**: `RIL-DB01` -- SQL Server, SYSPRO database
- **SYSPRO Account**: Shared account (set up by Reece Taylor), local admin on both servers
- **Managed IT**: NCS (helpdesk@ncs.cloud), ticket #44257

### Local Development

- PostgreSQL via Docker Compose for staging database
- Mock Shopify webhooks for order testing
- VPN required only for SYSPRO integration testing (not local dev)

### Environment Variables

```
SHOPIFY_WEBHOOK_SECRET    # HMAC verification secret for incoming webhooks
SHOPIFY_API_KEY           # Shopify app API key
SHOPIFY_API_SECRET        # Shopify app API secret
SHOPIFY_STORE_URL         # e.g. rectella.myshopify.com

SYSPRO_ENET_URL           # e.net endpoint on RIL-APP01
SYSPRO_OPERATOR           # SYSPRO logon operator
SYSPRO_PASSWORD           # SYSPRO logon password
SYSPRO_COMPANY_ID         # SYSPRO company ID

DATABASE_URL              # PostgreSQL connection string

STOCK_SYNC_INTERVAL       # Cron interval for stock sync (e.g. "15m")
BATCH_INTERVAL            # Cron interval for order batch processing (e.g. "5m")
LOG_LEVEL                 # debug, info, warn, error
```

## Stakeholders

| Name | Role | Organisation | Email | Phone |
|---|---|---|---|---|
| Clare Braithwaite | Project Lead (Shopify/requirements) | Flexible Reinforcements (Flexr) | clare@flexr.co.uk | 01282 478212 |
| Melanie Higgins | SYSPRO / Operations | Rectella International | higginsm@rectella.com | |
| Liz Buckley | Finance Director | Rectella International | buckleyl@rectella.com | 01282 478200 / 07890 653106 |
| Reece Taylor | SYSPRO Admin | Rectella International | taylorr@rectella.com | |
| Ross Tomlinson | IT Support (VPN/servers) | NCS | helpdesk@ncs.cloud | |
| Chris Rawstron | IT Support (initial contact) | NCS | helpdesk@ncs.cloud | |
| Sarah Adamo | Consultant (SYSPRO expertise) | Ctrl Alt Insight | sarah@ctrlaltinsight.co.uk | 07905 406382 |
| Sebastian Adamo | Developer | Ctrl Alt Insight | sebastian@ctrlaltinsight.co.uk | |

## Timeline

- **Project started**: Late January 2026
- **VPN access granted**: Mid-February 2026 (NCS ticket #44257)
- **Scaffolding**: 23 February 2026
- **Target go-live**: 31 March 2026
- **Hypercare**: Four weeks post go-live

## MCP Servers

The following MCP servers are recommended for this project. Install globally or per-project:

```bash
# Shopify API docs and schema reference (essential)
claude mcp add shopify-dev -- npx -y @shopify/dev-mcp@latest

# PostgreSQL read-only access for inspecting dev/test databases
claude mcp add postgres -- npx -y @modelcontextprotocol/server-postgres "postgresql://user:pass@localhost:5432/rectella"

# Up-to-date library documentation (Go stdlib, pgx, chi, etc.)
claude mcp add context7 -- npx -y @upstash/context7-mcp@latest

# HTTP requests for testing endpoints and fetching docs
claude mcp add fetch -- npx -y @anthropic/mcp-server-fetch

# REST API testing with custom headers (Shopify HMAC, Syspro auth)
claude mcp add rest-api -- npx -y @dkmaker/mcp-rest-api
```

Optional depending on workflow:

```bash
# Go LSP intelligence (requires gopls v0.20.0+)
claude mcp add gopls -- mcp-gopls --workspace ~/Work/ctrlaltinsight/rectella-shopify-service
```

Note: No MCP server exists for SYSPRO. Use the fetch or rest-api MCPs to test e.net endpoints directly.

## Environment

- This is an Omarchy (Arch Linux + Hyprland) system
- Go 1.25.7 toolchain managed via mise (`~/Work/.mise.toml` adds `./bin` to PATH)
- Git default branch is `master`, pull rebases, push auto-sets upstream
- Remote: `codeberg.org/speeder091/rectella-shopify-service`
