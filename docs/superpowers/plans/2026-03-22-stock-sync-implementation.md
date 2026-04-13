# Stock Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One-way stock sync: SYSPRO INVQRY quantities to Shopify inventory levels, polling every 15 minutes with order-aware adjustments and webhook-triggered near-instant updates.

**Architecture:** Syncer polls SYSPRO for stock via INVQRY (one call per SKU), subtracts pending/processing order quantities from the database, clamps to zero, and pushes absolute values to Shopify via GraphQL `inventorySetQuantities`. Webhook handler signals a trigger channel after each order, which fires a debounced push using cached SYSPRO data + fresh DB reservations.

**Tech Stack:** Go stdlib `net/http` (no GraphQL library), `encoding/xml`, `pgx/v5`, Shopify Admin API 2025-04.

**Spec:** `docs/superpowers/specs/2026-03-22-stock-sync-design.md`

---

## File Structure

### New files

| File | Responsibility |
|---|---|
| `internal/syspro/inventory.go` | INVQRY XML builder, response parser, `QueryStock()` method on `EnetClient` |
| `internal/syspro/inventory_test.go` | XML round-trip tests, `QueryStock` httptest tests |
| `internal/inventory/shopify.go` | Shopify GraphQL client: location discovery, inventory item cache, `SetInventoryLevels` |
| `internal/inventory/shopify_test.go` | httptest-based Shopify client tests |
| `internal/inventory/syncer.go` | Interfaces (`InventoryQuerier`, `InventoryPusher`, `ReservationStore`), `Syncer` struct, polling loop, debounce, order-aware adjustments |
| `internal/inventory/syncer_test.go` | Unit tests with mock querier/pusher/store |

### Modified files

| File | Change |
|---|---|
| `config/config.go` | Add `ShopifyAccessToken`, `ShopifyLocationID`, `SysproWarehouse`, `SysproSKUs` fields |
| `internal/syspro/client.go` | Export `EnetClient`, add private `query()` method |
| `internal/syspro/session.go` | Update receiver type to `EnetClient` |
| `internal/syspro/client_test.go` | Add `/Query/Query` handler to `fakeEnet` |
| `internal/store/order.go` | Add `FetchReservedQuantities(ctx) (map[string]int, error)` |
| `internal/webhook/handler.go` | Add `triggerCh chan<- struct{}` to `Handler`, non-blocking send after `CreateOrder` |
| `internal/webhook/handler_test.go` | Update `NewHandler` calls (pass `nil` triggerCh) |
| `cmd/server/main.go` | Wire syncer, trigger channel, graceful shutdown |
| `internal/integration/testhelper_test.go` | Update `NewHandler` call for triggerCh |

---

## Task 1: Config — Add stock sync config vars

**Files:**
- Modify: `config/config.go`

These are optional — stock sync is disabled gracefully if unconfigured. Service still handles orders.

- [ ] **Step 1: Add fields to Config struct**

In `config/config.go`, add after the `LogLevel` field (line 29):

```go
// Stock sync (optional — disabled if SysproSKUs is empty).
ShopifyAccessToken string
ShopifyLocationID  string
SysproWarehouse    string
SysproSKUs         []string
```

- [ ] **Step 2: Load optional config vars**

In `Load()`, add after `c.AdminToken = os.Getenv("ADMIN_TOKEN")` (line 62):

```go
c.ShopifyAccessToken = os.Getenv("SHOPIFY_ACCESS_TOKEN")
c.ShopifyLocationID = os.Getenv("SHOPIFY_LOCATION_ID")
c.SysproWarehouse = os.Getenv("SYSPRO_WAREHOUSE")

// Parse comma-separated SKU list.
if raw := os.Getenv("SYSPRO_SKUS"); raw != "" {
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			c.SysproSKUs = append(c.SysproSKUs, s)
		}
	}
}
```

Add `"strings"` to the import block.

- [ ] **Step 3: Verify it compiles**

Run: `go build ./...`
Expected: clean build, no errors.

- [ ] **Step 4: Run existing tests**

Run: `go test ./... -count=1`
Expected: all 46 unit tests pass (config changes are additive, nothing breaks).

- [ ] **Step 5: Commit**

```bash
git add config/config.go
git commit -m "feat: add stock sync config vars (SHOPIFY_ACCESS_TOKEN, SYSPRO_WAREHOUSE, SYSPRO_SKUS)"
```

---

## Task 2: SYSPRO INVQRY — query method, XML builder, parser, tests

**Files:**
- Modify: `internal/syspro/client.go` (add `query()` method, export `EnetClient`)
- Modify: `internal/syspro/session.go` (update receiver type)
- Modify: `internal/syspro/client_test.go` (add `/Query/Query` to `fakeEnet`)
- Create: `internal/syspro/inventory.go`
- Create: `internal/syspro/inventory_test.go`

### Step group A: Export EnetClient + add query()

- [ ] **Step 1: Export EnetClient**

In `internal/syspro/client.go`, rename `enetClient` to `EnetClient` throughout. This affects:
- The struct definition (line 49)
- `NewEnetClient` return type (line 59): change `Client` to `*EnetClient`
- All method receivers: `(c *enetClient)` to `(c *EnetClient)`

In `internal/syspro/session.go`, update:
- `enetSession` struct field: `client *EnetClient`
- `OpenSession` method: update receiver to `(c *EnetClient)`

In `internal/syspro/client_test.go`, update the `client()` helper:
- Return type: `func (f *fakeEnet) client(t *testing.T) *EnetClient`
- Struct literal: `return &EnetClient{` (was `&enetClient{`)

Also update `TestNewEnetClient_Interface` to preserve the compile-time interface check:
```go
var _ Client = NewEnetClient("http://example.com", "op", "pw", "co", slog.Default())
```

Note: `*EnetClient` still satisfies the `Client` interface — all existing code using `Client` continues to work. `main.go` receives `*EnetClient` directly from `NewEnetClient`, so no type assertion is needed for `QueryStock` or for passing to batch processor (which accepts `Client` interface).

- [ ] **Step 2: Add query() method to client.go**

In `internal/syspro/client.go`, add after the `transaction()` method (after line 152):

