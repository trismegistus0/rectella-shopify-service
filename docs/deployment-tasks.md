# Deployment Task Tracker — Rectella Shopify Service Go-Live

The single source of truth for what's left to ship. Update statuses inline as
work completes. Critical path notes at the bottom.

## Status legend

- ✅ done
- ⏳ in flight / blocked on listed dependency
- 📋 backlog (Phase 2)
- 🆕 just added

## Task table

| # | Task | Owner | Blocked on | Status |
|---|---|---|---|---|
| **Phase 1 — Local stack (today)** |||||
| 1 | Stock-list business object research + RILT probe (`cmd/invbrwtest`) | Bast | — | ✅ done |
| 2 | Shopify-first dynamic SKU lister + `SKULister` interface + Syncer dynamic mode + main.go wiring | Bast | — | ✅ committed `5807b2c` |
| 3 | Branch `feat/self-contained-pipeline-test` pushed to GitHub | Bast | — | ✅ |
| 4 | CI publishes `sha-eba4ab5` image to GHCR (workflow trigger fix) | Bast | — | ✅ committed `670937c` + `eba4ab5` |
| 5 | Bicep param file pinned to `sha-eba4ab5` | Bast | — | ✅ committed `fbdf7b4` |
| **In flight — local-side go-live readiness** |||||
| 6 | Sarah writes SQL query: `SELECT StockCode, QtyOnHand FROM <table> WHERE Warehouse = 'WEBS'` | Sarah | her | 🆕 asked |
| 7 | New SQL-based lister in service (replaces Shopify-first lister as primary; Shopify-first becomes fallback) | Bast | task 6 + SQL Server creds + driver decision | ⏳ |
| 8 | Live Shopify Admin API token created in live store custom app | Bast | live store admin access (already have) | ⏳ |
| 9 | Webhook signing secret captured from live store after registering `orders/create` against current Cloudflare tunnel URL | Bast | task 8 | ⏳ |
| 10 | Local config populated with `SHOPIFY_ACCESS_TOKEN`, `SHOPIFY_STORE_URL`, `SHOPIFY_WEBHOOK_SECRET`, `SYSPRO_WAREHOUSE=WEBS`, leave `SYSPRO_SKUS=` empty | Bast | tasks 8+9 | ⏳ |
| 11 | Bounce service, confirm boot log shows `stock sync enabled mode=dynamic warehouse=WEBS` | Bast | task 10 | ⏳ |
| 12 | Runtime-verify dynamic stock sync against RILT (or live SYSPRO if Sarah's already provided creds): observe `stock list refreshed count=N`, then `stock sync cycle complete skus_updated=N` | Bast | task 11 | ⏳ |
| 13 | Place real test order on live Shopify store with cheapest SKU, watch full flow webhook → DB → batch → SORTOI → real SYSPRO order number | Bast | task 12 | ⏳ |
| 14 | Sarah confirms the order is correct in SYSPRO + cancels it | Sarah | task 13 | ⏳ |
| **Phase 2 — Azure cutover (after local is green)** |||||
| 15 | Andrew @ NCS bumps Bast's Azure role to Contributor on whole "Rectella Azure Plan" subscription | NCS | email already sent | ⏳ |
| 16 | `az provider register --namespace Microsoft.Web` and `--namespace Microsoft.Compute` | Bast | task 15 | ⏳ |
| 17 | Sarah provides live SYSPRO company ID | Sarah | her | ⏳ |
| 18 | `az deployment group create` for `infra/app-service.bicep` (image already pinned to sha-eba4ab5) | Bast | tasks 15-17 | ⏳ |
| 19 | App Service env vars set: live `SYSPRO_COMPANY_ID`, `SYSPRO_WAREHOUSE=WEBS`, `SHOPIFY_*` from local validation, SQL Server connection string for the new lister | Bast | task 18 | ⏳ |
| 20 | SSH into App Service, verify TCP reachability `192.168.3.150:31002` and non-zero `egressBytesTransferred` on the VPN tunnel | Bast | task 18 | ⏳ |
| 21 | Verify SQL Server connectivity from App Service (RIL-DB01 over the VPN) | Bast | tasks 18+19 | ⏳ |
| 22 | Re-run live Shopify test order, this time webhook → Azure App Service → live SYSPRO company | Bast | tasks 19-21 | ⏳ |
| 23 | Sarah confirms test order in live SYSPRO + cancels | Sarah | task 22 | ⏳ |
| 24 | Swap Shopify webhook URL: Cloudflare tunnel → Azure FQDN | Bast | task 23 | ⏳ |
| 25 | Final live test order via the Azure path, confirm + cancel | Bast + Sarah | task 24 | ⏳ |
| 26 | Announce live, watch logs 2 hours | Bast | task 25 | ⏳ |
| **Tear-down + cleanup (after launch is stable)** |||||
| 27 | Delete the broken Container App + rectella-env from Shopify-RG | Bast | 24h post-launch stable | ⏳ |
| 28 | Delete the unused `apps-subnet` + `apps-subnet-rt` route table | Bast | task 27 | ⏳ |
| 29 | Stop local service + Cloudflare tunnel, archive logs | Bast | task 26 | ⏳ |
| **Phase 2 backlog (post-launch)** |||||
| 30 | ARSPAY automated cash-receipt posting (Sarah confident this is straightforward) | Bast + Sarah | her ARSPAY field defaults | 📋 |
| 31 | SYSPRO order cancellation handler (Shopify `orders/cancelled` webhook → cancel in SYSPRO) | Bast | scope decision | 📋 |
| 32 | Gift card handling (non-stocked SORTOI lines) | Bast | Liz approval | 📋 |
| 33 | Multi-warehouse support if scope changes | Bast | business decision | 📋 |

## Critical path right now

1. **Sarah writes the WEBS warehouse SQL query** (task 6)
2. **Bast creates Shopify Admin API token + webhook secret** (tasks 8+9)
3. **Bast wires the SQL lister + populates local config + bounces service** (tasks 7+10+11)
4. **Bast runs live test order through the local stack** (tasks 12+13)
5. **Sarah confirms in SYSPRO** (task 14)

That's the local-stack go-live gate. Once green, the Azure cutover (tasks 15-26) is mechanical and gated only on Andrew @ NCS unblocking the Azure permission bump (task 15).

## Notes

- The Shopify-first lister we built tonight is not wasted work — it becomes the fallback path when Sarah's SQL is unreachable (DB down, VPN down, credentials wrong). The `SKULister` interface keeps both implementations swappable.
- `infra/app-service.bicep` already has `RECONCILIATION_INTERVAL=15m` baked in — no need to set this manually.
- The `cmd/invbrwtest` probe tool stays in the repo as evidence of why we picked the Shopify-first / SQL approach over an e.net business object.
