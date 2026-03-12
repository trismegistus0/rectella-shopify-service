# Rectella Phase 1 — Go-live 31 March

## To Do
- Build batch processor (#1)
- Add non-stocked SORTOI lines / gift cards (#2, #5 — blocked on Liz)
- Add discount + delivery charge lines to SORTOI XML (SOW 2.1)
- Stock sync: SYSPRO e.net → Shopify inventory API (business object TBD)
- Shipment/despatch status feedback: SYSPRO → Shopify (business object TBD)
- Order cancellation handler (SOW 2.1)
- GET /orders?status=failed endpoint (#6)
- Deployment: Azure Container Apps + VPN Gateway setup
- Documentation: process flows, troubleshooting notes (SOW deliverable 3)
- Formal test plan documentation (SOW deliverable 3)

## In Progress
- SYSPRO e.net connectivity: ports 31001/40000 requested open (waiting on Reece)

## Blocked
- Gift card GL code + VAT config (waiting on Liz) (#5)
- All SYSPRO testing (waiting on port confirmation) (#3, #4)
- Stock sync warehouse nomination (waiting on Melanie/Reece)
- Shipment status: need to identify SYSPRO business object (Sarah)

## Done
- Webhook handler (HMAC + dedup + persist)
- SYSPRO e.net client (logon/SORTOI/logoff)
- SORTOI XML builder (stocked lines)
- Database: PostgreSQL + embedded migrations + pgxpool
- Health/readiness endpoints
- VPN split tunnel tooling (vpn.sh, vpn-monitor.sh)
- Dev tooling (run/check/test/reset/nuke)
- Batch processor design (docs/plans/2026-03-10-batch-processor-design.md)
- Project constraints register (docs/project-constraints.md)
- 31 unit tests, 10 integration test scenarios