```go
// query calls GET /Query/Query and returns the raw XML response body.
// Query-class business objects take only XmlIn (no XmlParameters).
func (c *EnetClient) query(ctx context.Context, guid, businessObject, xmlIn string) (string, error) {
	params := url.Values{
		"UserId":         {guid},
		"BusinessObject": {businessObject},
		"XmlIn":          {xmlIn},
	}
	body, err := c.get(ctx, "/Query/Query", params)
	if err != nil {
		return "", err
	}
	var xmlStr string
	if err := json.Unmarshal(body, &xmlStr); err != nil {
		xmlStr = strings.TrimSpace(string(body))
	}
	if xmlStr == "" {
		return "", fmt.Errorf("query returned empty response")
	}
	c.logger.Debug("query response", "length", len(xmlStr), "first100", xmlStr[:min(100, len(xmlStr))])
	return xmlStr, nil
}
```

### Step group B: Test infrastructure

- [ ] **Step 3: Add query support to fakeEnet**

In `internal/syspro/client_test.go`, add fields to `fakeEnet` struct (after `transactXML`):

```go
queryCalls     int
queryResponses map[string]string // SKU → response XML (key extracted from XmlIn)
queryErr       bool
```

Update `newFakeEnet` initialization:

```go
f := &fakeEnet{transactXML: successfulSORTOIResponse, queryResponses: make(map[string]string)}
```

Add handler case in the `switch r.URL.Path` block (before `default:`):

```go
case "/Query/Query":
	f.queryCalls++
	if f.queryErr {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if params.Get("UserId") != testGUID {
		http.Error(w, "bad UserId", http.StatusBadRequest)
		return
	}
	xmlIn := params.Get("XmlIn")
	respXML := `<InvQuery><QueryOptions><StockCode>UNKNOWN</StockCode></QueryOptions></InvQuery>`
	for sku, xml := range f.queryResponses {
		if strings.Contains(xmlIn, sku) {
			respXML = xml
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(respXML)
```

Add `"strings"` to the import block in `client_test.go` if not already present.

### Step group C: INVQRY implementation + tests

- [ ] **Step 4: Write INVQRY XML tests**

Create `internal/syspro/inventory_test.go`:

```go
package syspro

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"
)

func TestBuildINVQRY(t *testing.T) {
	xmlStr, err := buildINVQRY("CBBQ0001", "WH01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(xmlStr, "<StockCode>CBBQ0001</StockCode>") {
		t.Errorf("expected StockCode in XML, got: %s", xmlStr)
	}
	if !strings.Contains(xmlStr, "<WarehouseFilterType>S</WarehouseFilterType>") {
		t.Errorf("expected WarehouseFilterType=S, got: %s", xmlStr)
	}
	if !strings.Contains(xmlStr, "<WarehouseFilterValue>WH01</WarehouseFilterValue>") {
		t.Errorf("expected WarehouseFilterValue=WH01, got: %s", xmlStr)
	}
}

func TestBuildINVQRY_RoundTrip(t *testing.T) {
	xmlStr, err := buildINVQRY("CBBQ0001", "WH01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req invqryRequest
	if err := xml.Unmarshal([]byte(xmlStr), &req); err != nil {
		t.Fatalf("round-trip unmarshal failed: %v", err)
	}
	if req.Key.StockCode != "CBBQ0001" {
		t.Errorf("expected StockCode=CBBQ0001, got %q", req.Key.StockCode)
	}
	if req.Option.WarehouseFilterType != "S" {
		t.Errorf("expected WarehouseFilterType=S, got %q", req.Option.WarehouseFilterType)
	}
	if req.Option.WarehouseFilterValue != "WH01" {
		t.Errorf("expected WarehouseFilterValue=WH01, got %q", req.Option.WarehouseFilterValue)
	}
}

const sampleINVQRYResponse = `<InvQuery>
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
</InvQuery>`

func TestParseINVQRY_Success(t *testing.T) {
	resp, err := parseINVQRY(sampleINVQRYResponse)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.QueryOptions.StockCode != "CBBQ0001" {
		t.Errorf("expected StockCode=CBBQ0001, got %q", resp.QueryOptions.StockCode)
	}
	if resp.QueryOptions.Description != "MBBQ Kamado BBQ" {
		t.Errorf("expected Description='MBBQ Kamado BBQ', got %q", resp.QueryOptions.Description)
	}
	if resp.WarehouseItem == nil {
		t.Fatal("expected WarehouseItem to be non-nil")
	}
	if resp.WarehouseItem.QtyAvailable != "120.000" {
		t.Errorf("expected QtyAvailable=120.000, got %q", resp.WarehouseItem.QtyAvailable)
	}
	if resp.WarehouseItem.QtyOnHand != "150.000" {
		t.Errorf("expected QtyOnHand=150.000, got %q", resp.WarehouseItem.QtyOnHand)
	}
}

func TestParseINVQRY_Windows1252(t *testing.T) {
	xml1252 := `<?xml version="1.0" encoding="Windows-1252"?>` + sampleINVQRYResponse
	resp, err := parseINVQRY(xml1252)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.WarehouseItem == nil {
		t.Fatal("expected WarehouseItem to be non-nil")
	}
	if resp.WarehouseItem.QtyAvailable != "120.000" {
		t.Errorf("expected QtyAvailable=120.000, got %q", resp.WarehouseItem.QtyAvailable)
	}
}

func TestParseINVQRY_EmptyWarehouse(t *testing.T) {
	emptyResp := `<InvQuery>
  <QueryOptions>
    <StockCode>UNKNOWN</StockCode>
    <Description></Description>
  </QueryOptions>
</InvQuery>`
	resp, err := parseINVQRY(emptyResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.WarehouseItem != nil {
		t.Errorf("expected nil WarehouseItem for unknown stock code")
	}
}

func TestParseINVQRY_InvalidXML(t *testing.T) {
	_, err := parseINVQRY("<broken>")
	if err == nil {
		t.Fatal("expected error for invalid XML, got nil")
	}
}

func TestQueryStock_Success(t *testing.T) {
	fake := newFakeEnet(t)
	fake.queryResponses["CBBQ0001"] = sampleINVQRYResponse
	fake.queryResponses["CBBQ0002"] = `<InvQuery>
  <QueryOptions><StockCode>CBBQ0002</StockCode><Description>BBQ Starter</Description></QueryOptions>
  <WarehouseItem>
    <Warehouse>WH01</Warehouse>
    <QtyOnHand>50.000</QtyOnHand>
    <QtyAvailable>45.000</QtyAvailable>
    <QtyAllocatedToSo>5.000</QtyAllocatedToSo>
  </WarehouseItem>
