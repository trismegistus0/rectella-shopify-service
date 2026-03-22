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
type sortoiResponse struct {
	XMLName          xml.Name `xml:"SalesOrders"`
	OrderNumber      string   `xml:"Orders>OrderHeader>SalesOrder"`
	CustomerPoNumber string   `xml:"Orders>OrderHeader>CustomerPoNumber"`
	ValidationStatus string   `xml:"ValidationStatus>Status"`
	ItemsProcessed   string   `xml:"StatusOfItems>ItemsProcessed"`
	ItemsInvalid     string   `xml:"StatusOfItems>ItemsInvalid"`
}

// enetClient is the real implementation that talks to SYSPRO e.net REST.
type enetClient struct {
	baseURL    string
	operator   string
	password   string
	companyID  string
	logger     *slog.Logger
	httpClient *http.Client
}

// NewEnetClient constructs a Client backed by the real SYSPRO e.net REST API.
func NewEnetClient(baseURL, operator, password, companyID string, logger *slog.Logger) Client {
	return &enetClient{
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
func (c *enetClient) SubmitSalesOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*SalesOrderResult, error) {
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
func (c *enetClient) logon(ctx context.Context) (string, error) {
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
func (c *enetClient) logoff(ctx context.Context, guid string) error {
	params := url.Values{
		"UserId": {guid},
	}
	_, err := c.get(ctx, "/Logoff", params)
	return err
}

// transaction calls GET /Transaction/Post and returns the raw XML response body.
func (c *enetClient) transaction(ctx context.Context, guid, businessObject, paramsXML, dataXML string) (string, error) {
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

// get sends a GET request with query parameters and returns the response body.
func (c *enetClient) get(ctx context.Context, path string, params url.Values) ([]byte, error) {
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

	if resp.ValidationStatus != "Successful" {
		return &SalesOrderResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("SYSPRO validation failed (processed: %s, invalid: %s)", resp.ItemsProcessed, resp.ItemsInvalid),
		}, nil
	}

	// SORTOI doesn't always echo back the generated SO number.
	// When empty, use the customer PO number for traceability.
	orderRef := resp.OrderNumber
	if orderRef == "" {
		orderRef = resp.CustomerPoNumber
	}

	return &SalesOrderResult{
		SysproOrderNumber: orderRef,
		Success:           true,
	}, nil
}
