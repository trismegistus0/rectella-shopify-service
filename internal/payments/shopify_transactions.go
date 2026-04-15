package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ShopifyTransaction is the internal shape returned by TransactionsFetcher.
// Only the fields we care about for ARSPAY cash-receipt posting: gross
// amount paid, processor fee, the resulting net that lands in the
// Rectella bank account, and the timestamp SYSPRO uses for the posting
// period.
type ShopifyTransaction struct {
	ID             int64
	OrderID        int64
	OrderNumber    string
	CustomerEmail  string
	Gross          float64
	Fee            float64
	Net            float64
	Currency       string
	PaymentGateway string
	ProcessedAt    time.Time
}

// TransactionsFetcher pulls per-order transactions from the Shopify
// Admin REST API. Uses the REST endpoint rather than GraphQL because
// `receipt.charges.data[0].balance_transaction.fee` is still the only
// reliable path to the processor fee on 2025-04.
type TransactionsFetcher struct {
	baseURL     string // full https://{store}/admin/api/2025-04, overridable for tests
	accessToken string
	httpClient  *http.Client
	logger      *slog.Logger
}

// NewTransactionsFetcher constructs a fetcher. `storeURL` is the bare
// host (e.g. "rectella.myshopify.com") — the full base URL is derived.
func NewTransactionsFetcher(storeURL, accessToken string, logger *slog.Logger) *TransactionsFetcher {
	base := fmt.Sprintf("https://%s/admin/api/2025-04", strings.TrimRight(storeURL, "/"))
	return &TransactionsFetcher{
		baseURL:     base,
		accessToken: accessToken,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		logger:      logger,
	}
}

// WithBaseURL overrides the full base URL. Test-only.
func (f *TransactionsFetcher) WithBaseURL(base string) *TransactionsFetcher {
	f.baseURL = strings.TrimRight(base, "/")
	return f
}

// shopifyTransactionsResponse mirrors Shopify's transactions.json shape.
// Amount strings are parsed to float64 at the boundary.
type shopifyTransactionsResponse struct {
	Transactions []struct {
		ID           int64     `json:"id"`
		OrderID      int64     `json:"order_id"`
		Kind         string    `json:"kind"`
		Status       string    `json:"status"`
		Amount       string    `json:"amount"`
		Currency     string    `json:"currency"`
		Gateway      string    `json:"gateway"`
		ProcessedAt  time.Time `json:"processed_at"`
		Receipt      struct {
			Charges struct {
				Data []struct {
					BalanceTransaction struct {
						Fee int64 `json:"fee"` // minor units (pence)
					} `json:"balance_transaction"`
				} `json:"data"`
			} `json:"charges"`
		} `json:"receipt"`
	} `json:"transactions"`
}

// FetchForOrder returns the settled `sale` or `capture` transactions for
// a single Shopify order. Refunds, authorizations, and voids are filtered
// out — only successful money-in events become cash receipts.
func (f *TransactionsFetcher) FetchForOrder(ctx context.Context, orderID int64, orderNumber, customerEmail string) ([]ShopifyTransaction, error) {
	path := fmt.Sprintf("%s/orders/%d/transactions.json", f.baseURL, orderID)
	return f.fetch(ctx, path, orderNumber, customerEmail)
}

// FetchForOrderRange returns all settled money-in transactions across
// the Shopify orders API for a given time window. Used by the daily
// report to avoid having to iterate orders first. Currently calls the
// Admin REST endpoint `/admin/api/2025-04/shopify_payments/balance/transactions.json`
// is NOT used — Shopify Payments Payouts API requires Shopify Plus.
// For the MVP daily email we list orders in the window and then fetch
// per-order transactions. That keeps the code path identical to
// FetchForOrder and works on non-Plus stores.
func (f *TransactionsFetcher) FetchOrdersInWindow(ctx context.Context, since, until time.Time) ([]ShopifyTransaction, error) {
	// 1. List orders in the window. /orders.json with financial_status=paid
	//    paginated via Link header.
	orders, err := f.listOrdersInWindow(ctx, since, until)
	if err != nil {
		return nil, err
	}
	// 2. Fetch transactions for each order, concatenate, filter.
	var all []ShopifyTransaction
	for _, o := range orders {
		txns, err := f.FetchForOrder(ctx, o.ID, o.Name, o.Email)
		if err != nil {
			f.logger.Warn("fetching order transactions", "order_id", o.ID, "error", err)
			continue
		}
		for _, t := range txns {
			if t.ProcessedAt.Before(since) || !t.ProcessedAt.Before(until) {
				continue
			}
			all = append(all, t)
		}
	}
	return all, nil
}

type orderSummary struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type ordersResponse struct {
	Orders []orderSummary `json:"orders"`
}

func (f *TransactionsFetcher) listOrdersInWindow(ctx context.Context, since, until time.Time) ([]orderSummary, error) {
	u, err := url.Parse(f.baseURL + "/orders.json")
	if err != nil {
		return nil, fmt.Errorf("parsing orders url: %w", err)
	}
	q := u.Query()
	q.Set("status", "any")
	q.Set("financial_status", "paid")
	q.Set("processed_at_min", since.UTC().Format(time.RFC3339))
	q.Set("processed_at_max", until.UTC().Format(time.RFC3339))
	q.Set("limit", "250")
	q.Set("fields", "id,name,email,processed_at")
	u.RawQuery = q.Encode()

	next := u.String()
	var all []orderSummary
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Shopify-Access-Token", f.accessToken)
		resp, err := f.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("orders.json: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("orders.json HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var or ordersResponse
		if err := json.Unmarshal(body, &or); err != nil {
			return nil, fmt.Errorf("parsing orders.json: %w", err)
		}
		all = append(all, or.Orders...)
		next = nextLink(resp.Header.Get("Link"))
	}
	return all, nil
}

// nextLink parses the `Link` header and returns the URL for rel="next"
// or "" if there is none.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		if i := strings.Index(part, "<"); i != -1 {
			if j := strings.Index(part[i+1:], ">"); j != -1 {
				return part[i+1 : i+1+j]
			}
		}
	}
	return ""
}

func (f *TransactionsFetcher) fetch(ctx context.Context, target, orderNumber, customerEmail string) ([]ShopifyTransaction, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("X-Shopify-Access-Token", f.accessToken)
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transactions.json: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading transactions.json: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transactions.json HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var raw shopifyTransactionsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing transactions.json: %w", err)
	}
	var out []ShopifyTransaction
	for _, t := range raw.Transactions {
		if t.Status != "success" {
			continue
		}
		if t.Kind != "sale" && t.Kind != "capture" {
			continue
		}
		gross, err := strconv.ParseFloat(t.Amount, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing amount %q: %w", t.Amount, err)
		}
		// Fee comes in minor units (pence). Zero fee = unknown (non-Shopify
		// Payments gateway), treat as 0. The Shopify Payments path always
		// populates this field for successful captures.
		var fee float64
		if len(t.Receipt.Charges.Data) > 0 {
			feeMinor := t.Receipt.Charges.Data[0].BalanceTransaction.Fee
			fee = float64(feeMinor) / 100.0
		}
		out = append(out, ShopifyTransaction{
			ID:             t.ID,
			OrderID:        t.OrderID,
			OrderNumber:    orderNumber,
			CustomerEmail:  customerEmail,
			Gross:          gross,
			Fee:            fee,
			Net:            gross - fee,
			Currency:       t.Currency,
			PaymentGateway: t.Gateway,
			ProcessedAt:    t.ProcessedAt,
		})
	}
	return out, nil
}