</InvQuery>`

	c := fake.client(t)
	result, err := c.QueryStock(context.Background(), []string{"CBBQ0001", "CBBQ0002"}, "WH01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if result["CBBQ0001"] != 120.0 {
		t.Errorf("CBBQ0001: expected 120.0, got %f", result["CBBQ0001"])
	}
	if result["CBBQ0002"] != 45.0 {
		t.Errorf("CBBQ0002: expected 45.0, got %f", result["CBBQ0002"])
	}

	if fake.logonCalls != 1 {
		t.Errorf("expected 1 logon, got %d", fake.logonCalls)
	}
	if fake.logoffCalls != 1 {
		t.Errorf("expected 1 logoff, got %d", fake.logoffCalls)
	}
	if fake.queryCalls != 2 {
		t.Errorf("expected 2 query calls, got %d", fake.queryCalls)
	}
}

func TestQueryStock_PartialFailure(t *testing.T) {
	fake := newFakeEnet(t)
	fake.queryResponses["CBBQ0001"] = sampleINVQRYResponse
	// CBBQ0002 has no configured response -> gets empty InvQuery -> nil WarehouseItem -> skipped

	c := fake.client(t)
	result, err := c.QueryStock(context.Background(), []string{"CBBQ0001", "CBBQ0002"}, "WH01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 result (partial), got %d", len(result))
	}
	if result["CBBQ0001"] != 120.0 {
		t.Errorf("CBBQ0001: expected 120.0, got %f", result["CBBQ0001"])
	}
}

func TestQueryStock_LogonFailure(t *testing.T) {
	fake := newFakeEnet(t)
	fake.logonErr = true

	c := fake.client(t)
	_, err := c.QueryStock(context.Background(), []string{"CBBQ0001"}, "WH01")
	if err == nil {
		t.Fatal("expected error on logon failure, got nil")
	}
	if !strings.Contains(err.Error(), "syspro logon") {
		t.Errorf("error should mention logon, got: %v", err)
	}
}

func TestQueryStock_QueryError(t *testing.T) {
	fake := newFakeEnet(t)
	fake.queryErr = true

	c := fake.client(t)
	result, err := c.QueryStock(context.Background(), []string{"CBBQ0001"}, "WH01")
	if err != nil {
		t.Fatalf("unexpected error (query errors are per-SKU): %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty results on query error, got %d", len(result))
	}
}
```

- [ ] **Step 5: Create inventory.go**

Create `internal/syspro/inventory.go`:

```go
package syspro

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// invqryRequest is the XML body for an INVQRY business object query.
type invqryRequest struct {
	XMLName xml.Name     `xml:"Query"`
	Key     invqryKey    `xml:"Key"`
	Option  invqryOption `xml:"Option"`
}

type invqryKey struct {
	StockCode string `xml:"StockCode"`
}

type invqryOption struct {
	WarehouseFilterType  string `xml:"WarehouseFilterType"`
	WarehouseFilterValue string `xml:"WarehouseFilterValue"`
}

// invqryResponse is the parsed INVQRY response XML.
type invqryResponse struct {
	XMLName       xml.Name         `xml:"InvQuery"`
	QueryOptions  invqryOptions    `xml:"QueryOptions"`
	WarehouseItem *invqryWarehouse `xml:"WarehouseItem"`
}

type invqryOptions struct {
	StockCode   string `xml:"StockCode"`
	Description string `xml:"Description"`
}

type invqryWarehouse struct {
	Warehouse        string `xml:"Warehouse"`
	QtyOnHand        string `xml:"QtyOnHand"`
	QtyAvailable     string `xml:"QtyAvailable"`
	QtyAllocatedToSo string `xml:"QtyAllocatedToSo"`
}

// buildINVQRY produces the XmlIn string for an INVQRY call.
func buildINVQRY(sku, warehouse string) (string, error) {
	req := invqryRequest{
		Key: invqryKey{StockCode: sku},
		Option: invqryOption{
			WarehouseFilterType:  "S",
			WarehouseFilterValue: warehouse,
		},
	}
	b, err := xml.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshalling INVQRY request: %w", err)
	}
	return string(b), nil
}

// parseINVQRY parses the XML response from an INVQRY query.
func parseINVQRY(xmlStr string) (*invqryResponse, error) {
	// Strip Windows-1252 XML declaration (same pattern as SORTOI parser).
	if i := strings.Index(xmlStr, "?>"); i != -1 {
		xmlStr = strings.TrimSpace(xmlStr[i+2:])
	}

	var resp invqryResponse
	if err := xml.Unmarshal([]byte(xmlStr), &resp); err != nil {
		return nil, fmt.Errorf("parsing INVQRY response: %w", err)
	}
	return &resp, nil
}

// QueryStock queries SYSPRO for stock levels of the given SKUs in the specified
// warehouse. Returns a map of SKU -> available quantity. SKUs that fail
// individually are logged and skipped (partial results returned).
//
// This is on EnetClient directly -- NOT on the Client interface (the batch
// processor does not need it).
func (c *EnetClient) QueryStock(ctx context.Context, skus []string, warehouse string) (map[string]float64, error) {
	guid, err := c.logon(ctx)
	if err != nil {
		return nil, fmt.Errorf("syspro logon: %w", err)
	}
	defer func() {
		if lerr := c.logoff(ctx, guid); lerr != nil {
			c.logger.Warn("syspro logoff failed", "error", lerr)
		}
	}()

	result := make(map[string]float64, len(skus))

	for _, sku := range skus {
		queryCtx, queryCancel := context.WithTimeout(ctx, 10*time.Second)

		xmlIn, err := buildINVQRY(sku, warehouse)
		if err != nil {
			c.logger.Warn("building INVQRY request", "sku", sku, "error", err)
			queryCancel()
			continue
		}

		respXML, err := c.query(queryCtx, guid, "INVQRY", xmlIn)
		queryCancel()
		if err != nil {
			c.logger.Warn("INVQRY query failed", "sku", sku, "error", err)
			continue
		}

		resp, err := parseINVQRY(respXML)
		if err != nil {
			c.logger.Warn("parsing INVQRY response", "sku", sku, "error", err)
			continue
		}

		if resp.WarehouseItem == nil {
			c.logger.Warn("stock code not found in warehouse", "sku", sku, "warehouse", warehouse)
			continue
		}

		qty, err := strconv.ParseFloat(strings.TrimSpace(resp.WarehouseItem.QtyAvailable), 64)
		if err != nil {
			c.logger.Warn("parsing QtyAvailable", "sku", sku, "value", resp.WarehouseItem.QtyAvailable, "error", err)
			continue
		}

		result[sku] = qty
	}

	return result, nil
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/syspro/ -v -count=1`
Expected: all SYSPRO tests pass (existing + new INVQRY + QueryStock tests).

