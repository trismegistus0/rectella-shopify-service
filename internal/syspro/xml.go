package syspro

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

// sortoiParams maps to the <SalesOrders><Parameters>...</Parameters></SalesOrders> XML
// sent as XmlParameters on every SORTOI call.
//
// Canonical field set per CyberStore's production SORTOI XSLT template
// (https://documentation.cyberstoreforsyspro.com/ecommerce2023/SORTOI-Params.html)
// cross-referenced against `docs/reports/Claude.md:9-30`, which explicitly
// lists the SORTOI parameter enum and flags `ApplyIfEntireDocumentValid`
// as NOT a recognised SORTOI parameter (silently discarded — it belongs
// to SORTBO/INVTMA/CSHTWD/APSSSG). That field is intentionally absent.
//
// `AllocationAction` is the field that decides whether a line is Ship,
// Reserve, or Back-ordered at import time. Without it, SYSPRO's headless
// default is Back-order, which is exactly the "nothing on back order"
// failure Sarah flagged against our initial live orders (`docs/Sarah-Latest.md:15`).
type sortoiParams struct {
	XMLName                    xml.Name `xml:"SalesOrders"`
	Process                    string   `xml:"Parameters>Process"`
	StatusInProcess            string   `xml:"Parameters>StatusInProcess"`
	ValidateOnly               string   `xml:"Parameters>ValidateOnly"`
	IgnoreWarnings             string   `xml:"Parameters>IgnoreWarnings"`
	AllocationAction           string   `xml:"Parameters>AllocationAction,omitempty"`
	AcceptEarlierShipDate      string   `xml:"Parameters>AcceptEarlierShipDate,omitempty"`
	ShipFromDefaultBin         string   `xml:"Parameters>ShipFromDefaultBin,omitempty"`
	AlwaysUsePrice             string   `xml:"Parameters>AlwaysUsePriceEntered"`
	AllowZeroPrice             string   `xml:"Parameters>AllowZeroPrice"`
	AllowDuplicateOrderNumbers string   `xml:"Parameters>AllowDuplicateOrderNumbers,omitempty"`
	OrderStatus                string   `xml:"Parameters>OrderStatus,omitempty"`
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
	OrderDate         string `xml:"OrderDate"`         // YYYY-MM-DD
	RequestedShipDate string `xml:"RequestedShipDate"` // YYYY-MM-DD — Rectella internal reports sort by this
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
	FreightValue string `xml:"FreightValue"` // %.2f at construction (see sortoiStockLine.Price)
	FreightCost  string `xml:"FreightCost"`  // %.2f at construction
}

