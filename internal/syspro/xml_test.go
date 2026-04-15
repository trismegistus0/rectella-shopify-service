package syspro

import (
	"strings"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

func TestBuildSORTOI_ParamsXML(t *testing.T) {
	order := minimalOrder()
	paramsXML, _, err := buildSORTOI(order, nil, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		"<Process>Import</Process>",
		"<StatusInProcess>N</StatusInProcess>",
		"<ValidateOnly>N</ValidateOnly>",
		"<IgnoreWarnings>W</IgnoreWarnings>",
		"<AllocationAction>A</AllocationAction>",
		"<AcceptEarlierShipDate>Y</AcceptEarlierShipDate>",
		"<ShipFromDefaultBin>Y</ShipFromDefaultBin>",
		"<AlwaysUsePriceEntered>Y</AlwaysUsePriceEntered>",
		"<AllowZeroPrice>Y</AllowZeroPrice>",
		"<AllowDuplicateOrderNumbers>Y</AllowDuplicateOrderNumbers>",
		"<OrderStatus>1</OrderStatus>",
	} {
		if !strings.Contains(paramsXML, want) {
			t.Errorf("params XML missing %q\ngot: %s", want, paramsXML)
		}
	}

	// ApplyIfEntireDocumentValid is NOT a SORTOI parameter (docs/reports/Claude.md:32).
	// It must not appear in the rendered XML.
	if strings.Contains(paramsXML, "ApplyIfEntireDocumentValid") {
		t.Errorf("params XML contains spurious ApplyIfEntireDocumentValid; got: %s", paramsXML)
	}
}

func TestBuildSORTOI_AllocationActionOmittedWhenEmpty(t *testing.T) {
	order := minimalOrder()
	paramsXML, _, err := buildSORTOI(order, nil, "WEBS", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(paramsXML, "<AllocationAction>") {
		t.Errorf("empty allocationAction should omit the element; got: %s", paramsXML)
	}
}

func TestBuildSORTOI_DataXML_HeaderFields(t *testing.T) {
	order := model.Order{
		OrderNumber:     "#BBQ1001",
		CustomerAccount: "WEBS01",
		OrderDate:       time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC),
		ShipEmail:       "john@example.com",
		ShipAddress1:    "42 Bancroft Road",
		ShipAddress2:    "Burnley",
		ShipCity:        "Lancashire",
		ShipProvince:    "England",
		ShipCountry:     "UK",
		ShipPostcode:    "BB10 2TP",
	}

	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := map[string]string{
		"CustomerPoNumber": "<CustomerPoNumber>#BBQ1001</CustomerPoNumber>",
		"OrderActionType":  "<OrderActionType>A</OrderActionType>",
		"Customer":         "<Customer>WEBS01</Customer>",
		"OrderDate":        "<OrderDate>2026-02-24</OrderDate>",
		"Email":            "<Email>john@example.com</Email>",
		"ShipAddress1":     "<ShipAddress1>42 Bancroft Road</ShipAddress1>",
		"ShipAddress2":     "<ShipAddress2>Burnley</ShipAddress2>",
		"ShipAddress3":     "<ShipAddress3>Lancashire</ShipAddress3>",
		"ShipAddress4":     "<ShipAddress4>England</ShipAddress4>",
		"ShipAddress5":     "<ShipAddress5>UK</ShipAddress5>",
		"ShipPostalCode":   "<ShipPostalCode>BB10 2TP</ShipPostalCode>",
	}
	for field, want := range checks {
		if !strings.Contains(dataXML, want) {
			t.Errorf("data XML missing %s field: want %q\ngot: %s", field, want, dataXML)
		}
	}
}

func TestBuildSORTOI_DataXML_StockLines(t *testing.T) {
	order := minimalOrder()
	lines := []model.OrderLine{
		{SKU: "CBBQ0001", Quantity: 2, UnitPrice: 599.00},
		{SKU: "CBBQ0002", Quantity: 1, UnitPrice: 12.50},
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		"<CustomerPoLine>0001</CustomerPoLine>",
		"<LineActionType>A</LineActionType>",
		"<StockCode>CBBQ0001</StockCode>",
		"<OrderQty>2</OrderQty>",
		"<OrderUom>EA</OrderUom>",
		"<Price>599</Price>",
		"<PriceUom>EA</PriceUom>",
		"<CustomerPoLine>0002</CustomerPoLine>",
		"<StockCode>CBBQ0002</StockCode>",
		"<OrderQty>1</OrderQty>",
		"<Price>12.5</Price>",
	} {
		if !strings.Contains(dataXML, want) {
			t.Errorf("data XML missing %q\ngot: %s", want, dataXML)
		}
	}
}

func TestBuildSORTOI_EmptyAddressOmitted(t *testing.T) {
	order := minimalOrder()
	// No address fields set
	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, absent := range []string{"ShipAddress1", "ShipAddress2", "ShipAddress3", "Email"} {
		if strings.Contains(dataXML, "<"+absent+">") {
			t.Errorf("data XML should omit empty field <%s>; got: %s", absent, dataXML)
		}
	}
}

func TestBuildSORTOI_SpecialCharsEscaped(t *testing.T) {
	order := minimalOrder()
	order.ShipAddress1 = `Foo & Bar <Baz> "Qux"`

	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(dataXML, "<Baz>") {
		t.Errorf("XML should escape angle brackets; got: %s", dataXML)
	}
	if !strings.Contains(dataXML, "&amp;") && !strings.Contains(dataXML, "&#") {
		t.Errorf("XML should escape & character; got: %s", dataXML)
	}
}

func TestBuildSORTOI_DataXML_NetPriceAfterDiscount(t *testing.T) {
	order := minimalOrder()
	lines := []model.OrderLine{
		// 10% off: £20 unit price, £4 total discount across 2 units = £18/unit net
		{SKU: "CBBQ0001", Quantity: 2, UnitPrice: 20.00, Discount: 4.00},
		// No discount: price unchanged
		{SKU: "CBBQ0002", Quantity: 1, UnitPrice: 12.50, Discount: 0},
		// Full line discount on single unit: £5 off £15 = £10 net
		{SKU: "CBBQ0003", Quantity: 1, UnitPrice: 15.00, Discount: 5.00},
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Net prices: 18.00, 12.50, 10.00
	for _, want := range []string{
		"<Price>18</Price>",
		"<Price>12.5</Price>",
		"<Price>10</Price>",
	} {
		if !strings.Contains(dataXML, want) {
			t.Errorf("data XML missing net price %q\ngot: %s", want, dataXML)
		}
	}

	// Gross prices should NOT appear
	for _, absent := range []string{
		"<Price>20</Price>",
		"<Price>15</Price>",
	} {
		if strings.Contains(dataXML, absent) {
			t.Errorf("data XML should contain net price, not gross; found %q\ngot: %s", absent, dataXML)
		}
	}
}

func TestBuildSORTOI_DataXML_FreightLine(t *testing.T) {
	order := minimalOrder()
	order.ShippingAmount = 5.99
	lines := []model.OrderLine{
		{SKU: "CBBQ0001", Quantity: 1, UnitPrice: 599.00},
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		"<FreightLine>",
		"<FreightValue>5.99</FreightValue>",
		"<FreightCost>5.99</FreightCost>",
		"</FreightLine>",
	} {
		if !strings.Contains(dataXML, want) {
			t.Errorf("data XML missing %q\ngot: %s", want, dataXML)
		}
	}
}

func TestBuildSORTOI_DataXML_NoFreightWhenZero(t *testing.T) {
	order := minimalOrder()
	// ShippingAmount is zero (default)
	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(dataXML, "FreightLine") {
		t.Errorf("data XML should not contain FreightLine when shipping is zero; got: %s", dataXML)
	}
}

// TestBuildSORTOI_TruncatesLongFields verifies that customer data exceeding
// SYSPRO SORTOI field length limits is silently truncated to the maximum,
// rather than being sent as-is and rejected by SYSPRO. This protects against
// real-world Shopify customer data where names, street names, city names,
// and international postcodes routinely exceed SORTOI's schema limits.
func TestBuildSORTOI_TruncatesLongFields(t *testing.T) {
	order := model.Order{
		OrderNumber:     "#THIS-IS-AN-INCREDIBLY-LONG-SHOPIFY-ORDER-NUMBER-THAT-SYSPRO-WILL-REJECT",
		CustomerAccount: "WEBS01",
		OrderDate:       time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC),
		ShipEmail:       strings.Repeat("a", 100) + "@example.com",
		ShipAddress1:    "42 Extremely Long Road That Some Customers Definitely Have Near Manchester",
		ShipAddress2:    "Flat 9, Third Building Round The Back Past The Blue Door With The Knocker",
		ShipCity:        "Kingston upon Hull, East Riding of Yorkshire",
		ShipProvince:    "Greater Manchester Metropolitan County",
		ShipCountry:     "United Kingdom of Great Britain and Northern Ireland",
		ShipPostcode:    "SOME-VERY-LONG-INTERNATIONAL-POSTCODE-12345",
	}

	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Extract each field and check byte length.
	checks := []struct {
		tag    string
		maxLen int
	}{
		{"CustomerPoNumber", maxCustomerPoNumber},
		{"Email", maxEmail},
		{"ShipAddress1", maxAddressLine},
		{"ShipAddress2", maxAddressLine},
		{"ShipAddress3", maxAddressLine},
		{"ShipAddress4", maxAddressLine},
		{"ShipAddress5", maxAddressLine},
		{"ShipPostalCode", maxPostcode},
	}
	for _, c := range checks {
		open := "<" + c.tag + ">"
		close := "</" + c.tag + ">"
		start := strings.Index(dataXML, open)
		end := strings.Index(dataXML, close)
		if start == -1 || end == -1 || end <= start {
			t.Errorf("%s tag not found in XML", c.tag)
			continue
		}
		val := dataXML[start+len(open) : end]
		if len(val) > c.maxLen {
			t.Errorf("%s = %q (%d bytes) exceeds max %d", c.tag, val, len(val), c.maxLen)
		}
	}
}

// TestTruncate verifies the helper directly.
func TestTruncate(t *testing.T) {
	cases := []struct {
		in  string
		n   int
		out string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"hello world", 5, "hello"},
		{"exact", 5, "exact"},
	}
	for _, tc := range cases {
		if got := truncate(tc.in, tc.n); got != tc.out {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.out)
		}
	}
}

// minimalOrder returns an Order with only the mandatory fields set.
func minimalOrder() model.Order {
	return model.Order{
		OrderNumber:     "#BBQ1001",
		CustomerAccount: "WEBS01",
		OrderDate:       time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC),
	}
}