- [ ] **Step 7: Commit**

```bash
git add internal/syspro/client.go internal/syspro/session.go internal/syspro/client_test.go internal/syspro/inventory.go internal/syspro/inventory_test.go
git commit -m "feat: add SYSPRO INVQRY query method, XML builder, and stock query"
```

---

## Task 3: Store — FetchReservedQuantities

**Files:**
- Modify: `internal/store/order.go`

- [ ] **Step 1: Add FetchReservedQuantities method**

In `internal/store/order.go`, add after the `ListOrdersByStatus` method:

```go
// FetchReservedQuantities returns the total quantity of each SKU in
// pending or processing orders. These orders have NOT been submitted to
// SYSPRO yet, so their quantities must be subtracted from SYSPRO's
// QtyAvailable to avoid overselling.
func (db *DB) FetchReservedQuantities(ctx context.Context) (map[string]int, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT ol.sku, COALESCE(SUM(ol.quantity), 0)::int AS reserved
		FROM order_lines ol
		JOIN orders o ON o.id = ol.order_id
		WHERE o.status IN ('pending', 'processing')
		GROUP BY ol.sku`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying reserved quantities: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var sku string
		var qty int
		if err := rows.Scan(&sku, &qty); err != nil {
			return nil, fmt.Errorf("scanning reserved quantity: %w", err)
		}
		result[sku] = qty
	}
	return result, rows.Err()
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./...`
Expected: clean build.

- [ ] **Step 3: Commit**

```bash
git add internal/store/order.go
git commit -m "feat: add FetchReservedQuantities for order-aware stock sync"
```

---

## Task 4: Shopify GraphQL Client

**Files:**
- Create: `internal/inventory/shopify.go`
- Create: `internal/inventory/shopify_test.go`

- [ ] **Step 1: Write Shopify client tests**

Create `internal/inventory/shopify_test.go`:

```go
package inventory

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeShopify is a configurable httptest server that mimics the Shopify Admin GraphQL API.
type fakeShopify struct {
	server          *httptest.Server
	calls           int
	lastQuery       string
	lastVariables   map[string]any
	locationResp    string
	inventoryResp   string
	setQuantityResp string
}

func newFakeShopify(t *testing.T) *fakeShopify {
	t.Helper()
	f := &fakeShopify{
		locationResp: `{
			"data": {
				"locations": {
					"edges": [{
						"node": {"id": "gid://shopify/Location/123", "name": "Main", "isActive": true}
					}]
				}
			}
		}`,
		inventoryResp: `{
			"data": {
				"inventoryItems": {
					"edges": [
						{"node": {"id": "gid://shopify/InventoryItem/1001", "sku": "CBBQ0001"}},
						{"node": {"id": "gid://shopify/InventoryItem/1002", "sku": "CBBQ0002"}}
					]
				}
			}
		}`,
		setQuantityResp: `{
			"data": {
				"inventorySetQuantities": {
					"inventoryAdjustmentGroup": {
						"reason": "correction",
						"changes": [{"name": "available", "delta": 10, "quantityAfterChange": 120}]
					},
					"userErrors": []
				}
			}
		}`,
	}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := r.Header.Get("X-Shopify-Access-Token")
		if token != "shpat_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal(body, &req)
		f.lastQuery = req.Query
		f.lastVariables = req.Variables

		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "locations("):
			w.Write([]byte(f.locationResp))
		case strings.Contains(req.Query, "inventoryItems("):
			w.Write([]byte(f.inventoryResp))
		case strings.Contains(req.Query, "inventorySetQuantities"):
			w.Write([]byte(f.setQuantityResp))
		default:
			http.Error(w, "unknown query", http.StatusBadRequest)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeShopify) client(t *testing.T, skus []string) *ShopifyClient {
	t.Helper()
	c := NewShopifyClient(
		"test.myshopify.com",
		"shpat_test",
		"",
		skus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	// Override endpoint and HTTP client for test server.
	c.baseURL = f.server.URL
	c.httpClient = f.server.Client()
	return c
}

func TestShopifyClient_SetInventoryLevels_Success(t *testing.T) {
	fake := newFakeShopify(t)
	c := fake.client(t, []string{"CBBQ0001", "CBBQ0002"})

	err := c.SetInventoryLevels(context.Background(), map[string]int{
		"CBBQ0001": 120,
		"CBBQ0002": 45,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have called: locations, inventoryItems, setQuantities = 3 calls.
	if fake.calls != 3 {
		t.Errorf("expected 3 API calls (location + inventory items + set), got %d", fake.calls)
	}

	if !strings.Contains(fake.lastQuery, "inventorySetQuantities") {
		t.Errorf("last query should be setQuantities mutation, got: %s", fake.lastQuery[:min(100, len(fake.lastQuery))])
	}
}

func TestShopifyClient_SetInventoryLevels_CachesLocation(t *testing.T) {
	fake := newFakeShopify(t)
	c := fake.client(t, []string{"CBBQ0001"})

	// First call: discovers location + inventory items.
	c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100})
	callsAfterFirst := fake.calls

	// Second call: should skip location + inventory item discovery.
	c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 110})

	// Only 1 additional call (setQuantities), not 3.
	if fake.calls-callsAfterFirst != 1 {
		t.Errorf("expected 1 call on second sync (cached), got %d", fake.calls-callsAfterFirst)
	}
}

func TestShopifyClient_SetInventoryLevels_ConfiguredLocation(t *testing.T) {
	fake := newFakeShopify(t)
	c := NewShopifyClient("test.myshopify.com", "shpat_test", "456", []string{"CBBQ0001"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = fake.server.URL
	c.httpClient = fake.server.Client()

	c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100})

	// Should skip location discovery (2 calls: inventory items + set).
	if fake.calls != 2 {
		t.Errorf("expected 2 API calls (skip location discovery), got %d", fake.calls)
	}
}

