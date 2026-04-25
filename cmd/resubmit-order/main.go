// Command resubmit-order pushes a single persisted order from the local
// Postgres through SYSPRO's SORTOI e.net business object and dumps the raw
// XML response. Used for one-off operator recovery of rows stuck in the
// silent "status=submitted, syspro_order_number=empty" phantom state that
// the original parseSORTOIResponse Case 3 could produce.
//
// The CLI never writes to the DB unless the response contains a non-empty
// <SalesOrder>NNNNNN</SalesOrder> AND the operator confirms at the prompt.
//
// Usage:
//
//	resubmit-order --order-id=41 [--validate-only] [--yes]
//
// Requires env: DATABASE_URL, SYSPRO_ENET_URL, SYSPRO_OPERATOR, SYSPRO_PASSWORD,
//
//	SYSPRO_COMPANY_ID, SYSPRO_WAREHOUSE, SYSPRO_COMPANY_PASSWORD (optional),
//	SYSPRO_ALLOCATION_ACTION (optional, default A),
//	SYSPRO_TAX_CODE_MAP (optional).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

type resubmitResponse struct {
	XMLName        xml.Name `xml:"SalesOrders"`
	SalesOrder     string   `xml:"Order>SalesOrder"`
	ItemsProcessed string   `xml:"StatusOfItems>ItemsProcessed"`
	ItemsInvalid   string   `xml:"StatusOfItems>ItemsRejectedWithWarnings"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	orderID := flag.Int64("order-id", 0, "orders.id to resubmit (required)")
	validateOnly := flag.Bool("validate-only", false, "send <ValidateOnly>Y</ValidateOnly> — SYSPRO parses but does not commit")
	yes := flag.Bool("yes", false, "skip the interactive confirmation prompt before DB write")
	flag.Parse()

	if *orderID == 0 {
		return fmt.Errorf("--order-id is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, requireEnv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("postgres connect: %w", err)
	}
	defer pool.Close()

	order, lines, err := loadOrder(ctx, pool, *orderID)
	if err != nil {
		return err
	}
	if order.Status != "submitted" || order.SysproOrderNumber != "" {
		return fmt.Errorf("refusing to resubmit: orders.id=%d is status=%q syspro_order_number=%q, expected status=submitted with empty syspro_order_number",
			order.ID, order.Status, order.SysproOrderNumber)
	}
	fmt.Printf("loaded order id=%d shopify_id=%d order_number=%s lines=%d\n",
		order.ID, order.ShopifyOrderID, order.OrderNumber, len(lines))

	warehouse := requireEnv("SYSPRO_WAREHOUSE")
	allocAction := os.Getenv("SYSPRO_ALLOCATION_ACTION")
	if allocAction == "" {
		allocAction = "A"
	}
	taxCodeMap := parseTaxCodeMap(os.Getenv("SYSPRO_TAX_CODE_MAP"))

	paramsXML, dataXML, err := syspro.BuildSORTOI(order, lines, warehouse, allocAction, taxCodeMap)
	if err != nil {
		return fmt.Errorf("build SORTOI: %w", err)
	}
	if *validateOnly {
		paramsXML = strings.Replace(paramsXML,
			"<ValidateOnly>N</ValidateOnly>",
			"<ValidateOnly>Y</ValidateOnly>", 1)
	}
	fmt.Println("\n=== SORTOI PARAMS ===\n" + paramsXML)
	fmt.Println("\n=== SORTOI DATA ===\n" + dataXML)

	baseURL := strings.TrimRight(requireEnv("SYSPRO_ENET_URL"), "/")
	httpClient := &http.Client{Timeout: 45 * time.Second}

	fmt.Print("\nLogon... ")
	guid, err := logon(ctx, httpClient, baseURL)
	if err != nil {
		return fmt.Errorf("logon: %w", err)
	}
	fmt.Printf("OK (GUID %s)\n", guid)
	defer func() {
		fmt.Print("Logoff... ")
		if err := logoff(ctx, httpClient, baseURL, guid); err != nil {
			fmt.Printf("WARN: %v\n", err)
			return
		}
		fmt.Println("OK")
	}()

	fmt.Print("Submitting SORTOI... ")
	raw, err := transact(ctx, httpClient, baseURL, guid, paramsXML, dataXML)
	if err != nil {
		return fmt.Errorf("transaction: %w", err)
	}
	fmt.Println("OK")
	fmt.Printf("\n=== RAW RESPONSE (%d bytes) ===\n%s\n=== END ===\n", len(raw), raw)

	if i := strings.Index(raw, "?>"); i != -1 {
		raw = strings.TrimSpace(raw[i+2:])
	}
	var r resubmitResponse
	if err := xml.Unmarshal([]byte(raw), &r); err != nil {
		return fmt.Errorf("parse response XML: %w", err)
	}
	fmt.Println("\n--- Extracted ---")
	fmt.Printf("  SalesOrder:     %q\n", r.SalesOrder)
	fmt.Printf("  ItemsProcessed: %q\n", r.ItemsProcessed)
	fmt.Printf("  ItemsInvalid:   %q\n", r.ItemsInvalid)

	if *validateOnly {
		fmt.Println("\n[--validate-only] No DB write. Re-run without the flag to commit for real.")
		return nil
	}
	if r.SalesOrder == "" {
		return fmt.Errorf("SYSPRO response had no <SalesOrder>NNNNNN</SalesOrder> — refusing to touch the DB")
	}

	if !*yes {
		fmt.Printf("\nUpdate orders.id=%d → syspro_order_number=%q, status=submitted, attempts+=1? [y/N] ",
			order.ID, r.SalesOrder)
		reader := bufio.NewReader(os.Stdin)
		ans, _ := reader.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(ans)) != "y" {
			fmt.Println("Aborted by operator.")
			return nil
		}
	}
	tag, err := pool.Exec(ctx,
		`UPDATE orders
		 SET syspro_order_number = $1,
		     status = 'submitted',
		     attempts = attempts + 1,
		     last_error = '',
		     updated_at = NOW()
		 WHERE id = $2 AND status = 'submitted' AND syspro_order_number = ''`,
		r.SalesOrder, order.ID,
	)
	if err != nil {
		return fmt.Errorf("UPDATE orders: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("expected 1 row updated, got %d — someone else may have touched the row", tag.RowsAffected())
	}
	fmt.Printf("OK — orders.id=%d now has syspro_order_number=%s. Fulfilment sync will pick it up within 30min.\n",
		order.ID, r.SalesOrder)
	return nil
}

func loadOrder(ctx context.Context, pool *pgxpool.Pool, id int64) (model.Order, []model.OrderLine, error) {
	var o model.Order
	err := pool.QueryRow(ctx, `
		SELECT id, shopify_order_id, order_number, status, customer_account,
		       ship_first_name, ship_last_name, ship_address1, ship_address2,
		       ship_city, ship_province, ship_postcode, ship_country,
		       ship_phone, ship_email,
		       payment_reference, payment_amount, shipping_amount,
		       raw_payload, syspro_order_number, attempts, last_error,
		       order_date, created_at, updated_at,
		       fulfilled_at, shopify_fulfillment_id
		FROM orders WHERE id = $1`, id).Scan(
		&o.ID, &o.ShopifyOrderID, &o.OrderNumber, &o.Status, &o.CustomerAccount,
		&o.ShipFirstName, &o.ShipLastName, &o.ShipAddress1, &o.ShipAddress2,
		&o.ShipCity, &o.ShipProvince, &o.ShipPostcode, &o.ShipCountry,
		&o.ShipPhone, &o.ShipEmail,
		&o.PaymentReference, &o.PaymentAmount, &o.ShippingAmount,
		&o.RawPayload, &o.SysproOrderNumber, &o.Attempts, &o.LastError,
		&o.OrderDate, &o.CreatedAt, &o.UpdatedAt,
		&o.FulfilledAt, &o.ShopifyFulfilmentID,
	)
	if err != nil {
		return model.Order{}, nil, fmt.Errorf("SELECT orders.id=%d: %w", id, err)
	}

	rows, err := pool.Query(ctx,
		`SELECT id, order_id, sku, quantity, unit_price, discount, tax
		 FROM order_lines WHERE order_id = $1 ORDER BY id`, o.ID)
	if err != nil {
		return model.Order{}, nil, fmt.Errorf("SELECT order_lines: %w", err)
	}
	defer rows.Close()
	var lines []model.OrderLine
	for rows.Next() {
		var l model.OrderLine
		if err := rows.Scan(&l.ID, &l.OrderID, &l.SKU, &l.Quantity, &l.UnitPrice, &l.Discount, &l.Tax); err != nil {
			return model.Order{}, nil, fmt.Errorf("scan order_line: %w", err)
		}
		lines = append(lines, l)
	}
	return o, lines, rows.Err()
}

func logon(ctx context.Context, c *http.Client, baseURL string) (string, error) {
	params := url.Values{
		"Operator":         {requireEnv("SYSPRO_OPERATOR")},
		"OperatorPassword": {os.Getenv("SYSPRO_PASSWORD")},
		"CompanyId":        {requireEnv("SYSPRO_COMPANY_ID")},
		"CompanyPassword":  {os.Getenv("SYSPRO_COMPANY_PASSWORD")},
	}
	body, err := doGet(ctx, c, baseURL+"/Logon", params)
	if err != nil {
		return "", err
	}
	var guid string
	if err := json.Unmarshal(body, &guid); err != nil {
		guid = strings.TrimSpace(string(body))
	}
	if strings.HasPrefix(guid, "ERROR") {
		return "", fmt.Errorf("logon rejected: %s", guid)
	}
	if guid == "" {
		return "", fmt.Errorf("logon returned empty GUID")
	}
	return guid, nil
}

func logoff(ctx context.Context, c *http.Client, baseURL, guid string) error {
	_, err := doGet(ctx, c, baseURL+"/Logoff", url.Values{"UserId": {guid}})
	return err
}

func transact(ctx context.Context, c *http.Client, baseURL, guid, paramsXML, dataXML string) (string, error) {
	params := url.Values{
		"UserId":         {guid},
		"BusinessObject": {"SORTOI"},
		"XmlParameters":  {paramsXML},
		"XmlIn":          {dataXML},
	}
	body, err := doGet(ctx, c, baseURL+"/Transaction/Post", params)
	if err != nil {
		return "", err
	}
	var xmlStr string
	if err := json.Unmarshal(body, &xmlStr); err != nil {
		xmlStr = string(body)
	}
	return strings.TrimSpace(xmlStr), nil
}

func doGet(ctx context.Context, c *http.Client, target string, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func parseTaxCodeMap(s string) map[float64]string {
	if s == "" {
		return nil
	}
	m := map[float64]string{}
	for _, pair := range strings.Split(s, ",") {
		parts := strings.Split(strings.TrimSpace(pair), ":")
		if len(parts) != 2 {
			continue
		}
		var rate float64
		if _, err := fmt.Sscanf(parts[0], "%f", &rate); err != nil {
			continue
		}
		m[rate] = strings.TrimSpace(parts[1])
	}
	return m
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required env var %s\n", key)
		os.Exit(1)
	}
	return v
}
