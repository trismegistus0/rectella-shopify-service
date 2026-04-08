package syspro

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type invqryRequest struct {
	XMLName xml.Name     `xml:"Query"`
	Key     invqryKey    `xml:"Key"`
	Option  invqryOption `xml:"Option"`
}

type invqryKey struct {
	StockCode string `xml:"StockCode"`
}

type invqryOption struct {
	WarehouseFilterType  string `xml:"WarehouseFilterType"`
	WarehouseFilterValue string `xml:"WarehouseFilterValue"`
}

type invqryResponse struct {
	XMLName        xml.Name          `xml:"InvQuery"`
	QueryOptions   invqryOptions     `xml:"QueryOptions"`
	WarehouseItems []invqryWarehouse `xml:"WarehouseItem"`
}

type invqryOptions struct {
	StockCode   string `xml:"StockCode"`
	Description string `xml:"Description"`
}

type invqryWarehouse struct {
	Warehouse    string `xml:"Warehouse"`
	QtyOnHand    string `xml:"QtyOnHand"`
	AvailableQty string `xml:"AvailableQty"` // SYSPRO uses AvailableQty, not QtyAvailable
}

func buildINVQRY(sku, warehouse string) (string, error) {
	req := invqryRequest{
		Key: invqryKey{StockCode: sku},
		Option: invqryOption{
			WarehouseFilterType:  "S",
			WarehouseFilterValue: warehouse,
		},
	}
	b, err := xml.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshalling INVQRY request: %w", err)
	}
	return string(b), nil
}

func parseINVQRY(xmlStr string) (*invqryResponse, error) {
	if i := strings.Index(xmlStr, "?>"); i != -1 {
		xmlStr = strings.TrimSpace(xmlStr[i+2:])
	}
	var resp invqryResponse
	if err := xml.Unmarshal([]byte(xmlStr), &resp); err != nil {
		return nil, fmt.Errorf("parsing INVQRY response: %w", err)
	}
	return &resp, nil
}

// QueryStock queries SYSPRO for stock levels of the given SKUs in the specified
// warehouse. Returns a map of SKU -> available quantity. SKUs that fail
// individually are logged and skipped (partial results returned).
func (c *EnetClient) QueryStock(ctx context.Context, skus []string, warehouse string) (map[string]float64, error) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()

	guid, err := c.logon(ctx)
	if err != nil {
		return nil, fmt.Errorf("syspro logon: %w", err)
	}
	defer func() {
		if lerr := c.logoff(ctx, guid); lerr != nil {
			c.logger.Warn("syspro logoff failed", "error", lerr)
		}
	}()

	result := make(map[string]float64, len(skus))
	for _, sku := range skus {
		queryCtx, queryCancel := context.WithTimeout(ctx, 10*time.Second)
		xmlIn, err := buildINVQRY(sku, warehouse)
		if err != nil {
			c.logger.Warn("building INVQRY request", "sku", sku, "error", err)
			queryCancel()
			continue
		}
		respXML, err := c.query(queryCtx, guid, "INVQRY", xmlIn)
		queryCancel()
		if err != nil {
			c.logger.Warn("INVQRY query failed", "sku", sku, "error", err)
			continue
		}
		resp, err := parseINVQRY(respXML)
		if err != nil {
			c.logger.Warn("parsing INVQRY response", "sku", sku, "error", err)
			continue
		}
		// SYSPRO returns all warehouses even with filter type "S".
		// Find the matching warehouse in the response.
		var found *invqryWarehouse
		for i := range resp.WarehouseItems {
			if strings.TrimSpace(resp.WarehouseItems[i].Warehouse) == warehouse {
				found = &resp.WarehouseItems[i]
				break
			}
		}
		if found == nil {
			c.logger.Warn("stock code not found in warehouse", "sku", sku, "warehouse", warehouse)
			continue
		}
		qty, err := strconv.ParseFloat(strings.TrimSpace(found.AvailableQty), 64)
		if err != nil {
			c.logger.Warn("parsing AvailableQty", "sku", sku, "value", found.AvailableQty, "error", err)
			continue
		}
		result[sku] = qty
	}
	return result, nil
}