func TestShopifyClient_SetInventoryLevels_UnresolvedSKU(t *testing.T) {
	fake := newFakeShopify(t)
	c := fake.client(t, []string{"CBBQ0001", "CBBQ0002"})

	err := c.SetInventoryLevels(context.Background(), map[string]int{
		"CBBQ0001": 100,
		"CBBQ9999": 50, // not in Shopify
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestShopifyClient_SetInventoryLevels_NoSKUsResolved(t *testing.T) {
	fake := newFakeShopify(t)
	fake.inventoryResp = `{"data": {"inventoryItems": {"edges": []}}}`
	c := fake.client(t, []string{"UNKNOWN"})

	err := c.SetInventoryLevels(context.Background(), map[string]int{"UNKNOWN": 50})
	if err != nil {
		t.Fatalf("unexpected error (should succeed with warning): %v", err)
	}
}

func TestShopifyClient_SetInventoryLevels_UserErrors(t *testing.T) {
	fake := newFakeShopify(t)
	fake.setQuantityResp = `{
		"data": {
			"inventorySetQuantities": {
				"inventoryAdjustmentGroup": null,
				"userErrors": [{"code": "UNTRACKED", "field": ["quantities","0"], "message": "Item is not tracked"}]
			}
		}
	}`
	c := fake.client(t, []string{"CBBQ0001"})

	// User errors are logged but do not return Go error.
	err := c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100})
	if err != nil {
		t.Fatalf("unexpected Go error (user errors should be logged, not fatal): %v", err)
	}
}

func TestShopifyClient_SetInventoryLevels_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewShopifyClient("test.myshopify.com", "shpat_test", "123", []string{"CBBQ0001"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = srv.URL
	c.httpClient = srv.Client()

	err := c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100})
	if err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/inventory/ -v -count=1`
Expected: FAIL -- package/types don't exist yet.

- [ ] **Step 3: Create shopify.go**

Create `internal/inventory/shopify.go`:

```go
package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ShopifyClient handles Shopify Admin API GraphQL calls for inventory management.
type ShopifyClient struct {
	storeURL             string
	accessToken          string
	configuredLocationID string
	skus                 []string

	mu         sync.Mutex
	locationID string            // resolved GID
	skuMap     map[string]string // SKU -> inventory item GID

	// baseURL is the full GraphQL endpoint. Overridden in tests.
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewShopifyClient creates a Shopify inventory client.
func NewShopifyClient(storeURL, accessToken, locationID string, skus []string, logger *slog.Logger) *ShopifyClient {
	return &ShopifyClient{
		storeURL:             storeURL,
		accessToken:          accessToken,
		configuredLocationID: locationID,
		skus:                 skus,
		skuMap:               make(map[string]string),
		baseURL:              fmt.Sprintf("https://%s/admin/api/2025-04/graphql.json", strings.TrimRight(storeURL, "/")),
		httpClient:           &http.Client{Timeout: 30 * time.Second},
		logger:               logger,
	}
}

// graphqlResponse is the standard GraphQL response envelope.
type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *ShopifyClient) graphql(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling graphql request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("creating graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Access-Token", c.accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading graphql response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("parsing graphql response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", gqlResp.Errors[0].Message)
	}

	return gqlResp.Data, nil
}

func (c *ShopifyClient) resolveLocation(ctx context.Context) error {
	if c.configuredLocationID != "" {
		c.locationID = c.configuredLocationID
		if !strings.HasPrefix(c.locationID, "gid://") {
			c.locationID = fmt.Sprintf("gid://shopify/Location/%s", c.locationID)
		}
		return nil
	}

	const q = `{ locations(first: 50) { edges { node { id name isActive } } } }`

	data, err := c.graphql(ctx, q, nil)
	if err != nil {
		return fmt.Errorf("querying locations: %w", err)
	}

	var result struct {
		Locations struct {
			Edges []struct {
				Node struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					IsActive bool   `json:"isActive"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing locations: %w", err)
	}

	for _, edge := range result.Locations.Edges {
		if edge.Node.IsActive {
			c.locationID = edge.Node.ID
			c.logger.Info("discovered Shopify location", "id", edge.Node.ID, "name", edge.Node.Name)
			return nil
		}
	}

	return fmt.Errorf("no active Shopify locations found")
}

func (c *ShopifyClient) resolveInventoryItems(ctx context.Context) error {
	var parts []string
	for _, sku := range c.skus {
		if _, ok := c.skuMap[sku]; !ok {
			parts = append(parts, fmt.Sprintf("sku:'%s'", sku))
		}
	}
	if len(parts) == 0 {
		return nil
	}

	skuQuery := strings.Join(parts, " OR ")
	q := fmt.Sprintf(`{ inventoryItems(first: 50, query: %q) { edges { node { id sku } } } }`, skuQuery)

	data, err := c.graphql(ctx, q, nil)
	if err != nil {
		return fmt.Errorf("querying inventory items: %w", err)
	}

	var result struct {
		InventoryItems struct {
			Edges []struct {
				Node struct {
					ID  string `json:"id"`
					SKU string `json:"sku"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"inventoryItems"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing inventory items: %w", err)
	}

	for _, edge := range result.InventoryItems.Edges {
		c.skuMap[edge.Node.SKU] = edge.Node.ID
	}

	for _, sku := range c.skus {
		if _, ok := c.skuMap[sku]; !ok {
			c.logger.Warn("SKU not found in Shopify inventory", "sku", sku)
		}
	}

	return nil
}

// SetInventoryLevels pushes stock quantities to Shopify.
// quantities maps SKU -> effective quantity (already adjusted for pending orders).
func (c *ShopifyClient) SetInventoryLevels(ctx context.Context, quantities map[string]int) error {
	c.mu.Lock()

	if c.locationID == "" {
		if err := c.resolveLocation(ctx); err != nil {
			c.mu.Unlock()
			return fmt.Errorf("resolving location: %w", err)
		}
	}

	if len(c.skuMap) < len(c.skus) {
		if err := c.resolveInventoryItems(ctx); err != nil {
			c.logger.Warn("resolving inventory items", "error", err)
		}
	}

	locationID := c.locationID
	skuMap := make(map[string]string, len(c.skuMap))
	for k, v := range c.skuMap {
		skuMap[k] = v
	}
	c.mu.Unlock()

	type quantityInput struct {
		InventoryItemID string `json:"inventoryItemId"`
		LocationID      string `json:"locationId"`
		Quantity        int    `json:"quantity"`
	}

	var items []quantityInput
	for sku, qty := range quantities {
		itemID, ok := skuMap[sku]
		if !ok {
			c.logger.Warn("skipping SKU without inventory item ID", "sku", sku)
			continue
		}
		items = append(items, quantityInput{
			InventoryItemID: itemID,
			LocationID:      locationID,
			Quantity:        qty,
		})
	}

	if len(items) == 0 {
		c.logger.Warn("no resolved SKUs for inventory update")
		return nil
	}

	const mutation = `mutation inventorySetQuantities($input: InventorySetQuantitiesInput!) {
  inventorySetQuantities(input: $input) {
    inventoryAdjustmentGroup {
      reason
      changes { name delta quantityAfterChange }
    }
    userErrors { code field message }
  }
}`

	variables := map[string]any{
		"input": map[string]any{
			"name":                  "available",
			"reason":                "correction",
			"ignoreCompareQuantity": true,
			"quantities":            items,
		},
	}

	data, err := c.graphql(ctx, mutation, variables)
	if err != nil {
		return fmt.Errorf("inventory set quantities: %w", err)
	}

	var result struct {
		InventorySetQuantities struct {
			UserErrors []struct {
				Code    string   `json:"code"`
				Field   []string `json:"field"`
				Message string   `json:"message"`
			} `json:"userErrors"`
		} `json:"inventorySetQuantities"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing set quantities response: %w", err)
	}

	for _, ue := range result.InventorySetQuantities.UserErrors {
		c.logger.Warn("Shopify inventory user error",
			"code", ue.Code,
			"field", ue.Field,
			"message", ue.Message,
		)
	}

	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/inventory/ -v -count=1`
Expected: all Shopify client tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/inventory/shopify.go internal/inventory/shopify_test.go
git commit -m "feat: add Shopify GraphQL inventory client with lazy cache init"
```

---

## Task 5: Syncer — Polling Loop, Orchestration, Debounce

**Files:**
- Create: `internal/inventory/syncer.go`
- Create: `internal/inventory/syncer_test.go`

- [ ] **Step 1: Write syncer tests**

Create `internal/inventory/syncer_test.go`:

```go
package inventory

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type mockQuerier struct {
	mu    sync.Mutex
	stock map[string]float64
	err   error
	calls int
}

func (m *mockQuerier) QueryStock(ctx context.Context, skus []string, warehouse string) (map[string]float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.stock, nil
}

type mockPusher struct {
	mu        sync.Mutex
	lastPush  map[string]int
	err       error
	pushCalls int
}

func (m *mockPusher) SetInventoryLevels(ctx context.Context, quantities map[string]int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushCalls++
	if m.err != nil {
		return m.err
	}
	m.lastPush = make(map[string]int, len(quantities))
	for k, v := range quantities {
		m.lastPush[k] = v
	}
	return nil
}

type mockReservationStore struct {
	mu       sync.Mutex
	reserved map[string]int
	err      error
	calls    int
}

func (m *mockReservationStore) FetchReservedQuantities(ctx context.Context) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.reserved, nil
}

func syncerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSyncer_FullSync_ComputesEffectiveQuantity(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{
		"CBBQ0001": 120.0,
		"CBBQ0002": 50.0,
	}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{
		"CBBQ0001": 3,
	}}

	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001", "CBBQ0002"},
		make(chan struct{}, 1), syncerLogger())

	syncer.fullSync(context.Background())

	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push, got %d", p.pushCalls)
	}
	if p.lastPush["CBBQ0001"] != 117 {
		t.Errorf("CBBQ0001: expected 117, got %d", p.lastPush["CBBQ0001"])
	}
	if p.lastPush["CBBQ0002"] != 50 {
		t.Errorf("CBBQ0002: expected 50, got %d", p.lastPush["CBBQ0002"])
	}
}

func TestSyncer_FullSync_ClampsNegatives(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 2.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{"CBBQ0001": 5}}

	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())

	syncer.fullSync(context.Background())

	if p.lastPush["CBBQ0001"] != 0 {
		t.Errorf("expected 0 (clamped), got %d", p.lastPush["CBBQ0001"])
	}
}

