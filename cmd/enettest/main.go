// Command enettest validates connectivity to SYSPRO e.net by performing
// a logon → logoff cycle. Run it while connected to the Rectella VPN.
//
// Usage:
//
//	export $(grep -v '^#' .env | xargs)
//	go run ./cmd/enettest
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
	password := os.Getenv("SYSPRO_PASSWORD") // blank password is valid
	companyID := requireEnv("SYSPRO_COMPANY_ID")

	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	ctx := context.Background()

	fmt.Printf("e.net URL:  %s\n", baseURL)
	fmt.Printf("Operator:   %s\n", operator)
	fmt.Printf("Company ID: %s\n", companyID)
	fmt.Println()

	// Step 1: Logon
	fmt.Print("Logon... ")
	guid, err := logon(ctx, client, baseURL, operator, password, companyID)
	if err != nil {
		return fmt.Errorf("logon failed: %w", err)
	}
	fmt.Printf("OK (GUID: %s)\n", guid)

	// Step 2: Logoff
	fmt.Print("Logoff... ")
	if err := logoff(ctx, client, baseURL, guid); err != nil {
		return fmt.Errorf("logoff failed: %w", err)
	}
	fmt.Println("OK")

	fmt.Println()
	fmt.Println("SUCCESS: e.net connectivity verified.")
	return nil
}

func logon(ctx context.Context, client *http.Client, baseURL, operator, password, companyID string) (string, error) {
	params := url.Values{
		"Operator":         {operator},
		"OperatorPassword": {password},
		"CompanyId":        {companyID},
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
	params := url.Values{
		"UserId": {guid},
	}
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
