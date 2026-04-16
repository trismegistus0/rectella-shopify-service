package syspro

import (
	"strings"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

func TestBuildSORTOI_ParamsXML(t *testing.T) {
	order := minimalOrder()
	paramsXML, _, err := buildSORTOI(order, nil, "WEBS", "A", nil)
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
	paramsXML, _, err := buildSORTOI(order, nil, "WEBS", "", nil)
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

	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := map[string]string{
		"CustomerPoNumber":  "<CustomerPoNumber>#BBQ1001</CustomerPoNumber>",
		"OrderActionType":   "<OrderActionType>A</OrderActionType>",
		"Customer":          "<Customer>WEBS01</Customer>",
		"OrderDate":         "<OrderDate>2026-02-24</OrderDate>",
		"RequestedShipDate": "<RequestedShipDate>2026-02-24</RequestedShipDate>",
		"Email":             "<Email>john@example.com</Email>",
		"ShipAddress1":      "<ShipAddress1>42 Bancroft Road</ShipAddress1>",
		"ShipAddress2":      "<ShipAddress2>Burnley</ShipAddress2>",
		"ShipAddress3":      "<ShipAddress3>Lancashire</ShipAddress3>",
		"ShipAddress4":      "<ShipAddress4>England</ShipAddress4>",
		"ShipAddress5":      "<ShipAddress5>UK</ShipAddress5>",
		"ShipPostalCode":    "<ShipPostalCode>BB10 2TP</ShipPostalCode>",
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

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
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
	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A", nil)
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

	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A", nil)
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

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
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

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
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
	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(dataXML, "FreightLine") {
		t.Errorf("data XML should not contain FreightLine when shipping is zero; got: %s", dataXML)
	}
}

// TestBuildSORTOI_DataXML_StripsVATWhenInclusive verifies Sarah's VAT fix:
// when Shopify's raw payload has taxes_included=true, buildSORTOI must
// subtract the per-line tax from the unit price before emitting <Price>.
// Shopify sends £8.00 gross with £1.33 VAT baked in; SYSPRO's exclusive
// tax code will add VAT back on top of the net figure we send.
func TestBuildSORTOI_DataXML_StripsVATAndSetsTaxCode(t *testing.T) {
	order := minimalOrder()
	order.RawPayload = []byte(`{"taxes_included":true,"line_items":[{"tax_lines":[{"rate":0.05}]}]}`)
	lines := []model.OrderLine{
		// £8 gross, £0.38 VAT (5%) → £7.62 net, tax code B
		{SKU: "BRIQ0152", Quantity: 1, UnitPrice: 8.00, Tax: 0.38},
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(dataXML, "<Price>7.62</Price>") {
		t.Errorf("expected <Price>7.62</Price> (8.00 - 0.38 VAT); got: %s", dataXML)
	}
	if !strings.Contains(dataXML, "<StockTaxCode>B</StockTaxCode>") {
		t.Errorf("expected <StockTaxCode>B</StockTaxCode> for 5%% rate; got: %s", dataXML)
	}
}

func TestBuildSORTOI_DataXML_TaxCodeA_For20Percent(t *testing.T) {
	order := minimalOrder()
	order.RawPayload = []byte(`{"taxes_included":true,"line_items":[{"tax_lines":[{"rate":0.20}]}]}`)
	lines := []model.OrderLine{
		{SKU: "MBBQ0159", Quantity: 1, UnitPrice: 149.00, Tax: 24.83},
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(dataXML, "<StockTaxCode>A</StockTaxCode>") {
		t.Errorf("expected <StockTaxCode>A</StockTaxCode> for 20%% rate; got: %s", dataXML)
	}
}

func TestBuildSORTOI_DataXML_MixedRateTaxCodes(t *testing.T) {
	order := minimalOrder()
	order.RawPayload = []byte(`{"taxes_included":true,"line_items":[{"tax_lines":[{"rate":0.05}]},{"tax_lines":[{"rate":0.20}]}]}`)
	lines := []model.OrderLine{
		{SKU: "BRIQ0152", Quantity: 1, UnitPrice: 8.00, Tax: 0.38},    // 5% → B
		{SKU: "MBBQ0159", Quantity: 1, UnitPrice: 149.00, Tax: 24.83}, // 20% → A
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(dataXML, "<StockTaxCode>B</StockTaxCode>") {
		t.Errorf("expected B for 5%% line; got: %s", dataXML)
	}
	if !strings.Contains(dataXML, "<StockTaxCode>A</StockTaxCode>") {
		t.Errorf("expected A for 20%% line; got: %s", dataXML)
	}
}

// TestBuildSORTOI_DataXML_PreservesNetWhenExclusive verifies the inverse:
// when taxes_included=false the line_items[].price is already net, so we
// must NOT strip VAT (would produce a negative result). This is the
// API-draft-order path that tolerates our existing test debris.
func TestBuildSORTOI_DataXML_PreservesNetWhenExclusive(t *testing.T) {
	order := minimalOrder()
	order.RawPayload = []byte(`{"taxes_included":false}`)
	lines := []model.OrderLine{
		{SKU: "BRIQ0152", Quantity: 1, UnitPrice: 8.00, Tax: 1.60},
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(dataXML, "<Price>8</Price>") {
		t.Errorf("expected <Price>8</Price> (price already net, no strip); got: %s", dataXML)
	}
}

// TestBuildSORTOI_DataXML_StripsFreightVAT verifies freight VAT is stripped
// the same way as line prices when taxes_included=true.
func TestBuildSORTOI_DataXML_StripsFreightVAT(t *testing.T) {
	order := minimalOrder()
	order.ShippingAmount = 5.99
	order.RawPayload = []byte(`{
		"taxes_included": true,
		"shipping_lines": [
			{"tax_lines": [{"price": "1.00"}]}
		]
	}`)
	lines := []model.OrderLine{
		{SKU: "BRIQ0152", Quantity: 1, UnitPrice: 8.00, Tax: 1.33},
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(dataXML, "<FreightValue>4.99</FreightValue>") {
		t.Errorf("expected <FreightValue>4.99</FreightValue> (5.99 - 1.00 VAT); got: %s", dataXML)
	}
	if !strings.Contains(dataXML, "<FreightCost>4.99</FreightCost>") {
		t.Errorf("expected <FreightCost>4.99</FreightCost>; got: %s", dataXML)
	}
}

// TestBuildSORTOI_DataXML_NonTaxableUnchanged verifies that when
// taxes_included=true but a specific line has Tax=0 (zero-rated item),
// the unit price is NOT modified — avoids false stripping of exempt lines.
func TestBuildSORTOI_DataXML_NonTaxableUnchanged(t *testing.T) {
	order := minimalOrder()
	order.RawPayload = []byte(`{"taxes_included":true}`)
	lines := []model.OrderLine{
		{SKU: "EXEMPT0001", Quantity: 2, UnitPrice: 15.00, Tax: 0},
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(dataXML, "<Price>15</Price>") {
		t.Errorf("expected <Price>15</Price> (tax=0, no strip); got: %s", dataXML)
	}
}

// TestBuildSORTOI_DataXML_MixedTaxableAndExempt verifies that on a single
// order with both a VAT-bearing line and a zero-rated line, each line is
// treated independently.
func TestBuildSORTOI_DataXML_MixedTaxableAndExempt(t *testing.T) {
	order := minimalOrder()
	order.RawPayload = []byte(`{"taxes_included":true}`)
	lines := []model.OrderLine{
		{SKU: "BRIQ0152", Quantity: 1, UnitPrice: 8.00, Tax: 1.33}, // taxable
		{SKU: "EXEMPT0001", Quantity: 1, UnitPrice: 15.00, Tax: 0}, // zero-rated
	}

	_, dataXML, err := buildSORTOI(order, lines, "WEBS", "A", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Taxable line stripped
	if !strings.Contains(dataXML, "<Price>6.67</Price>") {
		t.Errorf("expected <Price>6.67</Price> for taxable line; got: %s", dataXML)
	}
	// Zero-rated line untouched
	if !strings.Contains(dataXML, "<Price>15</Price>") {
		t.Errorf("expected <Price>15</Price> for zero-rated line; got: %s", dataXML)
	}
}

// TestExtractTaxesIncluded_MalformedPayload verifies the helper is
// defensive against malformed JSON — returning false is the safe default
// (preserves existing behaviour for any edge case).
func TestExtractTaxesIncluded_MalformedPayload(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("{"),
		[]byte(`{"taxes_included":"not-a-bool"}`),
	}
	for _, c := range cases {
		if extractTaxesIncluded(c) {
			t.Errorf("expected false for malformed payload %q, got true", string(c))
		}
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

	_, dataXML, err := buildSORTOI(order, nil, "WEBS", "A", nil)
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
