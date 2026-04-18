// Command arspaytest probes the SYSPRO 8 ARSTPY (Post AR Payments,
// Adjustments and Miscellaneous Receipts) business object.
//
// Naming gotcha: ARSTPY is the BUSINESS OBJECT name (the .dll on disk
// and the value passed as `BusinessObject=`). ARSPAY is the UI program
// name and is NOT a valid e.net BO — calling BusinessObject=ARSPAY
// returns `e.net exception 100000`. See docs/handover.md §5 for the
// full naming explanation.
//
// Two modes:
//
//	go run ./cmd/arspaytest --mode=validate          # ValidateOnly=Y dry run (no commit)
//	go run ./cmd/arspaytest --mode=post              # Actually post a £0.01 receipt to RILT
//
// Default mode is validate. The post mode is hard-gated to RILT
// (refuses to run against RIL) — never test against the live company.
//
// Re-uses the production XML builder in internal/syspro/cash_receipt.go
// so the probe XML is identical to what the live syncer emits. If the
// probe lints clean against RILT, the production path will too.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	mode := flag.String("mode", "validate", "validate | post")
	customer := flag.String("customer", "WEBS01", "customer code")
	reference := flag.String("ref", fmt.Sprintf("#TEST-%d", time.Now().Unix()), "payment reference (Shopify order name)")
	amount := flag.Float64("amount", 0.01, "gross amount paid")
	bankCharges := flag.Float64("fee", 0.00, "bank/Shopify fee (gross - net)")
	cashBook := flag.String("cashbook", os.Getenv("ARSPAY_CASH_BOOK"), "SYSPRO cashbook code (Bank)")
	paymentType := flag.String("paymenttype", os.Getenv("ARSPAY_PAYMENT_TYPE"), "payment-method code (e.g. 01 / EF)")
	flag.Parse()

	if *cashBook == "" {
		return fmt.Errorf("--cashbook required (or set ARSPAY_CASH_BOOK)")
	}
	if *paymentType == "" {
		return fmt.Errorf("--paymenttype required (or set ARSPAY_PAYMENT_TYPE)")
	}

	baseURL := strings.TrimRight(requireEnv("SYSPRO_ENET_URL"), "/")
	operator := requireEnv("SYSPRO_OPERATOR")
	password := os.Getenv("SYSPRO_PASSWORD")
	companyID := requireEnv("SYSPRO_COMPANY_ID")
	companyPassword := os.Getenv("SYSPRO_COMPANY_PASSWORD")

	if *mode == "post" && companyID != "RILT" {
		return fmt.Errorf("refusing to --post against company %q (only RILT allowed)", companyID)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	ctx := context.Background()

	receipt := syspro.CashReceipt{
		CustomerCode:  *customer,
		Bank:          *cashBook,
		PaymentType:   *paymentType,
		InvoiceNumber: *reference,
		Amount:        *amount,
		BankCharges:   *bankCharges,
		Currency:      "GBP",
		PaymentMethod: "test",
		PostedAt:      time.Now().UTC(),
	}

	dataXML, err := syspro.BuildARSTPYData(receipt)
	if err != nil {
		return fmt.Errorf("building ARSTPY data: %w", err)
	}
	paramsXML, err := syspro.BuildARSTPYParams(*mode == "validate")
	if err != nil {
		return fmt.Errorf("building ARSTPY params: %w", err)
	}

	fmt.Printf("e.net URL:     %s\n", baseURL)
	fmt.Printf("Operator:      %s\n", operator)
	fmt.Printf("Company ID:    %s\n", companyID)
	fmt.Printf("Mode:          %s\n", *mode)
	fmt.Printf("Customer:      %s\n", receipt.CustomerCode)
	fmt.Printf("Bank:          %s\n", receipt.Bank)
	fmt.Printf("PaymentType:   %s\n", receipt.PaymentType)
	fmt.Printf("Reference:     %s\n", receipt.InvoiceNumber)
	fmt.Printf("Amount:        %.2f\n", receipt.Amount)
	fmt.Printf("BankCharges:   %.2f\n", receipt.BankCharges)
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

	fmt.Printf("\n=== ARSTPY PARAMS XML ===\n%s\n", paramsXML)
	fmt.Printf("\n=== ARSTPY DATA XML ===\n%s\n", dataXML)

	params := url.Values{
		"UserId":         {guid},
		"BusinessObject": {"ARSTPY"},
		"XmlParameters":  {paramsXML},
		"XmlIn":          {dataXML},
	}
	fmt.Print("\nSubmitting ARSTPY... ")
	respBody, err := doGet(ctx, client, baseURL+"/Transaction/Post", params)
	if err != nil {
		fmt.Println("FAIL")
		return fmt.Errorf("ARSTPY transaction failed: %w", err)
	}
	fmt.Println("OK")

	var xmlStr string
	if err := json.Unmarshal(respBody, &xmlStr); err != nil {
		xmlStr = string(respBody)
	}
	fmt.Printf("\n=== RAW ARSTPY RESPONSE (%d bytes) ===\n%s\n=== END RESPONSE ===\n", len(xmlStr), xmlStr)

	ref, perr := syspro.ParseARSTPYResponse(xmlStr)
	if perr != nil {
		fmt.Printf("\n--- Parsed: BUSINESS ERROR: %v\n", perr)
		return perr
	}
	if ref == "" {
		fmt.Printf("\n--- Parsed: SUCCESS (no receipt reference returned)\n")
	} else {
		fmt.Printf("\n--- Parsed: SUCCESS (receipt reference: %s)\n", ref)
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
	if strings.HasPrefix(guid, "ERROR") {
		return "", fmt.Errorf("SYSPRO logon error: %s", guid)
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