func TestSyncer_FullSync_SysproFailure_NoPush(t *testing.T) {
	q := &mockQuerier{err: fmt.Errorf("syspro logon failed")}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}

	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())

	syncer.fullSync(context.Background())

	if p.pushCalls != 0 {
		t.Errorf("expected no push on SYSPRO failure, got %d", p.pushCalls)
	}
}

func TestSyncer_FullSync_PartialSysproData(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 100.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}

	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001", "CBBQ0002"},
		make(chan struct{}, 1), syncerLogger())

	syncer.fullSync(context.Background())

	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push (partial data), got %d", p.pushCalls)
	}
	if len(p.lastPush) != 1 {
		t.Errorf("expected 1 SKU in push, got %d", len(p.lastPush))
	}
}

func TestSyncer_TriggeredSync_UsesCachedSyspro(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 100.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{"CBBQ0001": 2}}

	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())

	syncer.fullSync(context.Background())
	initialQueryCalls := q.calls

	syncer.triggeredSync(context.Background())

	if q.calls != initialQueryCalls {
		t.Errorf("triggered sync should not query SYSPRO, but calls went from %d to %d", initialQueryCalls, q.calls)
	}
	if s.calls != 2 {
		t.Errorf("expected 2 reservation store calls (1 full + 1 triggered), got %d", s.calls)
	}
}

func TestSyncer_TriggeredSync_ColdCache_NoPush(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}

	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())

	syncer.triggeredSync(context.Background())

	if p.pushCalls != 0 {
		t.Errorf("expected no push on cold cache, got %d", p.pushCalls)
	}
}

func TestSyncer_Run_StopsOnContextCancel(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}

	syncer := NewSyncer(q, p, s, 50*time.Millisecond, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		syncer.Run(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

func TestSyncer_ConsecutiveFailures(t *testing.T) {
	q := &mockQuerier{err: fmt.Errorf("syspro down")}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}

	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())

	syncer.fullSync(context.Background())
	syncer.fullSync(context.Background())
	syncer.fullSync(context.Background())

	if syncer.consecutiveFailures != 3 {
		t.Errorf("expected 3 consecutive failures, got %d", syncer.consecutiveFailures)
	}

	q.err = nil
	q.stock = map[string]float64{"CBBQ0001": 10}
	syncer.fullSync(context.Background())

	if syncer.consecutiveFailures != 0 {
		t.Errorf("expected 0 after success, got %d", syncer.consecutiveFailures)
	}
}

