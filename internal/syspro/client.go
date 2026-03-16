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

// sortoiResponse is used to parse the XML returned by a successful SORTOI transaction.
// SYSPRO wraps the result in a <SalesOrders><Orders><OrderHeader>...</OrderHeader></Orders></SalesOrders> envelope.
type sortoiResponse struct {
	XMLName     xml.Name `xml:"SalesOrders"`
	OrderNumber string   `xml:"Orders>OrderHeader>SalesOrder"`
	ReturnCode  string   `xml:"ReturnCode"`
	Message     string   `xml:"Message"`
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

// logon calls POST /Logon and returns the session GUID.
func (c *enetClient) logon(ctx context.Context) (string, error) {
	form := url.Values{
		"Operator":         {c.operator},
		"OperatorPassword": {c.password},
		"CompanyId":        {c.companyID},
	}
	body, err := c.post(ctx, "/Logon", form)
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

// logoff calls POST /Logoff with the session GUID.
func (c *enetClient) logoff(ctx context.Context, guid string) error {
	form := url.Values{
		"UserId": {guid},
	}
	_, err := c.post(ctx, "/Logoff", form)
	return err
}

// transaction calls POST /Transaction and returns the raw XML response body.
func (c *enetClient) transaction(ctx context.Context, guid, businessObject, paramsXML, dataXML string) (string, error) {
	form := url.Values{
		"UserId":         {guid},
		"BusinessObject": {businessObject},
		"XmlParameters":  {paramsXML},
		"XmlIn":          {dataXML},
	}
	body, err := c.post(ctx, "/Transaction", form)
	if err != nil {
		return "", err
	}
	// e.net wraps the XML in a JSON string
	var xmlStr string
	if err := json.Unmarshal(body, &xmlStr); err != nil {
		xmlStr = strings.TrimSpace(string(body))
	}
	return xmlStr, nil
}

// post is a thin wrapper around http.PostForm that handles URL construction,
// context propagation, and non-2xx status codes.
func (c *enetClient) post(ctx context.Context, path string, form url.Values) ([]byte, error) {
	target := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return body, nil
}

// parseSORTOIResponse interprets the XML string returned by a SORTOI transaction.
func parseSORTOIResponse(xmlStr string) (*SalesOrderResult, error) {
	var resp sortoiResponse
	if err := xml.Unmarshal([]byte(xmlStr), &resp); err != nil {
		return nil, fmt.Errorf("parsing SORTOI response XML: %w", err)
	}

	// SYSPRO signals failure via a non-empty ReturnCode or empty SalesOrder number.
	if resp.ReturnCode != "" && resp.ReturnCode != "0" {
		return &SalesOrderResult{
			Success:      false,
			ErrorMessage: resp.Message,
		}, nil
	}

	return &SalesOrderResult{
		SysproOrderNumber: resp.OrderNumber,
		Success:           resp.OrderNumber != "",
		ErrorMessage:      resp.Message,
	}, nil
}
