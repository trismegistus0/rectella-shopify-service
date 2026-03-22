# Stock Sync Design Spec

**Date:** 2026-03-22
**Status:** Approved for implementation (revised after spec review)
**Scope:** Phase 1 go-live (31 March 2026)

## Overview

One-way stock sync: SYSPRO inventory quantities to Shopify inventory levels. Polls SYSPRO every 15 minutes, pushes absolute quantities to Shopify. Order-aware adjustments make it feel near-real-time.

## Data Flow

```
Every 15 minutes (or on webhook trigger):

1. Logon to SYSPRO
2. INVQRY x N SKUs (one call per SKU, list from SYSPRO_SKUS config)
3. Logoff from SYSPRO
4. Query local DB for pending/processing order quantities
5. Compute: effective_qty = syspro_qty - reserved_qty
6. Clamp negatives to 0
7. Push to Shopify via GraphQL inventorySetQuantities mutation
```

## SKU List

The list of SKUs to sync comes from `SYSPRO_SKUS` config (comma-separated).

Parsed at startup into `[]string`. If empty or unset, stock sync is disabled (log WARN, do not start polling loop).

Why not derive from Shopify? The inventory item lookup needs the SKU list as input. Why not from the database? Only shows SKUs from past orders, missing products with no sales yet. An explicit list is simple and correct for 13 products.

## SYSPRO Side -- INVQRY

### Endpoint

```
GET /Query/Query?UserId={GUID}&BusinessObject=INVQRY&XmlIn={url-encoded-xml}
```

No `XmlParameters` -- Query class objects take only `XmlIn` (confirmed: e.net training guide p.20).

### Implementation path

A new private `query()` method is added to `enetClient` (alongside the existing `transaction()` method). It calls `/Query/Query` with `UserId`, `BusinessObject`, and `XmlIn` params only.

`QueryStock()` is added to `enetClient` directly (NOT to the `Client` interface -- the batch processor does not need it). It handles: logon, N sequential INVQRY calls, logoff.

INVQRY code lives in `internal/syspro/inventory.go` (same package as `enetClient`, so it can call private methods).

### Request XML (per SKU)

```xml
<Query>
  <Key>
    <StockCode>CBBQ0001</StockCode>
  </Key>
  <Option>
    <WarehouseFilterType>S</WarehouseFilterType>
    <WarehouseFilterValue>{SYSPRO_WAREHOUSE}</WarehouseFilterValue>
  </Option>
</Query>
```

Note: `WarehouseFilterType` values (S=Single, A=All, R=Range) are inferred from SYSPRO patterns. Verify against `INVQRY.XSD` on RIL-APP01 during first live test.

### Response XML (expected -- needs live verification)

```xml
<InvQuery>
  <QueryOptions>
    <StockCode>CBBQ0001</StockCode>
    <Description>MBBQ Kamado BBQ</Description>
  </QueryOptions>
  <WarehouseItem>
    <Warehouse>WH01</Warehouse>
    <QtyOnHand>150.000</QtyOnHand>
    <QtyAvailable>120.000</QtyAvailable>
    <QtyAllocatedToSo>30.000</QtyAllocatedToSo>
  </WarehouseItem>
</InvQuery>
```

**Use `QtyAvailable`** -- on-hand minus allocated to sales orders (confirmed: Inventory Control ref guide pp.350, 354). Do NOT use `QtyOnHand` (includes allocated stock).

**Windows-1252 encoding:** SYSPRO returns `encoding="Windows-1252"` on all responses. The INVQRY parser must strip the XML declaration before `xml.Unmarshal`, same as the existing SORTOI response parser.

### Why the subtraction is safe (no double-counting)

`QtyAvailable` already excludes orders that SYSPRO has processed (in `QtyAllocatedToSo`). Our order-aware adjustment only subtracts `pending` and `processing` orders -- these have NOT been submitted to SYSPRO yet. Once an order reaches `submitted` status, SYSPRO handles it. The two sets are non-overlapping.

### Error handling

- Stock code not found: empty `<WarehouseItem>` -- log WARN, skip SKU, do not update Shopify for that SKU
- Access denied: operator needs InvQuery permission -- log ERROR, skip entire cycle
- Timeout/connection error: log WARN, skip cycle, increment consecutive failure counter

## Shopify Side -- GraphQL Admin API

### API Version

Pin to Shopify Admin API version **`2025-04`**:
```
POST https://{SHOPIFY_STORE_URL}/admin/api/2025-04/graphql.json
```

### Authentication

```
Header: X-Shopify-Access-Token: shpat_xxxxx
```

This is the custom app access token. Distinct from `SHOPIFY_API_KEY`/`SHOPIFY_API_SECRET` (used for webhook HMAC only). The access token authenticates all Admin API calls.

### HTTP Client -- No External Dependencies

