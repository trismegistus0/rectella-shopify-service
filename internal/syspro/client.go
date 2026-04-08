package syspro

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codeberg.org/speeder091/rectella-shopify-service/internal/model"
)

// Session represents an open SYSPRO e.net session that can submit multiple
// orders before being closed. Use Client.OpenSession to create one.
type Session interface {
	SubmitOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*SalesOrderResult, error)
	Close(ctx context.Context) error
}

// Client is the interface the batch processor uses to submit orders to SYSPRO.
type Client interface {
	SubmitSalesOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*SalesOrderResult, error)
	OpenSession(ctx context.Context) (Session, error)
}

// SalesOrderResult carries the outcome of a SORTOI transaction.
type SalesOrderResult struct {
	SysproOrderNumber string
	Success           bool
	ErrorMessage      string
}

// sortoiResponse is used to parse the XML returned by a SORTOI transaction.
// With Process=Import, the response uses <Order> (not <Orders><OrderHeader>).
type sortoiResponse struct {
	XMLName          xml.Name `xml:"SalesOrders"`
	OrderNumber      string   `xml:"Order>SalesOrder"`
	CustomerPoNumber string   `xml:"Order>CustomerPoNumber"`
	ValidationStatus string   `xml:"ValidationStatus>Status"`
	ItemsProcessed   string   `xml:"StatusOfItems>ItemsProcessed"`
	ItemsInvalid     string   `xml:"StatusOfItems>ItemsRejectedWithWarnings"`
}

// EnetClient is the real implementation that talks to SYSPRO e.net REST.
// SYSPRO allows only one session per operator — a second logon kills the first.
// The sessionMu mutex serialises all logon-logoff lifecycles so concurrent
// callers (batch processor, stock syncer, fulfilment syncer) cannot evict
// each other's sessions.
type EnetClient struct {
	baseURL    string
	operator   string
	password   string
	companyID  string
	logger     *slog.Logger
	httpClient *http.Client
	sessionMu  sync.Mutex
}

// NewEnetClient constructs a Client backed by the real SYSPRO e.net REST API.
func NewEnetClient(baseURL, operator, password, companyID string, logger *slog.Logger) *EnetClient {
	return &EnetClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		operator:   operator,
		password:   password,
		companyID:  companyID,
		logger:     logger,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SubmitSalesOrder performs a full logon → SORTOI transaction → logoff cycle.
// Logoff is always attempted even when an earlier step fails.
func (c *EnetClient) SubmitSalesOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*SalesOrderResult, error) {
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

	paramsXML, dataXML, err := buildSORTOI(order, lines)
	if err != nil {
		return nil, fmt.Errorf("building SORTOI XML: %w", err)
	}

	c.logger.Debug("submitting SORTOI", "order_number", order.OrderNumber, "lines", len(lines))

	respXML, err := c.transaction(ctx, guid, "SORTOI", paramsXML, dataXML)
	if err != nil {
		return nil, fmt.Errorf("syspro SORTOI transaction: %w", err)
	}

	return parseSORTOIResponse(respXML)
}

// logon calls GET /Logon and returns the session GUID.
func (c *EnetClient) logon(ctx context.Context) (string, error) {
	params := url.Values{
		"Operator":         {c.operator},
		"OperatorPassword": {c.password},
		"CompanyId":        {c.companyID},
	}
	body, err := c.get(ctx, "/Logon", params)
	if err != nil {
		return "", err
	}
	// Response is a JSON-quoted string, e.g. "\"<guid>\""
	var guid string
	if err := json.Unmarshal(body, &guid); err != nil {
		// Fallback: treat raw body as the GUID (some e.net versions return plain text)
		guid = strings.TrimSpace(string(body))
	}
	if guid == "" {
		return "", fmt.Errorf("logon returned empty session GUID")
	}
	return guid, nil
}