type sortoiStockLine struct {
	CustomerPoLine string `xml:"CustomerPoLine"`
	LineActionType string `xml:"LineActionType"`
	StockCode      string `xml:"StockCode"`
	Warehouse      string `xml:"Warehouse,omitempty"`
	OrderQty       int    `xml:"OrderQty"`
	OrderUom       string `xml:"OrderUom"`
	Price          string `xml:"Price"` // %.2f at construction — Go's float64 marshal outputs 15-digit precision on non-terminating decimals, which SYSPRO rejects as "not numeric" (BBQ1026 regression 2026-04-21)
	PriceUom       string `xml:"PriceUom"`
	TaxCode        string `xml:"StockTaxCode,omitempty"` // Per-line tax code override — requires "Allow changes to tax code" in Sales Order Setup
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
			Title             string `json:"title"`
			Code              string `json:"code"`
			CarrierIdentifier string `json:"carrier_identifier"`
			Source            string `json:"source"`
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

// extractTaxesIncluded returns the order-level `taxes_included` flag from the
// raw Shopify webhook payload. When true, line_items[].price is gross (VAT
// baked in) and we must strip VAT before emitting <Price> to SORTOI — SYSPRO's
// WEBS stock items have exclusive tax codes, so sending gross would cause
// SYSPRO to double-charge VAT. Returns false if the payload can't be parsed
// or the field is missing — leaving prices untouched, which is the safer
// default for the rare exclusive-pricing case.
func extractTaxesIncluded(rawPayload []byte) bool {
	if len(rawPayload) == 0 {
		return false
	}
	var p struct {
		TaxesIncluded bool `json:"taxes_included"`
	}
	if err := json.Unmarshal(rawPayload, &p); err != nil {
		return false
	}
	return p.TaxesIncluded
}

// extractShippingTax sums tax_lines[].price across all shipping_lines in the
// raw Shopify payload. Used to strip VAT from the freight line when the order
// is VAT-inclusive. Returns 0 if no shipping lines, no tax lines, or parse
// failure — all safe defaults (no strip).
func extractShippingTax(rawPayload []byte) float64 {
	if len(rawPayload) == 0 {
		return 0
	}
	var p struct {
		ShippingLines []struct {
			TaxLines []struct {
				Price string `json:"price"`
			} `json:"tax_lines"`
		} `json:"shipping_lines"`
	}
	if err := json.Unmarshal(rawPayload, &p); err != nil {
		return 0
	}
	var total float64
	for _, sl := range p.ShippingLines {
		for _, tl := range sl.TaxLines {
			if v, err := strconv.ParseFloat(tl.Price, 64); err == nil {
				total += v
			}
		}
	}
	return total
}

// extractLineRates pulls per-line tax rates from the raw Shopify payload.
// Returns a map of line index (0-based) → tax rate (e.g. 0.20, 0.05).
// When a line has multiple tax_lines, uses the first. Returns 0 for
// non-taxable lines or missing data.
func extractLineRates(rawPayload []byte) map[int]float64 {
	if len(rawPayload) == 0 {
		return nil
	}
	var p struct {
		LineItems []struct {
			TaxLines []struct {
				Rate float64 `json:"rate"`
			} `json:"tax_lines"`
		} `json:"line_items"`
	}
	if err := json.Unmarshal(rawPayload, &p); err != nil {
		return nil
	}
	rates := make(map[int]float64, len(p.LineItems))
	for i, li := range p.LineItems {
		if len(li.TaxLines) > 0 {
			rates[i] = li.TaxLines[0].Rate
		}
	}
	return rates
}

// taxCodeFromRate maps a Shopify VAT rate to a SYSPRO tax code letter.
// Uses the taxCodeMap if provided (from SYSPRO_TAX_CODE_MAP env var),
// otherwise falls back to UK defaults. Returns empty string if no
// mapping found — SORTOI will use the stock-master default.
func taxCodeFromRate(rate float64, taxCodeMap map[float64]string) string {
	if taxCodeMap != nil {
		if code, ok := taxCodeMap[rate]; ok {
			return code
		}
	}
	// Rectella confirmed codes: A=20% standard, B=5% reduced, Z=zero-rated
	switch {
	case rate >= 0.195 && rate <= 0.205:
		return "A"
	case rate >= 0.045 && rate <= 0.055:
		return "B"
	case rate < 0.005:
		return "Z"
	default:
		return ""
	}
}

// ParseTaxCodeMap parses "0.20:A,0.05:B,0.00:Z" into a rate→code map.
func ParseTaxCodeMap(s string) map[float64]string {
	if s == "" {
		return nil
	}
	m := make(map[float64]string)
	for _, pair := range strings.Split(s, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
		if len(parts) != 2 {
			continue
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil {
			continue
		}
		m[rate] = strings.TrimSpace(parts[1])
	}
	return m
}

// SYSPRO ShippingInstrs XSD limits (from sales-orders-reference-guide).
const (
	maxShippingInstrs    = 30
	maxShippingInstrsCod = 10
)

// buildSORTOI produces the two XML strings required by the SORTOI transaction call.
// `warehouse` is forced onto every stock line (empty string falls back to the
// stock-code default warehouse). `allocationAction` tells SYSPRO whether a
// short-of-stock line should ship, reserve, or back-order — pass "S" (Ship)
// for the common web-orders path where we've already verified on-hand stock
// via INVQRY. Empty string lets SYSPRO pick its own default, which is
// back-order on headless imports.
// Returns (paramsXML, dataXML, error).
func buildSORTOI(order model.Order, lines []model.OrderLine, warehouse, allocationAction string, taxCodeMap map[float64]string) (string, string, error) {
	params := sortoiParams{
		Process:                    "Import",
		StatusInProcess:            "N",
		ValidateOnly:               "N",
		IgnoreWarnings:             "W",
		AllocationAction:           allocationAction,
		AcceptEarlierShipDate:      "Y",
		ShipFromDefaultBin:         "Y",
		AlwaysUsePrice:             "Y",
		AllowZeroPrice:             "Y",
		AllowDuplicateOrderNumbers: "Y",
		OrderStatus:                "1",
	}
	paramsBytes, err := xml.Marshal(params)
	if err != nil {
		return "", "", fmt.Errorf("marshalling SORTOI params: %w", err)
	}

	// Shopify sends line_items[].price VAT-inclusive when taxes_included=true.
	// SYSPRO's WEBS stock items have exclusive tax codes, so sending gross
	// makes SYSPRO double-charge VAT. Strip per-line VAT when the payload
	// says prices are inclusive. Absolute subtraction (Shopify already gave
	// us the tax amount per line) avoids rounding drift from rate-based math.
	taxesIncluded := extractTaxesIncluded(order.RawPayload)
	lineRates := extractLineRates(order.RawPayload)

	stockLines := make([]sortoiStockLine, len(lines))
	for i, l := range lines {
		netPrice := l.UnitPrice
		if l.Discount > 0 && l.Quantity > 0 {
			netPrice -= l.Discount / float64(l.Quantity)
		}
		if taxesIncluded && l.Tax > 0 && l.Quantity > 0 {
			netPrice -= l.Tax / float64(l.Quantity)
		}
		// Override the SYSPRO tax code per line so SYSPRO applies the
		// same rate Shopify charged — not whatever the stock-master has.
		taxCode := taxCodeFromRate(lineRates[i], taxCodeMap)
		stockLines[i] = sortoiStockLine{
			CustomerPoLine: fmt.Sprintf("%04d", i+1),
			LineActionType: "A",
			StockCode:      l.SKU,
			Warehouse:      warehouse,
			OrderQty:       l.Quantity,
			OrderUom:       "EA",
			Price:          fmt.Sprintf("%.2f", netPrice),
			PriceUom:       "EA",
			TaxCode:        taxCode,
		}
	}

	shipTitle, shipCode := extractShippingInstrs(order.RawPayload)

	details := sortoiDetail{Lines: stockLines}
	if order.ShippingAmount > 0 {
		// Same VAT strip applies to freight — Shopify's shipping_lines[].price
		// is gross when taxes_included=true. Subtract the shipping tax total
		// so SYSPRO can add its own VAT back via the freight tax code.
		freightNet := order.ShippingAmount
		if taxesIncluded {
			freightNet -= extractShippingTax(order.RawPayload)
		}
		details.FreightLine = &sortoiFreightLine{
			FreightValue: fmt.Sprintf("%.2f", freightNet),
			FreightCost:  fmt.Sprintf("%.2f", freightNet),
		}
	}

	doc := sortoiDocument{
		Orders: sortoiOrder{
			Header: sortoiHeader{
				CustomerPoNumber:  truncate(order.OrderNumber, maxCustomerPoNumber),
				OrderActionType:   "A",
				Customer:          order.CustomerAccount,
				OrderDate:         order.OrderDate.Format("2006-01-02"),
				RequestedShipDate: order.OrderDate.Format("2006-01-02"),
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

// BuildSORTOI is the exported wrapper around buildSORTOI. Used by operator
// CLIs (cmd/resubmit-order) that need to produce the exact SORTOI params
// and data XML the batch processor would emit for a given order. Keep this
// a pure delegate — no extra logic — so CLI submissions are byte-identical
// to production.
func BuildSORTOI(order model.Order, lines []model.OrderLine, warehouse, allocationAction string, taxCodeMap map[float64]string) (string, string, error) {
	return buildSORTOI(order, lines, warehouse, allocationAction, taxCodeMap)
}