Hand-rolled using Go stdlib `net/http`. POST to GraphQL endpoint with `Content-Type: application/json`, body `{"query": "...", "variables": {...}}`, access token header. Parse JSON response. No GraphQL library. Project stays on pgx/v5 as only external dep.

### One-time setup (on startup)

1. Query `locations(first: 50)` -- find location matching `SHOPIFY_LOCATION_ID` config, or use first active location if not set
2. Query `inventoryItems(first: 50, query: "sku:'SKU1' OR sku:'SKU2' ...")` -- build `SKU -> inventory_item_id` map

Cache in memory. 13 SKUs fit in one page (Shopify default is 50). No pagination needed.

**Startup failure:** If Shopify is unreachable at startup, service still starts. Stock sync logs ERROR and retries cache population on each tick. This prevents Azure Container Apps restart loops.

**Missing SKU:** If a SKU from `SYSPRO_SKUS` has no matching inventoryItem in Shopify, log WARN. On each full sync tick, re-attempt lookup for missing SKUs.

### Setting inventory levels

```graphql
mutation inventorySetQuantities($input: InventorySetQuantitiesInput!) {
  inventorySetQuantities(input: $input) {
    inventoryAdjustmentGroup {
      reason
      changes { name delta quantityAfterChange }
    }
    userErrors { code field message }
  }
}
```

Variables:
```json
{
  "input": {
    "name": "available",
    "reason": "correction",
    "ignoreCompareQuantity": true,
    "quantities": [
      {
        "inventoryItemId": "gid://shopify/InventoryItem/123",
        "locationId": "gid://shopify/Location/456",
        "quantity": 42
      }
    ]
  }
}
```

- `ignoreCompareQuantity: true` -- SYSPRO is source of truth, skip optimistic locking
- All resolved SKUs in one mutation -- single API call per sync cycle
- `reason: "correction"` -- standard for ERP sync
- Only include SKUs with resolved `inventory_item_id`
- Untracked products: Shopify returns `userError` per item -- handled by per-SKU error logging

### Rate limits

1,000-point bucket, 50 points/sec refill. ~15 points per mutation. No throttle risk at 15-min intervals. Log `currentlyAvailable` at DEBUG.

### Error handling

- `userErrors` per-item: log per-SKU at WARN, do not fail entire push
- `THROTTLED`: wait `(cost - available) / restoreRate` seconds, retry once
- 5xx / connection error: log ERROR, skip push, SYSPRO data discarded (re-queried next cycle)
- `ACCESS_DENIED`: log ERROR, config problem, skip cycle

## Order-Aware Adjustments

Subtract pending/processing order quantities before pushing to Shopify:

```sql
SELECT ol.sku, COALESCE(SUM(ol.quantity), 0) AS reserved
FROM order_lines ol
JOIN orders o ON o.id = ol.order_id
WHERE o.status IN ('pending', 'processing')
GROUP BY ol.sku
```

```
effective_qty = max(0, syspro_qty - reserved_qty)
```

**No new database migration required.** Uses existing `orders` and `order_lines` tables. New store method: `FetchReservedQuantities(ctx) (map[string]int, error)` in `internal/store/order.go`.

## Webhook-Triggered Sync

### Mechanism

- `main.go` creates `triggerCh := make(chan struct{}, 1)` and passes to both webhook handler and syncer
- Webhook handler: non-blocking send after successful `CreateOrder`: `select { case ch <- struct{}{}: default: }`
- Syncer `Run()` loop selects on both ticker and trigger channel
- On trigger: **2-second debounce** (timer-reset pattern -- each new signal resets the timer, fires 2s after last signal)
- Debounce path uses **cached SYSPRO values** minus fresh DB reservations, pushes to Shopify
- Does NOT re-query SYSPRO -- only queries on 15-minute tick

### Cold cache

Before first full sync completes, SYSPRO cache is empty. Webhook triggers during this window skip the Shopify push and log DEBUG "no cached SYSPRO data, skipping triggered sync". First full sync runs immediately on startup and populates the cache.

## Server Wiring (cmd/server/main.go)

### Constructor

```go
triggerCh := make(chan struct{}, 1)

syncer := inventory.NewSyncer(
    sysproClient,     // implements InventoryQuerier
    shopifyClient,    // implements InventoryPusher
    db,               // implements ReservationStore
    cfg.StockSyncInterval,
    cfg.SysproWarehouse,
    cfg.SysproSKUs,   // []string parsed from SYSPRO_SKUS
    triggerCh,
    logger,
)
```

Webhook handler receives `triggerCh` at construction.

### Startup

```go
go syncer.Run(syncCtx)
```

### Graceful shutdown

```go
syncCancel()
time.AfterFunc(10*time.Second, forceCancelFunc)
```

Same pattern as batch processor.

## Resilience Rules

