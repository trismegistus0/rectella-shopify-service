# ARSPAY investigation — handoff to main branch

Investigation done in the `arse-pay` conversation branch (2026-04-14). All
findings persisted to `docs/deployment-tasks.md` (commit `bafdcb5` on
`feat/self-contained-pipeline-test`). This file is the paste-target for
rehydrating the work into the main conversation's task list.

## What to do when you read this

1. Read `docs/deployment-tasks.md` — jump to the **ARSPAY design notes**
   section at the bottom and also check tasks **14a** and **30**.
2. Create these three visual tasks in Claude Code's task list:

### Task A — Daily Shopify cash-receipt email to credit control (MVP)

Minimum-viable payment posting so Liz can post cash receipts manually in
SYSPRO from a daily CSV/PDF. Unblocks finance sign-off for go-live
without full ARSPAY automation. Covers: pull prior-day paid Shopify
transactions, format summary (order ref, gross, Shopify fee, net,
gateway, `processed_at`, SYSPRO posting period `YYYYMM`), email to
Rectella credit control.

**Blocked on:** credit-control email address + outbound SMTP relay credentials.

### Task B — ARSPAY full automation (polling-cycle syncer)

Replaces Task A once ready. Scans
`orders WHERE status='submitted' AND payment_posted_at IS NULL`, fetches
Shopify transactions, posts to SYSPRO ARSPAY with gross in `Amount`,
(gross − net) in `BankCharges`, period `YYYYMM`, order name as payment
reference, WEBS01 customer. SYSPRO AR module auto-routes bank charges
GL. Polling cadence 15 min.

**Blocked on:** Sarah's ARSPAY XML field spec (element names) +
Liz's `ARSPAY_CASH_BOOK` code + Liz sign-off to lift payment posting
from Phase 2 into Phase 1 (currently out-of-scope per `CLAUDE.md:229`).

### Task C — Scaffold work for ARSPAY (safe pre-sign-off)

No external blockers — can start immediately:

1. Shopify Transactions API fetcher
   (`GET /admin/api/2025-04/orders/{id}/transactions.json`)
2. DB migration `002_payment_postings.up.sql` + down migration with
   `UNIQUE (shopify_transaction_id)` for idempotency
3. `internal/payments/syncer.go` polling-loop skeleton (single-flight,
   graceful drain, 15 min default interval)
4. `internal/syspro/cash_receipt.go` — `PostCashReceipt(ctx, r)`
   signature + XML builder stubbed with field-name TODOs
5. Unit tests with mocked Shopify + mocked SYSPRO `/Transaction`
   endpoint

## Key facts to remember (so you don't re-derive)

- **SYSPRO cash-receipt shape:** `Amount` = gross, `BankCharges` = gross − net,
  SYSPRO computes net internally and posts to the cashbook. One GL code
  needed from Liz (`ARSPAY_CASH_BOOK`), NOT two — the AR module's
  integration settings handle the bank-charges GL automatically.
- **Posting period format:** `YYYYMM`, computed in Go via
  `processedAt.UTC().Format("200601")`. Go's reference date is
  `Mon Jan 2 15:04:05 MST 2006`, so `"200601"` is a *layout string*
  meaning "4-digit year + 2-digit month" — it renders Jan 2026 as
  `"202601"`. Not a literal.
- **Financial year:** calendar year (1 Jan – 31 Dec).
- **Shopify fee timing:** `balance_transaction.fee` only populates
  after settlement (seconds to hours; days on some gateways). Real-time
  posting from `orders/create` webhook misses the fee frequently —
  hence the polling-cycle pattern.
- **Period closure edge case:** if Liz closes the prior period before a
  straggling fee settles, SYSPRO rejects with "posting period closed".
  Mitigation: catch that specific error, re-derive period as current
  month, re-submit, log both for audit. Alternative: dead-letter and
  Liz posts manually. Choose per Liz's preference.
- **Idempotency:** new `payment_postings` table, `UNIQUE
  (shopify_transaction_id)`, second attempt = no-op.
- **Don't hit SYSPRO from the webhook handler.** Same rule as orders
  (stage then process async).

## Things that are NOT in this investigation

- Refunds / credit notes (post-launch).
- Multi-currency (GBP only, explicit non-goal).
- Direct matching of receipts to SYSPRO invoices (cash-on-account
  against WEBS01 is the chosen shape).
- Shopify Payouts API — initial design is per-order, not per-payout.
