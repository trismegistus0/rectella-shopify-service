package syspro

import (
	"strings"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

func TestBuildSORTOI_ParamsXML(t *testing.T) {
	order := minimalOrder()
	paramsXML, _, err := buildSORTOI(order, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		"<Process>Import</Process>",
		"<StatusInProcess>N</StatusInProcess>",
		"<ValidateOnly>N</ValidateOnly>",
		"<IgnoreWarnings>W</IgnoreWarnings>",
		"<ApplyIfEntireDocumentValid>Y</ApplyIfEntireDocumentValid>",
		"<AlwaysUsePriceEntered>Y</AlwaysUsePriceEntered>",
		"<AllowZeroPrice>Y</AllowZeroPrice>",
	} {
		if !strings.Contains(paramsXML, want) {
			t.Errorf("params XML missing %q\ngot: %s", want, paramsXML)
		}
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

	_, dataXML, err := buildSORTOI(order, nil)
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

	_, dataXML, err := buildSORTOI(order, lines)
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
	_, dataXML, err := buildSORTOI(order, nil)
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

	_, dataXML, err := buildSORTOI(order, nil)
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

	_, dataXML, err := buildSORTOI(order, lines)
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

	_, dataXML, err := buildSORTOI(order, lines)
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
	_, dataXML, err := buildSORTOI(order, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(dataXML, "FreightLine") {
		t.Errorf("data XML should not contain FreightLine when shipping is zero; got: %s", dataXML)
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
