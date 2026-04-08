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

// sampleINVQRYResponse matches the real SYSPRO RILT response format:
// multiple WarehouseItem elements, AvailableQty (not QtyAvailable).
const sampleINVQRYResponse = `<InvQuery>
  <QueryOptions>
    <StockCode>MBBQ0159</StockCode>
    <Description>Bar-Be-Quick MBBQ Pizza Kettle</Description>
  </QueryOptions>
  <WarehouseItem>
    <Warehouse>AAAA</Warehouse>
    <QtyOnHand>            0.000000</QtyOnHand>
    <AvailableQty>            0.000000</AvailableQty>
  </WarehouseItem>
  <WarehouseItem>
    <Warehouse>BURN</Warehouse>
    <QtyOnHand>           75.000000</QtyOnHand>
    <AvailableQty>           75.000000</AvailableQty>
  </WarehouseItem>
</InvQuery>`

func TestParseINVQRY_Success(t *testing.T) {
	resp, err := parseINVQRY(sampleINVQRYResponse)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.QueryOptions.StockCode != "MBBQ0159" {
		t.Errorf("expected StockCode=MBBQ0159, got %q", resp.QueryOptions.StockCode)
	}
	if len(resp.WarehouseItems) != 2 {
		t.Fatalf("expected 2 WarehouseItems, got %d", len(resp.WarehouseItems))
	}
	burn := resp.WarehouseItems[1]
	if strings.TrimSpace(burn.AvailableQty) != "75.000000" {
		t.Errorf("expected AvailableQty=75.000000, got %q", burn.AvailableQty)
	}
}

func TestParseINVQRY_Windows1252(t *testing.T) {
	xml1252 := `<?xml version="1.0" encoding="Windows-1252"?>` + sampleINVQRYResponse
	resp, err := parseINVQRY(xml1252)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.WarehouseItems) != 2 {
		t.Fatalf("expected 2 WarehouseItems, got %d", len(resp.WarehouseItems))
	}
}

func TestParseINVQRY_EmptyWarehouse(t *testing.T) {
	emptyResp := `<InvQuery>
  <QueryOptions><StockCode>UNKNOWN</StockCode><Description></Description></QueryOptions>
</InvQuery>`
	resp, err := parseINVQRY(emptyResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.WarehouseItems) != 0 {
		t.Errorf("expected 0 WarehouseItems for unknown stock code, got %d", len(resp.WarehouseItems))
	}
}

func TestParseINVQRY_InvalidXML(t *testing.T) {
	_, err := parseINVQRY("<broken>")
	if err == nil {
		t.Fatal("expected error for invalid XML, got nil")
	}
}

// realINVQRYResponse is a trimmed version of the actual SYSPRO RILT response for
// CBBQ0001. It includes the XML declaration with Windows-1252 encoding,
// QueryOptions with StockCode/Description, and multiple WarehouseItem entries
// (AAAA, BQUR, BURN) matching what the production system returns.
const realINVQRYResponse = `<?xml version="1.0" encoding="Windows-1252"?>
<InvQuery>
  <QueryOptions>
    <StockCode>CBBQ0001</StockCode>
    <Description>Bar-Be-Quick Instant BBQ - Standard</Description>
  </QueryOptions>
  <WarehouseItem>
    <Warehouse>AAAA</Warehouse>
    <QtyOnHand>            0.000000</QtyOnHand>
    <AvailableQty>            0.000000</AvailableQty>
  </WarehouseItem>
  <WarehouseItem>
    <Warehouse>BQUR</Warehouse>
    <QtyOnHand>            0.000000</QtyOnHand>
    <AvailableQty>            0.000000</AvailableQty>
  </WarehouseItem>
  <WarehouseItem>
    <Warehouse>BURN</Warehouse>
    <QtyOnHand>          240.000000</QtyOnHand>
    <AvailableQty>          240.000000</AvailableQty>
  </WarehouseItem>
</InvQuery>`

func TestParseINVQRY_RealResponse(t *testing.T) {
	resp, err := parseINVQRY(realINVQRYResponse)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.QueryOptions.StockCode != "CBBQ0001" {
		t.Errorf("expected StockCode=CBBQ0001, got %q", resp.QueryOptions.StockCode)
	}
	if resp.QueryOptions.Description != "Bar-Be-Quick Instant BBQ - Standard" {
		t.Errorf("unexpected Description: %q", resp.QueryOptions.Description)
	}
	if len(resp.WarehouseItems) != 3 {
		t.Fatalf("expected 3 WarehouseItems (AAAA, BQUR, BURN), got %d", len(resp.WarehouseItems))
	}
	// Verify BURN warehouse has the right AvailableQty.
	var burnFound bool
	for _, wh := range resp.WarehouseItems {
		if strings.TrimSpace(wh.Warehouse) == "BURN" {
			burnFound = true
			qty := strings.TrimSpace(wh.AvailableQty)
			if qty != "240.000000" {
				t.Errorf("BURN AvailableQty: expected 240.000000, got %q", qty)
			}
			qtyOnHand := strings.TrimSpace(wh.QtyOnHand)
			if qtyOnHand != "240.000000" {
				t.Errorf("BURN QtyOnHand: expected 240.000000, got %q", qtyOnHand)
			}
		}
	}
	if !burnFound {
		t.Error("BURN warehouse not found in response")
	}
	// Verify zero-stock warehouses parse correctly.
	for _, wh := range resp.WarehouseItems {
		name := strings.TrimSpace(wh.Warehouse)
		if name == "AAAA" || name == "BQUR" {
			qty := strings.TrimSpace(wh.AvailableQty)
			if qty != "0.000000" {
				t.Errorf("%s AvailableQty: expected 0.000000, got %q", name, qty)
			}
		}
	}
}

