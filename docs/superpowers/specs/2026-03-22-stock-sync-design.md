# Stock Sync Design Spec

**Date:** 2026-03-22
**Status:** Approved for implementation
**Scope:** Phase 1 go-live (31 March 2026)

## Overview

One-way stock sync: SYSPRO inventory quantities → Shopify inventory levels. Polls SYSPRO every 15 minutes, pushes absolute quantities to Shopify. Order-aware adjustments make it feel near-real-time.

## Data Flow

```
Every 15 minutes (or on webhook trigger):

1. Logon to SYSPRO
2. INVQRY × 13 SKUs (one call per SKU)
3. Logoff from SYSPRO
4. Query local DB for pending/processing order quantities
5. Compute: effective_qty = syspro_qty - reserved_qty
6. Clamp negatives to 0
7. Push all 13 to Shopify via single GraphQL mutation
```

## SYSPRO Side — INVQRY

### Endpoint

```
GET /Query/Query?UserId={GUID}&BusinessObject=INVQRY&XmlIn={url-encoded-xml}
```

No `XmlParameters` — Query class objects take only `XmlIn`.

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

### Response XML (key fields)

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

**Use `QtyAvailable`** — on-hand minus allocated to sales orders. This is the correct sellable quantity.

### Error handling

- Stock code not found → empty/missing `<WarehouseItem>` → log WARN, skip SKU
- Access denied → operator needs InvQuery permission → log ERROR, skip cycle
- Timeout/connection error → log WARN, skip cycle, increment failure counter

## Shopify Side — GraphQL Admin API

### Authentication

```
Header: X-Shopify-Access-Token: shpat_xxxxx
```

New env var: `SHOPIFY_ACCESS_TOKEN`

### One-time setup (on startup)

1. Query `locations(first: 10)` → cache the single `location_id`
2. Query `inventoryItems(query: "sku:'SKU1' OR sku:'SKU2' ...")` → build `SKU → inventory_item_id` map

Cache in memory. Re-fetch if a SKU lookup misses.

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

- `ignoreCompareQuantity: true` — SYSPRO is source of truth, no optimistic locking
- All 13 SKUs in one mutation — single API call per sync cycle
- `reason: "correction"` — standard for ERP sync

### Rate limits

GraphQL: 1,000-point bucket, refills at 50 points/sec. A single `inventorySetQuantities` with 13 items costs ~15 points. No risk of throttling at 15-minute intervals.

### Error handling

- `userErrors` per-item → log per-SKU, don't fail entire batch
- `THROTTLED` → wait `(cost - available) / restoreRate` seconds, retry once
- 5xx / connection error → log ERROR, skip Shopify push, SYSPRO data discarded (re-queried next cycle)

## Order-Aware Adjustments

Before pushing to Shopify, subtract quantities from orders that have been received by our service but not yet submitted to SYSPRO:

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

This closes the gap between "Shopify order received" and "SYSPRO stock decremented".

## Webhook-Triggered Sync

When the webhook handler stages a new order, it signals the stock syncer via a channel. The syncer debounces (2 second wait) then recalculates using **last-known SYSPRO values** minus updated reservations, and pushes to Shopify.

This does NOT re-query SYSPRO — it reuses cached values. SYSPRO is only queried on the 15-minute tick.

## Resilience Rules

| Rule | Detail |
|---|---|
| Never zero Shopify on SYSPRO failure | Stale data is better than blocking all sales |
| Push partial data | If 10 of 13 SKUs return, push those 10 |
| Clamp negatives to 0 | SYSPRO allows negative stock; Shopify should not show it |
| Sync immediately on startup | Don't wait 15 minutes after a service restart |
| Track consecutive SYSPRO failures | Escalate to ERROR after 3 consecutive |
| Single-flight guard | `sync.Mutex.TryLock()` prevents overlapping cycles |
| Per-cycle timeout | 2 minutes covers SYSPRO query + Shopify push |
| Graceful shutdown | Finish in-flight sync before stopping |

## File Structure

```
internal/
  inventory/
    syncer.go          # Polling loop, orchestration, order-aware adjustments
    syncer_test.go     # Unit tests
    syspro.go          # INVQRY XML builder + response parser
    syspro_test.go     # XML round-trip tests
    shopify.go         # GraphQL client (locations, inventory items, set quantities)
    shopify_test.go    # httptest-based client tests
```

## New Environment Variables

```
SHOPIFY_ACCESS_TOKEN     # shpat_... token from custom app (required for stock sync)
SYSPRO_WAREHOUSE         # Warehouse code to query (e.g. "WH01")
STOCK_SYNC_INTERVAL      # Already exists in config, default 15m
```

## Interfaces

```go
// InventoryQuerier queries SYSPRO for stock levels.
type InventoryQuerier interface {
    QueryStock(ctx context.Context, skus []string, warehouse string) (map[string]float64, error)
}

// InventoryPusher sets stock levels in Shopify.
type InventoryPusher interface {
    SetInventoryLevels(ctx context.Context, quantities map[string]int) error
}

// ReservationStore queries pending order quantities from the database.
type ReservationStore interface {
    FetchReservedQuantities(ctx context.Context) (map[string]int, error)
}
```

## What We Need From Clare

1. Shopify app with **Orders (read)** + **Inventory (read/write)** permissions
2. The **access token** (`shpat_...`)
3. All 13 products: **"Track inventory" ON**, **"Continue selling when out of stock" OFF**
4. Store URL (e.g. `rectella.myshopify.com`)

## Out of Scope (go-live)

- Automated Shopify order cancellation on oversell (manual for now)
- Adaptive polling frequency
- Safety stock buffers
- Multi-warehouse support
- Fulfilment/shipment feedback (separate feature)

## Testing Strategy

1. **Unit tests**: Mock SYSPRO (httptest), mock Shopify (httptest), mock DB store
2. **XML tests**: INVQRY request building + response parsing with real SYSPRO response fixtures
3. **Integration tests**: Full syncer with real Postgres, mock SYSPRO + mock Shopify
4. **Live test**: VPN → INVQRY against RILT test company (once InvQuery permission granted)
