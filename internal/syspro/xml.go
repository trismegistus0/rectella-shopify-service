package syspro

import (
	"encoding/xml"
	"fmt"

	"codeberg.org/speeder091/rectella-shopify-service/internal/model"
)

// sortoiParams maps to the <SalesOrders><Parameters>...</Parameters></SalesOrders> XML
// sent as XmlParameters on every SORTOI call.
type sortoiParams struct {
	XMLName        xml.Name `xml:"SalesOrders"`
	IgnoreWarnings string   `xml:"Parameters>IgnoreWarnings"`
	AlwaysUsePrice string   `xml:"Parameters>AlwaysUsePriceEntered"`
	AllowZeroPrice string   `xml:"Parameters>AllowZeroPrice"`
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
	Customer          string `xml:"Customer"`
	OrderDate         string `xml:"OrderDate"` // YYYY-MM-DD
	Email             string `xml:"Email,omitempty"`
	ShippingInstrs    string `xml:"ShippingInstrs,omitempty"`    // Carrier from Shopify shipping method
	ShippingInstrsCod string `xml:"ShippingInstrsCod,omitempty"` // Carrier code if available
	// Ship2 = delivery address from Shopify (not ShipAddress which is the customer default)
	Ship2Address1   string `xml:"Ship2Address1,omitempty"`
	Ship2Address2   string `xml:"Ship2Address2,omitempty"`
	Ship2Address3   string `xml:"Ship2Address3,omitempty"` // City
	Ship2Address4   string `xml:"Ship2Address4,omitempty"` // Province
	Ship2Address5   string `xml:"Ship2Address5,omitempty"` // Country
	Ship2PostalCode string `xml:"Ship2PostalCode,omitempty"`
}

type sortoiDetail struct {
	Lines []sortoiStockLine `xml:"StockLine"`
}

type sortoiStockLine struct {
	CustomerPoLine string  `xml:"CustomerPoLine"`
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
		IgnoreWarnings: "Y",
		AlwaysUsePrice: "Y",
		AllowZeroPrice: "Y",
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
			StockCode:      l.SKU,
			OrderQty:       l.Quantity,
			OrderUom:       "EA",
			Price:          netPrice,
			PriceUom:       "EA",
		}
	}

	doc := sortoiDocument{
		Orders: sortoiOrder{
			Header: sortoiHeader{
				CustomerPoNumber: order.OrderNumber,
				Customer:         order.CustomerAccount,
				OrderDate:        order.OrderDate.Format("2006-01-02"),
				Email:            order.ShipEmail,
				Ship2Address1:    order.ShipAddress1,
				Ship2Address2:    order.ShipAddress2,
				Ship2Address3:    order.ShipCity,
				Ship2Address4:    order.ShipProvince,
				Ship2Address5:    order.ShipCountry,
				Ship2PostalCode:  order.ShipPostcode,
			},
			Details: sortoiDetail{Lines: stockLines},
		},
	}
	dataBytes, err := xml.Marshal(doc)
	if err != nil {
		return "", "", fmt.Errorf("marshalling SORTOI document: %w", err)
	}

	return string(paramsBytes), string(dataBytes), nil
}
