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
	CustomerPoNumber string `xml:"CustomerPoNumber"`
	Customer         string `xml:"Customer"`
	OrderDate        string `xml:"OrderDate"` // YYYY-MM-DD
	Email            string `xml:"Email,omitempty"`
	ShipAddress1     string `xml:"ShipAddress1,omitempty"`
	ShipAddress2     string `xml:"ShipAddress2,omitempty"`
	ShipAddress3     string `xml:"ShipAddress3,omitempty"` // City
	ShipAddress4     string `xml:"ShipAddress4,omitempty"` // Province
	ShipAddress5     string `xml:"ShipAddress5,omitempty"` // Country
	ShipPostalCode   string `xml:"ShipPostalCode,omitempty"`
}

type sortoiDetail struct {
	Lines []sortoiStockLine `xml:"StockLine"`
}

type sortoiStockLine struct {
	StockCode string  `xml:"StockCode"`
	OrderQty  int     `xml:"OrderQty"`
	Price     float64 `xml:"Price"`
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
		stockLines[i] = sortoiStockLine{
			StockCode: l.SKU,
			OrderQty:  l.Quantity,
			Price:     l.UnitPrice,
		}
	}

	doc := sortoiDocument{
		Orders: sortoiOrder{
			Header: sortoiHeader{
				CustomerPoNumber: order.OrderNumber,
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
			Details: sortoiDetail{Lines: stockLines},
		},
	}
	dataBytes, err := xml.Marshal(doc)
	if err != nil {
		return "", "", fmt.Errorf("marshalling SORTOI document: %w", err)
	}

	return string(paramsBytes), string(dataBytes), nil
}
