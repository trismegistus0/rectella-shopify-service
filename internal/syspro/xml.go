package syspro

import (
	"encoding/json"
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
	Warehouse      string  `xml:"Warehouse,omitempty"` // Forces allocation from this warehouse; omitted uses stock code default
	OrderQty       int     `xml:"OrderQty"`
	OrderUom       string  `xml:"OrderUom"`
	Price          float64 `xml:"Price"`
	PriceUom       string  `xml:"PriceUom"`
}

// SYSPRO 8 SORTOI field length limits (from sales-orders-reference-guide.pdf).
// Exceeding these causes SORTOI to either truncate silently, reject the line,
// or reject the whole order depending on the field. We truncate proactively
// so a long Shopify customer address never turns into a launch-day failed
// order requiring manual intervention.
const (
	maxCustomerPoNumber = 30 // SYSPRO CustomerPoNumber
	maxAddressLine      = 40 // ShipAddress1-5 each
	maxPostcode         = 15 // ShipPostalCode
	maxEmail            = 80 // Email field
)

// truncate returns s trimmed to at most n bytes. Uses byte-length for safety
// with SYSPRO's XSD limits (which count bytes, not UTF-8 code points).
// Non-ASCII characters are accepted but the byte budget is enforced.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// extractShippingInstrs pulls the first shipping-line title and carrier code
// (if present) out of the raw Shopify webhook payload. Returns empty strings
// if the payload can't be parsed or the order had no shipping lines.
// SYSPRO's ShippingInstrs XSD limit is ~30 chars; carrier code limit is shorter
// so callers should truncate before emitting.
func extractShippingInstrs(rawPayload []byte) (title string, code string) {
	if len(rawPayload) == 0 {
		return "", ""
	}
	var p struct {
		ShippingLines []struct {
			Title              string `json:"title"`
			Code               string `json:"code"`
			CarrierIdentifier  string `json:"carrier_identifier"`
			Source             string `json:"source"`
		} `json:"shipping_lines"`
	}
	if err := json.Unmarshal(rawPayload, &p); err != nil {
		return "", ""
	}
	if len(p.ShippingLines) == 0 {
		return "", ""
	}
	sl := p.ShippingLines[0]
	title = sl.Title
	// Prefer the explicit carrier_identifier, fall back to `code` (some
	// gateways populate only one of the two).
	code = sl.CarrierIdentifier
	if code == "" {
		code = sl.Code
	}
	return title, code
}

// SYSPRO ShippingInstrs XSD limits (from sales-orders-reference-guide).
const (
	maxShippingInstrs    = 30
	maxShippingInstrsCod = 10
)

// buildSORTOI produces the two XML strings required by the SORTOI transaction call.
// `warehouse` is forced onto every line (empty string falls back to stock-code default).
// Returns (paramsXML, dataXML, error).
func buildSORTOI(order model.Order, lines []model.OrderLine, warehouse string) (string, string, error) {
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
			Warehouse:      warehouse,
			OrderQty:       l.Quantity,
			OrderUom:       "EA",
			Price:          netPrice,
			PriceUom:       "EA",
		}
	}

	shipTitle, shipCode := extractShippingInstrs(order.RawPayload)

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
				CustomerPoNumber:  truncate(order.OrderNumber, maxCustomerPoNumber),
				OrderActionType:   "A",
				Customer:          order.CustomerAccount,
				OrderDate:         order.OrderDate.Format("2006-01-02"),
				Email:             truncate(order.ShipEmail, maxEmail),
				ShippingInstrs:    truncate(shipTitle, maxShippingInstrs),
				ShippingInstrsCod: truncate(shipCode, maxShippingInstrsCod),
				ShipAddress1:      truncate(order.ShipAddress1, maxAddressLine),
				ShipAddress2:      truncate(order.ShipAddress2, maxAddressLine),
				ShipAddress3:      truncate(order.ShipCity, maxAddressLine),
				ShipAddress4:      truncate(order.ShipProvince, maxAddressLine),
				ShipAddress5:      truncate(order.ShipCountry, maxAddressLine),
				ShipPostalCode:    truncate(order.ShipPostcode, maxPostcode),
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
