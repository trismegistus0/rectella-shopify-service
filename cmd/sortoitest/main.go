// Command sortoitest submits a test sales order to SYSPRO via SORTOI and
// dumps the full raw response. Use this to debug whether orders are actually
// being committed.
//
// Usage:
//
//	go run ./cmd/sortoitest [--post]
//
// Without --post: uses current params (no PostSalesOrders flag)
// With --post:    adds PostSalesOrders Y to params
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	baseURL := requireEnv("SYSPRO_ENET_URL")
	operator := requireEnv("SYSPRO_OPERATOR")
	password := os.Getenv("SYSPRO_PASSWORD")
	companyID := requireEnv("SYSPRO_COMPANY_ID")
	companyPassword := os.Getenv("SYSPRO_COMPANY_PASSWORD")
	warehouse := os.Getenv("SYSPRO_WAREHOUSE")
	if warehouse == "" {
		warehouse = "WEBS"
	}

	addPostFlag := false
	for _, arg := range os.Args[1:] {
		if arg == "--post" {
			addPostFlag = true
		}
	}

	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 30 * time.Second}
	ctx := context.Background()

	fmt.Printf("e.net URL:     %s\n", baseURL)
	fmt.Printf("Operator:      %s\n", operator)
	fmt.Printf("Company ID:    %s\n", companyID)
	fmt.Printf("Warehouse:     %s\n", warehouse)
	fmt.Printf("AllocAction:   %s\n", os.Getenv("ALLOC_ACTION"))
	fmt.Printf("PostFlag:      %v\n", addPostFlag)
	fmt.Println()

	fmt.Print("Logon... ")
	guid, err := logon(ctx, client, baseURL, operator, password, companyID, companyPassword)
	if err != nil {
		return fmt.Errorf("logon failed: %w", err)
	}
	fmt.Printf("OK (GUID: %s)\n", guid)

	defer func() {
		fmt.Print("\nLogoff... ")
		if err := logoff(ctx, client, baseURL, guid); err != nil {
			fmt.Printf("WARN: logoff failed: %v\n", err)
		} else {
			fmt.Println("OK")
		}
	}()

	poRef := fmt.Sprintf("#TEST-%d", time.Now().Unix())

	// Canonical SORTOI params per CyberStore's production template
	// (https://documentation.cyberstoreforsyspro.com/ecommerce2023/SORTOI-Params.html).
	// `ApplyIfEntireDocumentValid` intentionally NOT emitted — it's not a real
	// SORTOI parameter (silently discarded, docs/reports/Claude.md:32).
	// `AllocationAction` is the field that decides Ship vs Reserve vs Backorder;
	// override via ALLOC_ACTION env var for iterative probes.
	allocAction := os.Getenv("ALLOC_ACTION")
	if allocAction == "" {
		allocAction = "S"
	}

	paramsXML := `<SalesOrders><Parameters><Process>Import</Process><StatusInProcess>N</StatusInProcess><ValidateOnly>N</ValidateOnly><IgnoreWarnings>W</IgnoreWarnings>`
	paramsXML += `<AllocationAction>` + allocAction + `</AllocationAction>`
	paramsXML += `<AcceptEarlierShipDate>Y</AcceptEarlierShipDate><ShipFromDefaultBin>Y</ShipFromDefaultBin>`
	paramsXML += `<AlwaysUsePriceEntered>Y</AlwaysUsePriceEntered><AllowZeroPrice>Y</AllowZeroPrice>`
	paramsXML += `<AllowDuplicateOrderNumbers>Y</AllowDuplicateOrderNumbers><OrderStatus>1</OrderStatus>`
	if addPostFlag {
		paramsXML += `<PostSalesOrders>Y</PostSalesOrders>`
	}
	paramsXML += `</Parameters></SalesOrders>`

	dataXML := fmt.Sprintf(`<SalesOrders><Orders><OrderHeader><CustomerPoNumber>%s</CustomerPoNumber><OrderActionType>A</OrderActionType><Customer>WEBS01</Customer><OrderDate>%s</OrderDate><Email>test@example.com</Email><ShippingInstrs>Standard UK Delivery</ShippingInstrs><ShippingInstrsCod>STANDARD</ShippingInstrsCod><ShipAddress1>Bancroft Road</ShipAddress1><ShipAddress2>Unit 7</ShipAddress2><ShipAddress3>Burnley</ShipAddress3><ShipAddress4>Lancashire</ShipAddress4><ShipAddress5>United Kingdom</ShipAddress5><ShipPostalCode>BB10 2TP</ShipPostalCode></OrderHeader><OrderDetails><StockLine><CustomerPoLine>0001</CustomerPoLine><LineActionType>A</LineActionType><StockCode>BRIQ0152</StockCode><Warehouse>%s</Warehouse><OrderQty>1</OrderQty><OrderUom>EA</OrderUom><Price>8.00</Price><PriceUom>EA</PriceUom></StockLine></OrderDetails></Orders></SalesOrders>`,
		poRef,
		time.Now().Format("2006-01-02"),
		warehouse,
	)

	fmt.Printf("\n=== SORTOI PARAMS XML ===\n%s\n", paramsXML)
	fmt.Printf("\n=== SORTOI DATA XML ===\n%s\n", dataXML)

	fmt.Print("\nSubmitting SORTOI... ")
	params := url.Values{
		"UserId":         {guid},
		"BusinessObject": {"SORTOI"},
		"XmlParameters":  {paramsXML},
		"XmlIn":          {dataXML},
	}
	respBody, err := doGet(ctx, client, baseURL+"/Transaction/Post", params)
	if err != nil {
		return fmt.Errorf("SORTOI transaction failed: %w", err)
	}
	fmt.Println("OK")

	var xmlStr string
	if err := json.Unmarshal(respBody, &xmlStr); err != nil {
		xmlStr = string(respBody)
	}

	fmt.Printf("\n=== RAW SORTOI RESPONSE (%d bytes) ===\n%s\n=== END RESPONSE ===\n", len(xmlStr), xmlStr)

	fmt.Println("\n--- Quick Analysis ---")
	for _, field := range []string{"SalesOrder", "ValidationStatus", "ItemsProcessed", "ItemsInvalid", "CustomerPoNumber"} {
		start := strings.Index(xmlStr, "<"+field+">")
		if start == -1 {
			fmt.Printf("  %-20s NOT FOUND\n", field+":")
			continue
		}
		end := strings.Index(xmlStr[start:], "</"+field+">")
		if end == -1 {
			fmt.Printf("  %-20s MALFORMED\n", field+":")
			continue
		}
		value := xmlStr[start+len(field)+2 : start+end]
		if value == "" {
			value = "(empty)"
		}
		fmt.Printf("  %-20s %s\n", field+":", value)
	}

	return nil
}

func logon(ctx context.Context, client *http.Client, baseURL, operator, password, companyID, companyPassword string) (string, error) {
	params := url.Values{
		"Operator":         {operator},
		"OperatorPassword": {password},
		"CompanyId":        {companyID},
		"CompanyPassword":  {companyPassword},
	}
	body, err := doGet(ctx, client, baseURL+"/Logon", params)
	if err != nil {
		return "", err
	}
	var guid string
	if err := json.Unmarshal(body, &guid); err != nil {
		guid = strings.TrimSpace(string(body))
	}
	if guid == "" {
		return "", fmt.Errorf("logon returned empty GUID (body: %s)", string(body))
	}
	return guid, nil
}

func logoff(ctx context.Context, client *http.Client, baseURL, guid string) error {
	params := url.Values{"UserId": {guid}}
	_, err := doGet(ctx, client, baseURL+"/Logoff", params)
	return err
}

func doGet(ctx context.Context, client *http.Client, target string, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "FAIL: missing required env var %s\n", key)
		os.Exit(1)
	}
	return v
}
