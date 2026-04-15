# Deployment Task Tracker — Rectella Shopify Service Go-Live

The single source of truth for what's left to ship. Update statuses inline as
work completes. No self-imposed timelines — every task is "as soon as its
dependencies clear". Critical path notes at the bottom.

## Status legend

- ✅ done
- ⏳ in flight / blocked on listed dependency
- 📋 backlog (Phase 2)
- 🆕 just added
- 🔴 hard blocker on critical path

## Task table

| # | Task | Owner | Blocked on | Status |
|---|---|---|---|---|
| **Phase 1 — Local stack** |||||
| 1 | Stock-list business object research + RILT probe (`cmd/invbrwtest`) | Bast | — | ✅ done — no usable BO on RILT |
| 2 | Shopify-first dynamic SKU lister + `SKULister` interface + Syncer dynamic mode + main.go wiring | Bast | — | ✅ committed `5807b2c` |
| 3 | Branch `feat/self-contained-pipeline-test` pushed to GitHub | Bast | — | ✅ |
| 4 | CI publishes `sha-eba4ab5` image to GHCR (workflow trigger fix) | Bast | — | ✅ committed `670937c` + `eba4ab5` |
| 5 | Bicep param file pinned to `sha-eba4ab5` | Bast | — | ✅ committed `fbdf7b4` |
| **In flight — local-side go-live readiness** |||||
| 6 | Sarah writes SQL query: `SELECT StockCode, QtyOnHand FROM <table> WHERE Warehouse = 'WEBS'` | Sarah | her | ⏳ asked |
| 7 | New SQL-based lister in service (replaces Shopify-first lister as primary; Shopify-first becomes fallback) | Bast | task 6 + SQL Server creds + driver decision | ✅ code committed `4686691` — runtime verify blocks on RIL-DB01 creds |
| 8 | Live Shopify Admin API token created in live store custom app | Bast | live store admin access | ⏳ |
| 9 | Webhook signing secret captured from live store after registering `orders/create` against current Cloudflare tunnel URL | Bast | task 8 | ⏳ |
| 10 | Local config populated with `SHOPIFY_ACCESS_TOKEN`, `SHOPIFY_STORE_URL`, `SHOPIFY_WEBHOOK_SECRET`, `SYSPRO_WAREHOUSE=WEBS`, leave `SYSPRO_SKUS=` empty | Bast | tasks 8+9 | ⏳ |
| 11 | Bounce service, confirm boot log shows `stock sync enabled mode=dynamic warehouse=WEBS` | Bast | task 10 | ⏳ |
| 12 | Runtime-verify dynamic stock sync against RILT: observe `stock list refreshed count=N`, then `stock sync cycle complete skus_updated=N` | Bast | task 11 | ⏳ |
| 13 | Place real test order on live Shopify store with cheapest SKU, watch full flow webhook → DB → batch → SORTOI → real SYSPRO order number | Bast | task 12 | ⏳ |
| 14 | Sarah confirms the order is correct in SYSPRO + cancels it | Sarah | task 13 | ⏳ |
| **Payment posting — MVP for go-live (promoted from Phase 2)** |||||
| 14a | **Minimum-viable payment posting: daily Shopify cash-receipt email to credit control.** Scheduled job pulls prior-day paid transactions from Shopify Transactions API, formats a CSV summary (order ref, gross amount, Shopify fee, net, gateway, processed_at), emails to Rectella credit control. This is the floor — Liz posts cash receipts manually in SYSPRO from the report. | Bast | Liz credit-control email + SMTP relay creds | ✅ code committed `77a6f54` — disabled until SMTP vars + CREDIT_CONTROL_TO set |
| **Phase 2 — Azure cutover (parallel with local-side; advances independently)** |||||
| 15 | Andrew bumps Bast's Azure role to Contributor on whole "Rectella Azure Plan" subscription | Andrew | — | ✅ confirmed working via `az provider register` |
| 16 | `az provider register --namespace Microsoft.Web` and `--namespace Microsoft.Compute` | Bast | task 15 | ✅ both `Registered` |
| 17 | Meraki phase-2 selector includes new App Service subnet CIDR `10.0.6.0/27` (or broader `10.0.0.0/16`) + firewall permits `10.0.6.0/27 → 192.168.3.150:31002` TCP | Andrew @ NCS | — | ✅ applied per Andrew; untested (needs task 20 to validate) |
| 17a | Re-attach `apps-subnet-rt` route table to `app-service-subnet` (`192.168.3.0/24 → VNG`) | Bast | — | ✅ attached via `az network vnet subnet update` |
| 17b | Re-attach `apps-subnet-rt` route table to `apps-subnet` (was detached during yesterday's debugging) | Bast | — | ✅ attached |
| 17c | Proved (twice) that ACA Consumption on `apps-subnet` cannot reach `192.168.3.150:31002` even with UDR attached and Meraki opened — curl timeout, VPN `egressBytesTransferred=0`. Confirms App Service path is mandatory. | Bast | — | ✅ documented |
| 18 | **🔴 App Service quota in uksouth — currently 0 for all SKUs (Basic/Free/Standard). File quota increase request for 3 Basic App Service Plan instances** | Bast (via Azure Portal support ticket) or Andrew | Andrew's role bump (done) | 🔴 BLOCKING — Portal support ticket required |
| 19 | `az deployment group create` for `infra/app-service.bicep` (image pinned to sha-eba4ab5) | Bast | task 18 + DATABASE_URL secret + live `SYSPRO_COMPANY_ID` / `SYSPRO_WAREHOUSE` / `SYSPRO_SKUS` env vars | ⏳ |
| 20 | SSH into App Service, verify TCP reachability `192.168.3.150:31002`, confirm `az network vpn-connection show -n Azure-to-Office` `egressBytesTransferred > 0`. **This is the real test of Andrew's Meraki change.** | Bast | task 19 | ⏳ |
| 21 | Verify SQL Server connectivity from App Service (RIL-DB01 over the VPN) if SQL lister is primary | Bast | tasks 7+19+20 | ⏳ |
| 22 | Re-run live Shopify test order, webhook → Azure App Service → live SYSPRO company | Bast | tasks 19-21 | ⏳ |
| 23 | Sarah confirms test order in live SYSPRO + cancels | Sarah | task 22 | ⏳ |
| 24 | Swap Shopify webhook URL: Cloudflare tunnel → Azure FQDN | Bast | task 23 | ⏳ |
| 25 | Final live test order via the Azure path, confirm + cancel | Bast + Sarah | task 24 | ⏳ |
| 26 | Announce live, monitor logs + dead-letter count | Bast | task 25 | ⏳ |
| **Tear-down + cleanup (after launch is stable)** |||||
| 27 | Delete the broken Container App + rectella-env from Shopify-RG | Bast | 24h post-launch stable | ⏳ |
| 28 | Delete the unused `apps-subnet` + `apps-subnet-rt` route table (careful — same route table also attached to app-service-subnet; clone first if needed) | Bast | task 27 | ⏳ |
| 29 | Stop local service + Cloudflare tunnel, archive logs | Bast | task 26 | ⏳ |
| **Phase 2 backlog (post-launch)** |||||
| 30 | ARSPAY automated cash-receipt posting — full automation replacing task 14a. Polling-cycle syncer posts gross + bank charges per Shopify order; SYSPRO AR module routes bank charges GL automatically. Posting period computed `YYYYMM` from `processed_at`. One cash-book code from Liz (`ARSPAY_CASH_BOOK` env). **Scaffold committed `3020e48`:** migration 006, store methods, Shopify transactions fetcher, payments syncer skeleton, SYSPRO `PostCashReceipt` stub returning `ErrCashReceiptNotImplemented`. Syncer disabled unless `PAYMENTS_SYNC_INTERVAL` set and no-op until XML builder lands. | Bast + Sarah | Sarah: ARSPAY XML field spec. Liz: `ARSPAY_CASH_BOOK` code + Phase-1 lift sign-off. | 📋 scaffold ready, XML + approvals pending |
| 31 | SYSPRO order cancellation handler (Shopify `orders/cancelled` webhook → cancel in SYSPRO) | Bast | scope decision | 📋 |
| 32 | Gift card handling (non-stocked SORTOI lines) | Bast | Liz approval | 📋 |
| 33 | Multi-warehouse support if scope changes | Bast | business decision | 📋 |
| 34 | VPN Basic SKU → Standard/HighPerf SKU upgrade (enables BGP, replaces UDR, reduces tunnel fragility) | Bast + Andrew | post-hypercare | 📋 |

## Critical path right now

Two independent tracks both advance. Neither blocks the other — pursue in parallel:

**Track A — Local-stack live validation (gated on humans + creds):**
1. Sarah writes the WEBS warehouse SQL query (task 6)
2. Bast creates Shopify Admin API token + webhook secret (tasks 8+9)
3. Bast wires the SQL lister + populates local config + bounces service (tasks 7+10+11)
4. Bast runs live test order through the local stack (tasks 12+13)
5. Sarah confirms in SYSPRO (task 14)

**Track B — Azure App Service deploy (gated on quota):**
1. **🔴 File App Service quota increase in Azure Portal** (task 18) — biggest current blocker
2. Collect deploy-time env vars (DATABASE_URL from existing Container App secret, live SYSPRO company ID, warehouse, SKU list)
3. `az deployment group create` (task 19)
4. SSH + curl SYSPRO from inside App Service — validates Meraki change (task 20)
5. Live test order → SYSPRO → confirm (tasks 22+23)
6. Webhook URL swap (task 24)
7. Final test + announce (tasks 25+26)

Track A does not depend on Track B: the local stack already works and can go live via the Cloudflare tunnel if needed as a temporary measure. Track B is required for durable production hosting.

## Azure state snapshot

| Item | State |
|---|---|
| Subscription | `Rectella Azure Plan` (`83a17335-...`) |
| Role | Bast = subscription Contributor ✓ |
| Providers registered | `Microsoft.App`, `Microsoft.Web`, `Microsoft.Compute`, `Microsoft.Quota`, `Microsoft.Support` |
| VNet | `Rectella-Network` 10.0.0.0/16 in Shopify-RG |
| Subnets | `default`, `GatewaySubnet`, `db-subnet` 10.0.2.0/24 (PG delegated), `apps-subnet` 10.0.4.0/23 (ACA delegated, rt attached), `app-service-subnet` 10.0.6.0/27 (Web delegated, rt attached) |
| Route table `apps-subnet-rt` | `192.168.3.0/24 → VirtualNetworkGateway`, attached to both apps-subnet and app-service-subnet |
| VPN Gateway `RectellaVPN` | Basic SKU, RouteBased, Connected |
| VPN Connection `Azure-to-Office` | Connected; `ingressBytes=1400` (IKE keepalives only); `egressBytes=0` (nothing Azure-originated has ever reached on-prem) |
| Local gateway `Office-Meraki` | Remote IP `212.250.238.82`, address space `192.168.3.0/24` |
| Private DNS `rectella.private.postgres.database.azure.com` | Linked to VNet ✓ |
| Existing Container App `rectella-shopify-service` | Running stale `:latest` image, `/health` 200, has populated secrets we can reuse |
| App Service quota (all SKUs) in uksouth | **0 — blocks deploy** |

## Notes

- The Shopify-first lister is not wasted work — it becomes the fallback path when Sarah's SQL is unreachable (DB down, VPN down, credentials wrong). The `SKULister` interface keeps both implementations swappable.
- `infra/app-service.bicep` has `vnetRouteAllEnabled: true` and `RECONCILIATION_INTERVAL=15m` baked in — no manual settings needed for those.
- `cmd/invbrwtest` stays in the repo as evidence of why the Shopify-first / SQL approach was picked over an e.net business object.
- Quota requests in the Azure Portal (`Help + support → New support request → Service and subscription limits → App Service`) are typically auto-approved in 5–30 minutes for single-digit Basic-tier asks on new subscriptions.
- Operator session collision: local service and Azure service share the same SYSPRO operator `ctrlaltinsight`. Before the first live Azure test order (task 22), stop the local service to avoid the second logon killing the first. Post-launch, consider a dedicated App Service operator account (post-hypercare task).

## ARSPAY design notes (from `arse-pay` investigation branch)

Context: partial Phase-1 scope pulled in via task 14a (daily email). Full automation remains task 30 for post-launch. Investigation captured here so neither side has to re-derive.

**Canonical SYSPRO cash-receipt shape (per Bast's SYSPRO knowledge):**
- `Amount` = gross (what customer paid, i.e. Shopify `total_price`)
- `BankCharges` = gross − net (Shopify Payments + gateway fees)
- SYSPRO computes net = Amount − BankCharges internally and posts to the cashbook
- Bank charges GL is NOT supplied — AR module integration settings route it automatically
- Only one GL code needed from Liz: `ARSPAY_CASH_BOOK` (cashbook code, e.g. `BANK1`)

**Posting period format:** `YYYYMM` (e.g. `202601` = Jan 2026), computed in Go via `processedAt.UTC().Format("200601")` — Go's reference-date layout, not a literal. Financial year = calendar year (1 Jan – 31 Dec).

**Timing wrinkle:** Shopify Payments `balance_transaction.fee` only populates after settlement (seconds to hours; sometimes days on third-party gateways). Real-time posting from `orders/create` webhook would frequently miss the fee. Solution: polling-cycle syncer (mirrors existing batch-processor / stock-syncer / fulfilment-syncer pattern) that scans `orders WHERE status='submitted' AND payment_posted_at IS NULL`, fetches transactions, posts when fee is known, skips until next cycle otherwise.

**Period-closure edge case:** if Liz closes the prior period before a straggling fee settles, SYSPRO rejects with "posting period closed". Mitigation: catch the error, re-derive period as current month, re-submit, log both for audit.

**Idempotency:** new `payment_postings` table with `UNIQUE (shopify_transaction_id)`. Second attempt = no-op.

**Scaffold work that's safe to build now without Sarah's XML spec or Liz's sign-off:**
1. Shopify Transactions API fetcher (`GET /admin/api/2025-04/orders/{id}/transactions.json`)
2. DB migration `002_payment_postings.up.sql` + down migration
3. `internal/payments/syncer.go` polling-loop skeleton (single-flight, graceful drain, 15m default interval)
4. `internal/syspro/cash_receipt.go` — `PostCashReceipt(ctx, r)` signature + XML builder stubbed with field-name TODOs
5. Unit tests with mocked Shopify + mocked SYSPRO `/Transaction` endpoint

**Still needed before task 30 can be completed:**
| From | What |
|---|---|
| Sarah | ARSPAY XML field spec — exact element names for Customer, Amount, BankCharges, Period, CashBook, PaymentReference, PaymentDate, plus `<Parameters>` block equivalents |
| Liz | `ARSPAY_CASH_BOOK` code (single value) |
| Liz | Sign-off to pull automated payment posting from Phase 2 into Phase 1 (currently out-of-scope in `CLAUDE.md:229`) |
| Bast | Credit control email address + outbound SMTP relay (for task 14a MVP) |