func TestParseINVQRY_MultiWarehouse(t *testing.T) {
	// Response with multiple warehouses. Only BURN matches our filter.
	// The parser should return all warehouses; the caller (QueryStock) filters.
	multiResp := `<InvQuery>
  <QueryOptions>
    <StockCode>CBBQ0005</StockCode>
    <Description>Bar-Be-Quick Party BBQ</Description>
  </QueryOptions>
  <WarehouseItem>
    <Warehouse>AAAA</Warehouse>
    <QtyOnHand>           10.000000</QtyOnHand>
    <AvailableQty>           10.000000</AvailableQty>
  </WarehouseItem>
  <WarehouseItem>
    <Warehouse>BURN</Warehouse>
    <QtyOnHand>          350.000000</QtyOnHand>
    <AvailableQty>          325.000000</AvailableQty>
  </WarehouseItem>
  <WarehouseItem>
    <Warehouse>BQUR</Warehouse>
    <QtyOnHand>           50.000000</QtyOnHand>
    <AvailableQty>           42.000000</AvailableQty>
  </WarehouseItem>
</InvQuery>`

	resp, err := parseINVQRY(multiResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.WarehouseItems) != 3 {
		t.Fatalf("expected 3 WarehouseItems, got %d", len(resp.WarehouseItems))
	}

	// Simulate the warehouse filtering that QueryStock does: find BURN.
	warehouse := "BURN"
	var found *invqryWarehouse
	for i := range resp.WarehouseItems {
		if strings.TrimSpace(resp.WarehouseItems[i].Warehouse) == warehouse {
			found = &resp.WarehouseItems[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("warehouse %q not found in response", warehouse)
	}
	qty := strings.TrimSpace(found.AvailableQty)
	if qty != "325.000000" {
		t.Errorf("BURN AvailableQty: expected 325.000000, got %q", qty)
	}
	// Ensure non-matching warehouses have different values (parser didn't mix them up).
	for _, wh := range resp.WarehouseItems {
		name := strings.TrimSpace(wh.Warehouse)
		avail := strings.TrimSpace(wh.AvailableQty)
		switch name {
		case "AAAA":
			if avail != "10.000000" {
				t.Errorf("AAAA AvailableQty: expected 10.000000, got %q", avail)
			}
		case "BQUR":
			if avail != "42.000000" {
				t.Errorf("BQUR AvailableQty: expected 42.000000, got %q", avail)
			}
		}
	}
}

func TestQueryStock_Success(t *testing.T) {
	fake := newFakeEnet(t)
	fake.queryResponses["MBBQ0159"] = sampleINVQRYResponse
	fake.queryResponses["MBBQ0160"] = `<InvQuery>
  <QueryOptions><StockCode>MBBQ0160</StockCode><Description>BBQ Starter</Description></QueryOptions>
  <WarehouseItem>
    <Warehouse>BURN</Warehouse>
    <QtyOnHand>50.000</QtyOnHand>
    <AvailableQty>45.000</AvailableQty>
  </WarehouseItem>
</InvQuery>`
	c := fake.client(t)
	result, err := c.QueryStock(context.Background(), []string{"MBBQ0159", "MBBQ0160"}, "BURN")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if result["MBBQ0159"] != 75.0 {
		t.Errorf("MBBQ0159: expected 75.0, got %f", result["MBBQ0159"])
	}
	if result["MBBQ0160"] != 45.0 {
		t.Errorf("MBBQ0160: expected 45.0, got %f", result["MBBQ0160"])
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
	fake.queryResponses["MBBQ0159"] = sampleINVQRYResponse
	c := fake.client(t)
	result, err := c.QueryStock(context.Background(), []string{"MBBQ0159", "MBBQ0160"}, "BURN")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result (partial), got %d", len(result))
	}
	if result["MBBQ0159"] != 75.0 {
		t.Errorf("MBBQ0159: expected 75.0, got %f", result["MBBQ0159"])
	}
}

func TestQueryStock_LogonFailure(t *testing.T) {
	fake := newFakeEnet(t)
	fake.logonErr = true
	c := fake.client(t)
	_, err := c.QueryStock(context.Background(), []string{"MBBQ0159"}, "BURN")
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
	result, err := c.QueryStock(context.Background(), []string{"MBBQ0159"}, "BURN")
	if err != nil {
		t.Fatalf("unexpected error (query errors are per-SKU): %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty results on query error, got %d", len(result))
	}
}
