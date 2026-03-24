package syspro

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"
)

func TestBuildSORQRY(t *testing.T) {
	xmlStr, err := buildSORQRY("000100")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xmlStr, "<SalesOrder>000100</SalesOrder>") {
		t.Errorf("expected SalesOrder in XML, got: %s", xmlStr)
	}
	for _, tag := range []string{
		"<IncludeStockedLines>N</IncludeStockedLines>",
		"<IncludeNonStockedLines>N</IncludeNonStockedLines>",
		"<IncludeFreightLines>N</IncludeFreightLines>",
		"<IncludeMiscLines>N</IncludeMiscLines>",
		"<IncludeCommentLines>N</IncludeCommentLines>",
	} {
		if !strings.Contains(xmlStr, tag) {
			t.Errorf("expected %s in XML, got: %s", tag, xmlStr)
		}
	}
}

func TestBuildSORQRY_RoundTrip(t *testing.T) {
	xmlStr, err := buildSORQRY("000100")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var req sorqryRequest
	if err := xml.Unmarshal([]byte(xmlStr), &req); err != nil {
		t.Fatalf("round-trip unmarshal failed: %v", err)
	}
	if req.Key.SalesOrder != "000100" {
		t.Errorf("expected SalesOrder=000100, got %q", req.Key.SalesOrder)
	}
	if req.Option.IncludeStockedLines != "N" {
		t.Errorf("expected IncludeStockedLines=N, got %q", req.Option.IncludeStockedLines)
	}
}

// sampleSORQRYResponse matches the real SYSPRO RILT response format:
// flat structure under <SorDetail>, fields directly on root element.
const sampleSORQRYResponse = `<SorDetail>
<SalesOrder>000100</SalesOrder>
<OrderStatus>9</OrderStatus>
<OrderStatusDesc>Complete</OrderStatusDesc>
<ShippingInstrs>Avanti</ShippingInstrs>
<ShippingInstrsCod>AVA</ShippingInstrsCod>
<LastInvoice>200069</LastInvoice>
</SorDetail>`

func TestParseSORQRY_Success(t *testing.T) {
	result, err := parseSORQRY(sampleSORQRYResponse)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SalesOrder != "000100" {
		t.Errorf("expected SalesOrder=000100, got %q", result.SalesOrder)
	}
	if result.OrderStatus != "9" {
		t.Errorf("expected OrderStatus=9, got %q", result.OrderStatus)
	}
	if result.Carrier != "Avanti" {
		t.Errorf("expected Carrier=Avanti, got %q", result.Carrier)
	}
}

func TestParseSORQRY_Windows1252(t *testing.T) {
	xml1252 := `<?xml version="1.0" encoding="Windows-1252"?>` + sampleSORQRYResponse
	result, err := parseSORQRY(xml1252)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SalesOrder != "000100" {
		t.Errorf("expected SalesOrder=000100, got %q", result.SalesOrder)
	}
	if result.OrderStatus != "9" {
		t.Errorf("expected OrderStatus=9, got %q", result.OrderStatus)
	}
}

func TestParseSORQRY_InvalidXML(t *testing.T) {
	_, err := parseSORQRY("<broken>")
	if err == nil {
		t.Fatal("expected error for invalid XML, got nil")
	}
}

func TestQueryDispatchedOrders_Success(t *testing.T) {
	fake := newFakeEnet(t)
	fake.queryResponses["000100"] = sampleSORQRYResponse
	fake.queryResponses["000101"] = `<SorDetail>
<SalesOrder>000101</SalesOrder>
<OrderStatus>2</OrderStatus>
<OrderStatusDesc>Open</OrderStatusDesc>
<ShippingInstrs></ShippingInstrs>
</SorDetail>`
	c := fake.client(t)

	result, err := c.QueryDispatchedOrders(context.Background(), []string{"000100", "000101"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if result["000100"].OrderStatus != "9" {
		t.Errorf("000100: expected OrderStatus=9, got %q", result["000100"].OrderStatus)
	}
	if result["000100"].Carrier != "Avanti" {
		t.Errorf("000100: expected Carrier=Avanti, got %q", result["000100"].Carrier)
	}
	if result["000101"].OrderStatus != "2" {
		t.Errorf("000101: expected OrderStatus=2, got %q", result["000101"].OrderStatus)
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

func TestQueryDispatchedOrders_PartialFailure(t *testing.T) {
	fake := newFakeEnet(t)
	fake.queryResponses["000100"] = sampleSORQRYResponse
	// 000101 has no response configured, so fakeEnet returns the default INVQRY-shaped XML
	// which will fail to parse as SORQRY (no SalesOrder field).
	c := fake.client(t)

	result, err := c.QueryDispatchedOrders(context.Background(), []string{"000100", "000101"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result (partial), got %d", len(result))
	}
	if result["000100"].OrderStatus != "9" {
		t.Errorf("000100: expected OrderStatus=9, got %q", result["000100"].OrderStatus)
	}
}

func TestQueryDispatchedOrders_LogonFailure(t *testing.T) {
	fake := newFakeEnet(t)
	fake.logonErr = true
	c := fake.client(t)

	_, err := c.QueryDispatchedOrders(context.Background(), []string{"000100"})
	if err == nil {
		t.Fatal("expected error on logon failure, got nil")
	}
	if !strings.Contains(err.Error(), "syspro logon") {
		t.Errorf("error should mention logon, got: %v", err)
	}
}
