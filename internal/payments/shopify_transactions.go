package payments

import (
	"bytes"
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
// Only the fields the daily report (and ARSPAY scaffolding) need: gross
// amount paid, processor fee, the resulting net that lands in Rectella's
// bank account, and the timestamp for SYSPRO's posting period.
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

// TransactionsFetcher pulls per-order transactions from Shopify.
//
// Order listing uses the REST Admin endpoint (with Link-header pagination
// — already battle-tested via nextLink). Per-order transaction details
// use the GraphQL Admin endpoint, because GraphQL's `transactions.fees[]`
// is the only path on a non-Plus store that returns the actual
// Shopify Payments processing fee. PayPal still puts its fee in the
// REST-style `receipt.fee_amount` field, which we read out of the
// `receiptJson` blob GraphQL also exposes.
type TransactionsFetcher struct {
	baseURL     string // full https://{store}/admin/api/2025-04, overridable for tests
	accessToken string
	httpClient  *http.Client
	logger      *slog.Logger
}

// NewTransactionsFetcher constructs a fetcher. `storeURL` is the bare
// host (e.g. "h0snak-s5.myshopify.com") — the full base URL is derived.
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

// FetchForOrder returns the settled `sale` or `capture` transactions for
// a single Shopify order. Refunds, authorizations, and voids are filtered
// out — only successful money-in events become cash receipts.
func (f *TransactionsFetcher) FetchForOrder(ctx context.Context, orderID int64, orderNumber, customerEmail string) ([]ShopifyTransaction, error) {
	return f.fetchGraphQL(ctx, orderID, orderNumber, customerEmail)
}

// FetchOrdersInWindow lists paid orders in [since, until) via REST and
// fetches each order's transactions via GraphQL. Returns the flat list
// of money-in transactions whose ProcessedAt falls in the window.
func (f *TransactionsFetcher) FetchOrdersInWindow(ctx context.Context, since, until time.Time) ([]ShopifyTransaction, error) {
	orders, err := f.listOrdersInWindow(ctx, since, until)
	if err != nil {
		return nil, err
	}
	var all []ShopifyTransaction
	for _, o := range orders {
		// Skip Shopify-flagged test orders. Shopify exposes a top-level
		// boolean `test` on each order — true when the storefront was in
		// test mode or an admin manually placed a test order. These are
		// not real money movements and must not appear in credit-control
		// reports. Confirmed against Sarah on 2026-04-25 after the
		// initial backfill leaked BBQ1020-1023.
		if o.Test {
			f.logger.Debug("skipping test order", "order", o.Name, "id", o.ID)
			continue
		}
		txns, err := f.FetchForOrder(ctx, o.ID, o.Name, o.Email)
		if err != nil {
			f.logger.Warn("fetching order transactions", "order_id", o.ID, "error", err)
			continue
		}
		for _, t := range txns {
			if t.ProcessedAt.Before(since) || !t.ProcessedAt.Before(until) {
				continue
			}
			// Skip "manual" gateway — used in Shopify admin to mark an
			// order paid by a non-online method (cash on collection,
			// bank transfer, hand-keyed). Rectella's B2C storefront
			// doesn't legitimately use this in Phase 1; every "manual"
			// payment seen so far has been a test/admin operator
			// adjusting an order. Excluded per Sarah 2026-04-25.
			// If Rectella ever ships legitimate manual payments,
			// remove this filter and accept the £0 fee rows.
			if t.PaymentGateway == "manual" {
				f.logger.Debug("skipping manual-gateway transaction",
					"order", o.Name, "txn_id", t.ID)
				continue
			}
			all = append(all, t)
		}
	}
	return all, nil
}

// orderSummary / ordersResponse / nextLink / listOrdersInWindow — the
// REST orders-listing path is unchanged; pagination via the Link header
// already works at Rectella's volume and there's no fee data here so
// switching to GraphQL would just add cursor-pagination code.

type orderSummary struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Test  bool   `json:"test"`
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
	q.Set("fields", "id,name,email,processed_at,test")
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

// --- GraphQL transaction fetch ---

const transactionsQuery = `query($id: ID!) {
  order(id: $id) {
    transactions {
      id
      kind
      status
      gateway
      processedAt
      amountSet { shopMoney { amount currencyCode } }
      fees {
        amount { amount currencyCode }
        type
        taxAmount { amount currencyCode }
      }
      receiptJson
    }
  }
}`

type graphqlMoney struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currencyCode"`
}

type graphqlFee struct {
	Amount    graphqlMoney `json:"amount"`
	Type      string       `json:"type"`
	TaxAmount graphqlMoney `json:"taxAmount"`
}

