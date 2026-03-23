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
	if resp.WarehouseItem == nil {
		t.Fatal("expected WarehouseItem to be non-nil")
	}
	if resp.WarehouseItem.QtyAvailable != "120.000" {
		t.Errorf("expected QtyAvailable=120.000, got %q", resp.WarehouseItem.QtyAvailable)
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
  <QueryOptions><StockCode>UNKNOWN</StockCode><Description></Description></QueryOptions>
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