// logoff calls GET /Logoff with the session GUID.
func (c *EnetClient) logoff(ctx context.Context, guid string) error {
	params := url.Values{
		"UserId": {guid},
	}
	_, err := c.get(ctx, "/Logoff", params)
	return err
}

// transaction calls GET /Transaction/Post and returns the raw XML response body.
func (c *EnetClient) transaction(ctx context.Context, guid, businessObject, paramsXML, dataXML string) (string, error) {
	params := url.Values{
		"UserId":         {guid},
		"BusinessObject": {businessObject},
		"XmlParameters":  {paramsXML},
		"XmlIn":          {dataXML},
	}
	body, err := c.get(ctx, "/Transaction/Post", params)
	if err != nil {
		return "", err
	}
	// e.net may return JSON-wrapped or raw XML depending on version.
	var xmlStr string
	if err := json.Unmarshal(body, &xmlStr); err != nil {
		xmlStr = strings.TrimSpace(string(body))
	}
	if xmlStr == "" {
		return "", fmt.Errorf("transaction returned empty response")
	}
	c.logger.Debug("transaction response", "length", len(xmlStr), "first100", xmlStr[:min(100, len(xmlStr))])
	return xmlStr, nil
}

// query calls GET /Query/Query and returns the raw XML response body.
// Query-class business objects take only XmlIn (no XmlParameters).
func (c *EnetClient) query(ctx context.Context, guid, businessObject, xmlIn string) (string, error) {
	params := url.Values{
		"UserId":         {guid},
		"BusinessObject": {businessObject},
		"XmlIn":          {xmlIn},
	}
	body, err := c.get(ctx, "/Query/Query", params)
	if err != nil {
		return "", err
	}
	var xmlStr string
	if err := json.Unmarshal(body, &xmlStr); err != nil {
		xmlStr = strings.TrimSpace(string(body))
	}
	if xmlStr == "" {
		return "", fmt.Errorf("query returned empty response")
	}
	c.logger.Debug("query response", "length", len(xmlStr), "first100", xmlStr[:min(100, len(xmlStr))])
	return xmlStr, nil
}

// get sends a GET request with query parameters and returns the response body.
func (c *EnetClient) get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	target := c.baseURL + path + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", path, err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return body, nil
}

// parseSORTOIResponse interprets the XML string returned by a SORTOI transaction.
func parseSORTOIResponse(xmlStr string) (*SalesOrderResult, error) {
	// SYSPRO declares encoding="Windows-1252" which Go's xml package doesn't
	// support natively. The actual content is ASCII-safe, so strip the declaration.
	if i := strings.Index(xmlStr, "?>"); i != -1 {
		xmlStr = strings.TrimSpace(xmlStr[i+2:])
	}

	var resp sortoiResponse
	if err := xml.Unmarshal([]byte(xmlStr), &resp); err != nil {
		return nil, fmt.Errorf("parsing SORTOI response XML: %w", err)
	}

	// Case 1: SYSPRO returned a non-empty sales order number → explicit success.
	if resp.OrderNumber != "" {
		return &SalesOrderResult{SysproOrderNumber: resp.OrderNumber, Success: true}, nil
	}

	// Case 2: <Order> element present but SalesOrder is empty → import failed.
	// CustomerPoNumber is always populated when the <Order> element exists.
	if resp.CustomerPoNumber != "" {
		return &SalesOrderResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("SYSPRO import failed: order rejected (processed: %s, invalid: %s)", resp.ItemsProcessed, resp.ItemsInvalid),
		}, nil
	}

	// Case 3: No <Order> element. SYSPRO 8 omits the Order block for clean imports
	// that produce no warnings. Success iff ItemsProcessed is non-zero.
	if resp.ItemsProcessed != "" && resp.ItemsProcessed != "000000" {
		return &SalesOrderResult{Success: true}, nil
	}

	return &SalesOrderResult{
		Success:      false,
		ErrorMessage: fmt.Sprintf("SYSPRO import failed: no items processed (processed: %s)", resp.ItemsProcessed),
	}, nil
}
