package syspro

import (
	"encoding/xml"
	"fmt"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

// sortoiParams maps to the <SalesOrders><Parameters>...</Parameters></SalesOrders> XML
// sent as XmlParameters on every SORTOI call.
type sortoiParams struct {
	XMLName                    xml.Name `xml:"SalesOrders"`
	Process                    string   `xml:"Parameters>Process"`
	StatusInProcess            string   `xml:"Parameters>StatusInProcess"`
	ValidateOnly               string   `xml:"Parameters>ValidateOnly"`
	IgnoreWarnings             string   `xml:"Parameters>IgnoreWarnings"`
	ApplyIfEntireDocumentValid string   `xml:"Parameters>ApplyIfEntireDocumentValid"`
	AlwaysUsePrice             string   `xml:"Parameters>AlwaysUsePriceEntered"`
	AllowZeroPrice             string   `xml:"Parameters>AllowZeroPrice"`
}

// sortoiDocument maps to the <SalesOrders><Orders>...</Orders></SalesOrders> XML
// sent as XmlIn on a SORTOI call.
type sortoiDocument struct {
	XMLName xml.Name    `xml:"SalesOrders"`
	Orders  sortoiOrder `xml:"Orders"`
}

type sortoiOrder struct {
	Header  sortoiHeader `xml:"OrderHeader"`
	Details sortoiDetail `xml:"OrderDetails"`
}

type sortoiHeader struct {
	CustomerPoNumber  string `xml:"CustomerPoNumber"`
	OrderActionType   string `xml:"OrderActionType"`
	Customer          string `xml:"Customer"`
	OrderDate         string `xml:"OrderDate"` // YYYY-MM-DD
	Email             string `xml:"Email,omitempty"`
	ShippingInstrs    string `xml:"ShippingInstrs,omitempty"`    // Carrier from Shopify shipping method
	ShippingInstrsCod string `xml:"ShippingInstrsCod,omitempty"` // Carrier code if available
	// Ship-to address from Shopify (overrides customer default when populated)
	ShipAddress1   string `xml:"ShipAddress1,omitempty"`
	ShipAddress2   string `xml:"ShipAddress2,omitempty"`
	ShipAddress3   string `xml:"ShipAddress3,omitempty"` // City
	ShipAddress4   string `xml:"ShipAddress4,omitempty"` // Province
	ShipAddress5   string `xml:"ShipAddress5,omitempty"` // Country
	ShipPostalCode string `xml:"ShipPostalCode,omitempty"`
}

type sortoiDetail struct {
	Lines       []sortoiStockLine  `xml:"StockLine"`
	FreightLine *sortoiFreightLine `xml:"FreightLine,omitempty"`
}

type sortoiFreightLine struct {
	FreightValue float64 `xml:"FreightValue"`
	FreightCost  float64 `xml:"FreightCost"`
}

type sortoiStockLine struct {
	CustomerPoLine string  `xml:"CustomerPoLine"`
	LineActionType string  `xml:"LineActionType"`
	StockCode      string  `xml:"StockCode"`
	OrderQty       int     `xml:"OrderQty"`
	OrderUom       string  `xml:"OrderUom"`
	Price          float64 `xml:"Price"`
	PriceUom       string  `xml:"PriceUom"`
}

// buildSORTOI produces the two XML strings required by the SORTOI transaction call.
// Returns (paramsXML, dataXML, error).
func buildSORTOI(order model.Order, lines []model.OrderLine) (string, string, error) {
	params := sortoiParams{
		Process:                    "Import",
		StatusInProcess:            "N",
		ValidateOnly:               "N",
		IgnoreWarnings:             "W",
		ApplyIfEntireDocumentValid: "Y",
		AlwaysUsePrice:             "Y",
		AllowZeroPrice:             "Y",
	}
	paramsBytes, err := xml.Marshal(params)
	if err != nil {
		return "", "", fmt.Errorf("marshalling SORTOI params: %w", err)
	}

	stockLines := make([]sortoiStockLine, len(lines))
	for i, l := range lines {
		// Net price: subtract per-unit discount (Shopify sends total discount across all units).
		netPrice := l.UnitPrice
		if l.Discount > 0 && l.Quantity > 0 {
			netPrice -= l.Discount / float64(l.Quantity)
		}
		stockLines[i] = sortoiStockLine{
			CustomerPoLine: fmt.Sprintf("%04d", i+1),
			LineActionType: "A",
			StockCode:      l.SKU,
			OrderQty:       l.Quantity,
			OrderUom:       "EA",
			Price:          netPrice,
			PriceUom:       "EA",
		}
	}

	details := sortoiDetail{Lines: stockLines}
	if order.ShippingAmount > 0 {
		details.FreightLine = &sortoiFreightLine{
			FreightValue: order.ShippingAmount,
			FreightCost:  order.ShippingAmount,
		}
	}

	doc := sortoiDocument{
		Orders: sortoiOrder{
			Header: sortoiHeader{
				CustomerPoNumber: order.OrderNumber,
				OrderActionType:  "A",
				Customer:         order.CustomerAccount,
				OrderDate:        order.OrderDate.Format("2006-01-02"),
				Email:            order.ShipEmail,
				ShipAddress1:     order.ShipAddress1,
				ShipAddress2:     order.ShipAddress2,
				ShipAddress3:     order.ShipCity,
				ShipAddress4:     order.ShipProvince,
				ShipAddress5:     order.ShipCountry,
				ShipPostalCode:   order.ShipPostcode,
			},
			Details: details,
		},
	}
	dataBytes, err := xml.Marshal(doc)
	if err != nil {
		return "", "", fmt.Errorf("marshalling SORTOI document: %w", err)
	}

	return string(paramsBytes), string(dataBytes), nil
}