func TestSyncer_ReservationStoreError_StillPushes(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 100.0}}
	p := &mockPusher{}
	s := &mockReservationStore{err: fmt.Errorf("db connection lost")}

	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		make(chan struct{}, 1), syncerLogger())

	syncer.fullSync(context.Background())

	if p.pushCalls != 1 {
		t.Fatalf("expected 1 push even with DB error, got %d", p.pushCalls)
	}
	if p.lastPush["CBBQ0001"] != 100 {
		t.Errorf("expected 100 (no reservation subtracted), got %d", p.lastPush["CBBQ0001"])
	}
}

func TestSyncer_Debounce_CoalescesMultipleSignals(t *testing.T) {
	q := &mockQuerier{stock: map[string]float64{"CBBQ0001": 100.0}}
	p := &mockPusher{}
	s := &mockReservationStore{reserved: map[string]int{}}

	triggerCh := make(chan struct{}, 1)
	syncer := NewSyncer(q, p, s, time.Hour, "WH01", []string{"CBBQ0001"},
		triggerCh, syncerLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		syncer.Run(ctx)
		close(done)
	}()

	// Wait for initial full sync (T+0).
	time.Sleep(100 * time.Millisecond)
	initialPushes := p.pushCalls

	// Send two rapid signals — should coalesce into one triggered sync.
	triggerCh <- struct{}{}
	time.Sleep(500 * time.Millisecond)
	triggerCh <- struct{}{} // resets the 2-second timer

	// Wait for debounce to fire (2s after last signal).
	time.Sleep(3 * time.Second)

	p.mu.Lock()
	pushesAfterDebounce := p.pushCalls
	p.mu.Unlock()

	// Should have exactly 1 additional push (not 2).
	if pushesAfterDebounce-initialPushes != 1 {
		t.Errorf("expected 1 debounced push, got %d", pushesAfterDebounce-initialPushes)
	}

	cancel()
	<-done
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/inventory/ -v -run "Syncer" -count=1`
Expected: FAIL -- `NewSyncer`, `fullSync`, `triggeredSync` undefined.

- [ ] **Step 3: Create syncer.go**

Create `internal/inventory/syncer.go`:

```go
package inventory

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"
)

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

// Syncer orchestrates one-way stock sync from SYSPRO to Shopify.
type Syncer struct {
	querier   InventoryQuerier
	pusher    InventoryPusher
	store     ReservationStore
	interval  time.Duration
	warehouse string
	skus      []string
	triggerCh <-chan struct{}
	logger    *slog.Logger

	syncMu              sync.Mutex // single-flight guard
	mu                  sync.Mutex // protects cachedStock + consecutiveFailures
	cachedStock         map[string]float64
	consecutiveFailures int
}

// NewSyncer creates a stock syncer.
func NewSyncer(
	querier InventoryQuerier,
	pusher InventoryPusher,
	store ReservationStore,
	interval time.Duration,
	warehouse string,
	skus []string,
	triggerCh <-chan struct{},
	logger *slog.Logger,
) *Syncer {
	return &Syncer{
		querier:   querier,
		pusher:    pusher,
		store:     store,
		interval:  interval,
		warehouse: warehouse,
		skus:      skus,
		triggerCh: triggerCh,
		logger:    logger,
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	s.logger.Info("stock sync started", "interval", s.interval, "skus", len(s.skus))

	// First tick at T+0.
	s.tick(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("stock sync stopping")
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return

		case <-ticker.C:
			s.tick(ctx)

		case <-s.triggerCh:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(2 * time.Second)
			debounceCh = debounceTimer.C

		case <-debounceCh:
			debounceCh = nil
			s.triggeredTick(ctx)
		}
	}
}

func (s *Syncer) tick(ctx context.Context) {
	if !s.syncMu.TryLock() {
		s.logger.Debug("stock sync already running, skipping tick")
		return
	}
	defer s.syncMu.Unlock()

	syncCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	s.fullSync(syncCtx)
}

func (s *Syncer) triggeredTick(ctx context.Context) {
	if !s.syncMu.TryLock() {
		s.logger.Debug("stock sync already running, skipping triggered sync")
		return
	}
	defer s.syncMu.Unlock()

	syncCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	s.triggeredSync(syncCtx)
}

