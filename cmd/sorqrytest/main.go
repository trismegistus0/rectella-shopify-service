// Command sorqrytest queries SYSPRO e.net for sales order dispatch status
// using the SORQRY business object. Prints the raw XML response so we can
// verify the response format matches our parser.
//
// Usage:
//
//	go run ./cmd/sorqrytest ORDER_NUMBER
//
// Also tests INVQRY if SYSPRO_SKUS is set.
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

	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 30 * time.Second}
	ctx := context.Background()

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: go run ./cmd/sorqrytest ORDER_NUMBER")
		fmt.Fprintln(os.Stderr, "  e.g. go run ./cmd/sorqrytest 001234")
		os.Exit(1)
	}
	orderNumber := os.Args[1]

	fmt.Printf("e.net URL:    %s\n", baseURL)
	fmt.Printf("Operator:     %s\n", operator)
	fmt.Printf("Company ID:   %s\n", companyID)
	fmt.Printf("Order Number: %s\n", orderNumber)
	fmt.Println()

	// Step 1: Logon
	fmt.Print("Logon... ")
	guid, err := logon(ctx, client, baseURL, operator, password, companyID, companyPassword)
	if err != nil {
		return fmt.Errorf("logon failed: %w", err)
	}
	if strings.HasPrefix(guid, "ERROR") {
		return fmt.Errorf("logon returned error: %s", guid)
	}
	fmt.Printf("OK (GUID: %s)\n", guid)

	defer func() {
		fmt.Print("Logoff... ")
		if err := logoff(ctx, client, baseURL, guid); err != nil {
			fmt.Printf("WARN: logoff failed: %v\n", err)
		} else {
			fmt.Println("OK")
		}
	}()

	// Step 2: Query SORQRY — default to header-only, but accept INCLUDE_LINES=Y
	// to also pull stocked / non-stocked / freight lines + totals.
	includeFlag := "N"
	if os.Getenv("INCLUDE_LINES") == "Y" {
		includeFlag = "Y"
	}
	xmlIn := fmt.Sprintf(`<Query><Key><SalesOrder>%s</SalesOrder></Key><Option><IncludeStockedLines>%s</IncludeStockedLines><IncludeNonStockedLines>%s</IncludeNonStockedLines><IncludeFreightLines>%s</IncludeFreightLines><IncludeMiscLines>N</IncludeMiscLines><IncludeCommentLines>N</IncludeCommentLines></Option></Query>`, orderNumber, includeFlag, includeFlag, includeFlag)

	fmt.Printf("\nSORQRY request:\n%s\n\n", xmlIn)
	fmt.Print("Querying SORQRY... ")

	params := url.Values{
		"UserId":         {guid},
		"BusinessObject": {"SORQRY"},
		"XmlIn":          {xmlIn},
	}
	respBody, err := doGet(ctx, client, baseURL+"/Query/Query", params)
	if err != nil {
		return fmt.Errorf("SORQRY query failed: %w", err)
	}
	fmt.Println("OK")

	fmt.Printf("\n=== RAW SORQRY RESPONSE (%d bytes) ===\n", len(respBody))
	fmt.Println(string(respBody))
	fmt.Println("=== END RESPONSE ===")

	// Step 3: Also try INVQRY if a SKU and warehouse are available
	if skus := os.Getenv("SYSPRO_SKUS"); skus != "" {
		warehouse := os.Getenv("SYSPRO_WAREHOUSE")
		if warehouse == "" {
			warehouse = "WH01"
		}
		sku := strings.Split(skus, ",")[0]
		sku = strings.TrimSpace(sku)

		fmt.Printf("\n--- Bonus: Testing INVQRY for SKU %q in warehouse %q ---\n", sku, warehouse)
		invXml := fmt.Sprintf(`<Query><Key><StockCode>%s</StockCode></Key><Option><WarehouseFilterType>S</WarehouseFilterType><WarehouseFilterValue>%s</WarehouseFilterValue></Option></Query>`, sku, warehouse)

		invParams := url.Values{
			"UserId":         {guid},
			"BusinessObject": {"INVQRY"},
			"XmlIn":          {invXml},
		}
		invResp, err := doGet(ctx, client, baseURL+"/Query/Query", invParams)
		if err != nil {
			fmt.Printf("INVQRY failed: %v\n", err)
		} else {
			fmt.Printf("\n=== RAW INVQRY RESPONSE (%d bytes) ===\n", len(invResp))
			fmt.Println(string(invResp))
			fmt.Println("=== END RESPONSE ===")
		}
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
