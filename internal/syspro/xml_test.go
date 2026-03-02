package syspro

import (
	"strings"
	"testing"
	"time"

	"codeberg.org/speeder091/rectella-shopify-service/internal/model"
)

func TestBuildSORTOI_ParamsXML(t *testing.T) {
	order := minimalOrder()
	paramsXML, _, err := buildSORTOI(order, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		"<IgnoreWarnings>Y</IgnoreWarnings>",
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
		"<StockCode>CBBQ0001</StockCode>",
		"<OrderQty>2</OrderQty>",
		"<Price>599</Price>",
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

// minimalOrder returns an Order with only the mandatory fields set.
func minimalOrder() model.Order {
	return model.Order{
		OrderNumber:     "#BBQ1001",
		CustomerAccount: "WEBS01",
		OrderDate:       time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC),
	}
}