| Rule | Detail |
|---|---|
| Never zero Shopify on SYSPRO failure | Stale data better than blocking all sales |
| Push partial data | If 10 of 13 SKUs return, push those 10 |
| Clamp negatives to 0 | SYSPRO allows negative; Shopify should not |
| Sync immediately on startup | First tick at T+0, not T+15m |
| Consecutive failure tracking | Reset on success. After 3, log ERROR |
| Single-flight guard | `mu.TryLock()` on `sync.Mutex` |
| Per-cycle timeout | 3 minutes (13 INVQRY calls + Shopify push) |
| Per-query timeout | 10 seconds per individual INVQRY call |
| Graceful shutdown | 10-second drain |
| Disabled if unconfigured | Empty SYSPRO_SKUS = no stock sync |

## Operational Visibility

No `/stock-sync/status` endpoint (Phase 1). Log-based only:

- Every successful sync: `slog.Info("stock sync complete", "skus_updated", N, "skus_skipped", M, "duration_ms", D)`
- Every failed sync: `slog.Warn("stock sync failed", "consecutive_failures", N, "error", err)`
- Every triggered sync: `slog.Info("triggered stock sync complete", "skus_updated", N)`
- Startup: `slog.Info("stock sync started", "interval", interval, "skus", len(skus))`

## File Structure

```
internal/
  inventory/
    syncer.go          # Polling loop, orchestration, order-aware adjustments, debounce
    syncer_test.go     # Unit tests (mock querier, pusher, store)
    shopify.go         # GraphQL client (locations, inventory items, set quantities)
    shopify_test.go    # httptest-based client tests
  syspro/
    client.go          # Add private query() method to enetClient
    inventory.go       # INVQRY XML builder + response parser + QueryStock() (NEW)
    inventory_test.go  # XML round-trip tests (NEW)
```

INVQRY code in `internal/syspro/` because it extends `enetClient` private methods.
Orchestration + Shopify in `internal/inventory/` (separate concern).

## New Config Variables

```
SHOPIFY_ACCESS_TOKEN     # shpat_... from custom app (required for stock sync)
SHOPIFY_LOCATION_ID      # Shopify location ID (optional, auto-discovered if unset)
SYSPRO_WAREHOUSE         # Warehouse code, e.g. "WH01" (required for stock sync)
SYSPRO_SKUS              # Comma-separated SKUs, e.g. "CBBQ0001,CBBQ0002" (required)
STOCK_SYNC_INTERVAL      # Already exists, default 15m
```

All added to `config/config.go` and the example config.

## Interfaces

```go
// InventoryQuerier queries SYSPRO for stock levels.
// Implemented by enetClient in internal/syspro/.
type InventoryQuerier interface {
    QueryStock(ctx context.Context, skus []string, warehouse string) (map[string]float64, error)
}

// InventoryPusher sets stock levels in Shopify.
// Implemented by ShopifyClient in internal/inventory/shopify.go.
type InventoryPusher interface {
    SetInventoryLevels(ctx context.Context, quantities map[string]int) error
}

// ReservationStore queries pending order quantities from the database.
// Implemented by *store.DB in internal/store/order.go.
type ReservationStore interface {
    FetchReservedQuantities(ctx context.Context) (map[string]int, error)
}
```

## What We Need From Clare

1. Shopify app with **Orders (read)** + **Inventory (read/write)** permissions
2. The **access token** (shpat_...) -- NOT the API key/secret
3. All 13 products: **"Track inventory" ON**, **"Continue selling when out of stock" OFF**
4. Store URL (e.g. rectella.myshopify.com)
5. The **SKU list** for all 13 products (must exactly match SYSPRO stock codes)

## What We Need From Sarah/Mel

1. **InvQuery permission** granted to `ctrlaltinsight` operator
2. **Warehouse code** for the single warehouse

## Deployment Notes

- No Dockerfile changes, no new ports
- New config vars must be set in Azure Container Apps
- No new database migration
- Stock sync disabled gracefully if config incomplete (service still handles orders)

## Out of Scope (go-live)

- Automated Shopify order cancellation on oversell (manual for now)
- Adaptive polling frequency
- Safety stock buffers
- Multi-warehouse support
- Fulfilment/shipment feedback (separate feature)
- /stock-sync/status monitoring endpoint

## Testing Strategy

1. **Unit tests**: Mock SYSPRO (httptest), mock Shopify (httptest), mock DB store
2. **XML tests**: INVQRY request building + response parsing with fixtures
3. **Integration tests**: Full syncer with real Postgres, mock SYSPRO + mock Shopify
4. **Live test**: VPN, INVQRY against RILT (once InvQuery permission granted)

## Live Verification Checklist (first VPN test)

- [ ] Grab INVQRY.XSD from SYSPRO server to confirm Option element names
- [ ] Make one live INVQRY call, log raw response XML
- [ ] Verify response root element name (expected: InvQuery)
- [ ] Verify QtyAvailable field exists with expected value
- [ ] Verify warehouse filtering works with actual warehouse code
- [ ] Update this spec with any corrections