// fullSync queries SYSPRO, applies order-aware adjustments, and pushes to Shopify.
func (s *Syncer) fullSync(ctx context.Context) {
	start := time.Now()

	stock, err := s.querier.QueryStock(ctx, s.skus, s.warehouse)
	if err != nil {
		s.mu.Lock()
		s.consecutiveFailures++
		failures := s.consecutiveFailures
		s.mu.Unlock()

		lvl := slog.LevelWarn
		if failures >= 3 {
			lvl = slog.LevelError
		}
		s.logger.Log(ctx, lvl, "stock sync failed",
			"consecutive_failures", failures,
			"error", err,
		)
		return
	}

	if len(stock) == 0 {
		s.logger.Warn("SYSPRO returned no stock data, skipping push")
		return
	}

	s.mu.Lock()
	s.cachedStock = stock
	s.consecutiveFailures = 0
	s.mu.Unlock()

	quantities := s.computeEffective(ctx, stock)

	if err := s.pusher.SetInventoryLevels(ctx, quantities); err != nil {
		s.logger.Error("pushing inventory to Shopify", "error", err)
		return
	}

	s.logger.Info("stock sync complete",
		"skus_updated", len(quantities),
		"skus_skipped", len(s.skus)-len(quantities),
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// triggeredSync uses cached SYSPRO values with fresh DB reservations.
func (s *Syncer) triggeredSync(ctx context.Context) {
	s.mu.Lock()
	cached := s.cachedStock
	s.mu.Unlock()

	if len(cached) == 0 {
		s.logger.Debug("no cached SYSPRO data, skipping triggered sync")
		return
	}

	quantities := s.computeEffective(ctx, cached)

	if err := s.pusher.SetInventoryLevels(ctx, quantities); err != nil {
		s.logger.Error("triggered sync push failed", "error", err)
		return
	}

	s.logger.Info("triggered stock sync complete", "skus_updated", len(quantities))
}

// computeEffective calculates effective_qty = max(0, syspro_qty - reserved_qty).
func (s *Syncer) computeEffective(ctx context.Context, stock map[string]float64) map[string]int {
	reserved, err := s.store.FetchReservedQuantities(ctx)
	if err != nil {
		s.logger.Warn("fetching reserved quantities, using zero", "error", err)
		reserved = map[string]int{}
	}

	quantities := make(map[string]int, len(stock))
	for sku, sysproQty := range stock {
		effective := int(math.Round(sysproQty)) - reserved[sku]
		if effective < 0 {
			effective = 0
		}
		quantities[sku] = effective
	}

	return quantities
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/inventory/ -v -count=1`
Expected: all inventory tests pass (Shopify + syncer).

- [ ] **Step 5: Commit**

```bash
git add internal/inventory/syncer.go internal/inventory/syncer_test.go
git commit -m "feat: add stock sync orchestrator with polling, debounce, order-aware adjustments"
```

---

## Task 6: Webhook Trigger Channel

**Files:**
- Modify: `internal/webhook/handler.go`
- Modify: `internal/webhook/handler_test.go`
- Modify: `internal/integration/testhelper_test.go`

- [ ] **Step 1: Add triggerCh to Handler**

In `internal/webhook/handler.go`, add `triggerCh` field to `Handler` struct:

```go
type Handler struct {
	store     OrderStore
	secret    string
	triggerCh chan<- struct{}
	logger    *slog.Logger
}
```

Update `NewHandler` signature:

```go
func NewHandler(store OrderStore, secret string, triggerCh chan<- struct{}, logger *slog.Logger) *Handler {
	return &Handler{
		store:     store,
		secret:    secret,
		triggerCh: triggerCh,
		logger:    logger,
	}
}
```

Add trigger send after `h.logger.Info("order staged", ...)` (before the final `h.respond`):

```go
	// Signal stock syncer that a new order arrived (non-blocking).
	if h.triggerCh != nil {
		select {
		case h.triggerCh <- struct{}{}:
		default:
		}
	}
```

- [ ] **Step 2: Update handler_test.go**

In `internal/webhook/handler_test.go`, update the `NewHandler` call (line 297):

Change: `h := NewHandler(tt.store, testSecret, testLogger())`
To: `h := NewHandler(tt.store, testSecret, nil, testLogger())`

- [ ] **Step 3: Update integration testhelper_test.go**

In `internal/integration/testhelper_test.go`, update the `NewHandler` call (line 139):

Change: `wh := webhook.NewHandler(db, secret, logger)`
To: `wh := webhook.NewHandler(db, secret, nil, logger)`

- [ ] **Step 4: Run all tests**

Run: `go test ./... -count=1`
Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/handler.go internal/webhook/handler_test.go internal/integration/testhelper_test.go
git commit -m "feat: add trigger channel to webhook handler for stock sync signals"
```

---

## Task 7: Wire Everything in main.go

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add syncer wiring**

In `cmd/server/main.go`, add import:

```go
"github.com/trismegistus0/rectella-shopify-service/internal/inventory"
```

After the SYSPRO client instantiation and before the batch processor, add:

```go
	// Set up stock sync (disabled gracefully if SYSPRO_SKUS is empty).
	triggerCh := make(chan struct{}, 1)
	var syncCancel context.CancelFunc

	if len(cfg.SysproSKUs) > 0 {
		if cfg.ShopifyAccessToken == "" {
			slog.Warn("SYSPRO_SKUS configured but SHOPIFY_ACCESS_TOKEN missing, stock sync disabled")
		} else if cfg.SysproWarehouse == "" {
			slog.Warn("SYSPRO_SKUS configured but SYSPRO_WAREHOUSE missing, stock sync disabled")
		} else {
			shopifyClient := inventory.NewShopifyClient(
				cfg.ShopifyStoreURL,
				cfg.ShopifyAccessToken,
				cfg.ShopifyLocationID,
				cfg.SysproSKUs,
				logger,
			)

			syncer := inventory.NewSyncer(
				sysproClient, // *EnetClient satisfies InventoryQuerier
				shopifyClient,
				db,
				cfg.StockSyncInterval,
				cfg.SysproWarehouse,
				cfg.SysproSKUs,
				triggerCh,
				logger,
			)

			var syncCtx context.Context
			syncCtx, syncCancel = context.WithCancel(ctx)
			go syncer.Run(syncCtx)
		}
	} else {
		slog.Warn("SYSPRO_SKUS not configured, stock sync disabled")
	}
```

Update the webhook handler to pass triggerCh:

```go
	wh := webhook.NewHandler(db, cfg.ShopifyWebhookSecret, triggerCh, logger)
```

In the shutdown section, add syncer drain (after batch processor drain):

```go
	if syncCancel != nil {
		slog.Info("draining stock syncer (10s grace period)")
		time.AfterFunc(10*time.Second, syncCancel)
	}
```

After `batchCancel()`, add:

```go
	if syncCancel != nil {
		syncCancel()
	}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./...`
Expected: clean build.

- [ ] **Step 3: Run all tests**

Run: `go test ./... -count=1`
Expected: all tests pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat: wire stock syncer into server with graceful shutdown"
```

---

## Task 8: Full Verification

- [ ] **Step 1: Run full test suite**

Run: `go test ./... -count=1`
Then: `go test -tags integration ./... -count=1`
Expected: all tests pass.

- [ ] **Step 2: Run checks**

Run: `go vet ./... && gofmt -l .`
Expected: no issues.

- [ ] **Step 3: Update CLAUDE.md**

Update "What's Built" to add stock sync. Update test count. Update file layout. Add stock sync config vars.

- [ ] **Step 4: Final commit**

```bash
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md with stock sync feature"
```

---

## Verification Summary

After all tasks complete:

```bash
# Full test suite
go test ./... -count=1                          # unit tests (~55+)
go test -tags integration ./... -count=1        # unit + integration (~70+)
go vet ./...                                    # static analysis
gofmt -l .                                      # formatting
```

**Expected new test count:** ~55-60 unit tests + 16 integration tests.

**New files:** 6 created, 9 modified.

**Zero new external dependencies.** Everything uses Go stdlib + existing pgx/v5.