type graphqlTransaction struct {
	ID          string       `json:"id"` // gid://shopify/OrderTransaction/<n>
	Kind        string       `json:"kind"`
	Status      string       `json:"status"`
	Gateway     string       `json:"gateway"`
	ProcessedAt time.Time    `json:"processedAt"`
	AmountSet   struct {
		ShopMoney graphqlMoney `json:"shopMoney"`
	} `json:"amountSet"`
	Fees        []graphqlFee `json:"fees"`
	ReceiptJSON string       `json:"receiptJson"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type graphqlResponse struct {
	Data struct {
		Order struct {
			Transactions []graphqlTransaction `json:"transactions"`
		} `json:"order"`
	} `json:"data"`
	Errors []graphqlError `json:"errors,omitempty"`
}

func (f *TransactionsFetcher) fetchGraphQL(ctx context.Context, orderID int64, orderNumber, customerEmail string) ([]ShopifyTransaction, error) {
	body, err := json.Marshal(map[string]any{
		"query": transactionsQuery,
		"variables": map[string]string{
			"id": fmt.Sprintf("gid://shopify/Order/%d", orderID),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encoding graphql body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/graphql.json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("X-Shopify-Access-Token", f.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphql call: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading graphql response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var gr graphqlResponse
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return nil, fmt.Errorf("parsing graphql response: %w", err)
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, len(gr.Errors))
		for i, e := range gr.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}

	var out []ShopifyTransaction
	for _, t := range gr.Data.Order.Transactions {
		if t.Status != "SUCCESS" {
			continue
		}
		if t.Kind != "SALE" && t.Kind != "CAPTURE" {
			continue
		}
		gross, err := strconv.ParseFloat(t.AmountSet.ShopMoney.Amount, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing amount %q: %w", t.AmountSet.ShopMoney.Amount, err)
		}
		txnID := parseTxnID(t.ID)
		fee := f.extractFee(t, orderNumber, txnID)
		out = append(out, ShopifyTransaction{
			ID:             txnID,
			OrderID:        orderID,
			OrderNumber:    orderNumber,
			CustomerEmail:  customerEmail,
			Gross:          gross,
			Fee:            fee,
			Net:            gross - fee,
			Currency:       t.AmountSet.ShopMoney.CurrencyCode,
			PaymentGateway: t.Gateway,
			ProcessedAt:    t.ProcessedAt,
		})
	}
	return out, nil
}

// extractFee returns the per-transaction processor fee in major units.
//
// Order of precedence:
//
//  1. GraphQL `fees[].amount` — sum across all fees. This is how
//     Shopify Payments returns its processing fee. We deliberately
//     EXCLUDE `taxAmount` (VAT on the processing fee, owed to HMRC) —
//     that's a separate accounting line, not a deduction on the
//     payout. Don't "fix" this without talking to Liz.
//
//  2. PayPal puts its fee in `receipt.fee_amount` (top-level string,
//     major units) and leaves `fees[]` empty. Parse out of receiptJson.
//
//  3. Anything else with zero fee data on a known card processor
//     gateway (`shopify_payments`, `paypal`, `stripe`) is logged as a
//     WARN — that's the silent-fail signature that bit us on
//     2026-04-25 and would have been visible at boot if we'd had this.
//     Manual gateways get Debug only and are legitimately fee-free.
func (f *TransactionsFetcher) extractFee(t graphqlTransaction, orderNumber string, txnID int64) float64 {
	if len(t.Fees) > 0 {
		var sum float64
		for _, fee := range t.Fees {
			amt, err := strconv.ParseFloat(fee.Amount.Amount, 64)
			if err != nil {
				f.logger.Warn("non-numeric fee amount",
					"order", orderNumber, "txn_id", txnID,
					"raw", fee.Amount.Amount, "error", err)
				continue
			}
			sum += amt
		}
		return sum
	}

	if t.ReceiptJSON != "" {
		var rcpt struct {
			FeeAmount string `json:"fee_amount"`
		}
		if err := json.Unmarshal([]byte(t.ReceiptJSON), &rcpt); err == nil && rcpt.FeeAmount != "" {
			if amt, err := strconv.ParseFloat(rcpt.FeeAmount, 64); err == nil {
				return amt
			}
		}
	}

	if isKnownCardGateway(t.Gateway) {
		f.logger.Warn("fee extraction returned zero for known-paid gateway",
			"order", orderNumber, "gateway", t.Gateway, "txn_id", txnID)
	} else {
		f.logger.Debug("no fee data (manual gateway)",
			"order", orderNumber, "gateway", t.Gateway, "txn_id", txnID)
	}
	return 0
}

func isKnownCardGateway(gw string) bool {
	switch gw {
	case "shopify_payments", "paypal", "stripe", "amazon_payments", "klarna":
		return true
	}
	return false
}

// parseTxnID extracts the trailing numeric segment from a Shopify GID
// like "gid://shopify/OrderTransaction/13734118850892". Returns 0 if
// the GID can't be parsed — a non-fatal degradation since the ID is
// only used for downstream logging/audit, not as a join key.
func parseTxnID(gid string) int64 {
	if i := strings.LastIndex(gid, "/"); i >= 0 && i+1 < len(gid) {
		if id, err := strconv.ParseInt(gid[i+1:], 10, 64); err == nil {
			return id
		}
	}
	return 0
}
