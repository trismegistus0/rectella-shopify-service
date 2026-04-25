package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestFetcher(srv *httptest.Server) *TransactionsFetcher {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := NewTransactionsFetcher("unused", "test-token", logger)
	f.WithBaseURL(srv.URL)
	return f
}

// newGraphQLMock spins up an httptest.Server that responds to
// POST /graphql.json with the supplied transactions slice. Asserts
// the access-token header.
func newGraphQLMock(t *testing.T, txns []graphqlTransaction) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("X-Shopify-Access-Token") != "test-token" {
			t.Error("missing access token")
		}
		// Drain body to verify it parses as a GraphQL request.
		body, _ := io.ReadAll(r.Body)
		var reqBody struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &reqBody); err != nil {
			t.Errorf("request body not valid json: %v", err)
		}
		if !strings.Contains(reqBody.Query, "fees") {
			t.Errorf("query missing fees field: %s", reqBody.Query)
		}

		resp := graphqlResponse{}
		resp.Data.Order.Transactions = txns
		w.Header().Set("Content-Type", "application/json")
		// Test fake — JSON response under our control to a Go HTTP client,
		// not a browser. The XSS rule for direct ResponseWriter writes
		// doesn't apply.
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// helper: build a transaction with money values as string floats.
func txn(id, kind, status, gateway, amount string) graphqlTransaction {
	t := graphqlTransaction{
		ID:      id,
		Kind:    kind,
		Status:  status,
		Gateway: gateway,
	}
	t.AmountSet.ShopMoney = graphqlMoney{Amount: amount, CurrencyCode: "GBP"}
	return t
}

func TestFetchForOrder_ShopifyPayments(t *testing.T) {
	// Real shape captured from BBQ1042 on 2026-04-24.
	gt := txn("gid://shopify/OrderTransaction/13733937152332", "SALE", "SUCCESS", "shopify_payments", "27.98")
	gt.Fees = []graphqlFee{{
		Amount:    graphqlMoney{Amount: "0.81", CurrencyCode: "GBP"},
		Type:      "processing_fee",
		TaxAmount: graphqlMoney{Amount: "0.00", CurrencyCode: "GBP"},
	}}

	srv := newGraphQLMock(t, []graphqlTransaction{gt})
	defer srv.Close()

	f := newTestFetcher(srv)
	out, err := f.FetchForOrder(context.Background(), 12614627819852, "#BBQ1042", "c@example.com")
	if err != nil {
		t.Fatalf("FetchForOrder: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 txn, got %d", len(out))
	}
	got := out[0]
	if got.ID != 13733937152332 {
		t.Errorf("ID = %d", got.ID)
	}
	if got.Gross != 27.98 {
		t.Errorf("Gross = %.2f, want 27.98", got.Gross)
	}
	if got.Fee != 0.81 {
		t.Errorf("Fee = %.2f, want 0.81 (taxAmount excluded)", got.Fee)
	}
	if got.Net != 27.17 {
		t.Errorf("Net = %.2f, want 27.17", got.Net)
	}
	if got.PaymentGateway != "shopify_payments" {
		t.Errorf("Gateway = %q", got.PaymentGateway)
	}
}

func TestFetchForOrder_ShopifyPayments_TaxAmountExcluded(t *testing.T) {
	// Synthetic case: Shopify reports a non-zero taxAmount on the fee.
	// Our extractor must NOT add it to the headline fee total — that
	// VAT-on-fee belongs in a separate accounting line.
	gt := txn("gid://shopify/OrderTransaction/9000001", "SALE", "SUCCESS", "shopify_payments", "100.00")
	gt.Fees = []graphqlFee{{
		Amount:    graphqlMoney{Amount: "2.00", CurrencyCode: "GBP"},
		Type:      "processing_fee",
		TaxAmount: graphqlMoney{Amount: "0.40", CurrencyCode: "GBP"}, // 20% VAT, must be ignored
	}}

	srv := newGraphQLMock(t, []graphqlTransaction{gt})
	defer srv.Close()

	out, err := newTestFetcher(srv).FetchForOrder(context.Background(), 1, "#X", "")
	if err != nil {
		t.Fatalf("FetchForOrder: %v", err)
	}
	if out[0].Fee != 2.00 {
		t.Errorf("Fee = %.2f, want 2.00 (taxAmount must be excluded)", out[0].Fee)
	}
}

func TestFetchForOrder_PayPal(t *testing.T) {
	// Real shape captured from BBQ1043 on 2026-04-24.
	gt := txn("gid://shopify/OrderTransaction/13734118850892", "SALE", "SUCCESS", "paypal", "55.96")
	gt.Fees = nil // PayPal returns empty fees[]
	gt.ReceiptJSON = `{"timestamp":"2026-04-24T15:05:21Z","fee_amount":"1.92","gross_amount":"55.96","payment_status":"Completed"}`

	srv := newGraphQLMock(t, []graphqlTransaction{gt})
	defer srv.Close()

	out, err := newTestFetcher(srv).FetchForOrder(context.Background(), 1, "#BBQ1043", "")
	if err != nil {
		t.Fatalf("FetchForOrder: %v", err)
	}
	if out[0].Fee != 1.92 {
		t.Errorf("Fee = %.2f, want 1.92 from receipt.fee_amount", out[0].Fee)
	}
	if out[0].Net != 54.04 {
		t.Errorf("Net = %.2f, want 54.04", out[0].Net)
	}
}

func TestFetchForOrder_NoFeeData_Manual(t *testing.T) {
	// Manual gateway with no fee. Legitimate — no fee deducted on bank
	// transfer or COD. Should return 0 silently (Debug only).
	gt := txn("gid://shopify/OrderTransaction/9000002", "SALE", "SUCCESS", "manual", "100.00")

	srv := newGraphQLMock(t, []graphqlTransaction{gt})
	defer srv.Close()

	out, err := newTestFetcher(srv).FetchForOrder(context.Background(), 1, "#X", "")
	if err != nil {
		t.Fatalf("FetchForOrder: %v", err)
	}
	if out[0].Fee != 0 {
		t.Errorf("Fee = %.2f, want 0 for manual gateway", out[0].Fee)
	}
	if out[0].Net != 100.00 {
		t.Errorf("Net = %.2f", out[0].Net)
	}
}

func TestFetchForOrder_NoFeeData_KnownGateway_LogsWarn(t *testing.T) {
	// Shopify Payments txn with empty fees[] and no useful receiptJson.
	// Production code path returns 0 but logs WARN — that warn is the
	// silent-fail tripwire so future API breakage doesn't go unnoticed.
	gt := txn("gid://shopify/OrderTransaction/9000003", "SALE", "SUCCESS", "shopify_payments", "100.00")
	gt.Fees = nil
	gt.ReceiptJSON = `{"id":"pi_xxx"}` // no fee_amount key

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv := newGraphQLMock(t, []graphqlTransaction{gt})
	defer srv.Close()
	f := NewTransactionsFetcher("unused", "test-token", logger)
	f.WithBaseURL(srv.URL)

	out, err := f.FetchForOrder(context.Background(), 1, "#X", "")
	if err != nil {
		t.Fatalf("FetchForOrder: %v", err)
	}
	if out[0].Fee != 0 {
		t.Errorf("Fee = %.2f, want 0", out[0].Fee)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "WARN") {
		t.Errorf("expected WARN level log, got: %s", logged)
	}
	if !strings.Contains(logged, "fee extraction returned zero for known-paid gateway") {
		t.Errorf("missing tripwire message, got: %s", logged)
	}
}

func TestFetchForOrder_FilterRefundAndAuth(t *testing.T) {
	srv := newGraphQLMock(t, []graphqlTransaction{
		txn("gid://shopify/OrderTransaction/1", "SALE", "SUCCESS", "shopify_payments", "100.00"),
		txn("gid://shopify/OrderTransaction/2", "REFUND", "SUCCESS", "shopify_payments", "10.00"),       // dropped
		txn("gid://shopify/OrderTransaction/3", "AUTHORIZATION", "SUCCESS", "shopify_payments", "100.00"), // dropped
		txn("gid://shopify/OrderTransaction/4", "SALE", "FAILURE", "shopify_payments", "100.00"),         // dropped
		txn("gid://shopify/OrderTransaction/5", "CAPTURE", "SUCCESS", "shopify_payments", "50.00"),       // kept
	})
	defer srv.Close()

	out, err := newTestFetcher(srv).FetchForOrder(context.Background(), 1, "#X", "")
	if err != nil {
		t.Fatalf("FetchForOrder: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 txns (sale+capture only), got %d", len(out))
	}
	if out[0].ID != 1 || out[1].ID != 5 {
		t.Errorf("filtered set wrong: %+v", out)
	}
}

func TestFetchForOrder_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestFetcher(srv).FetchForOrder(context.Background(), 1, "", "")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %v, want HTTP 500", err)
	}
}

func TestFetchForOrder_GraphQLErrors(t *testing.T) {
	// 200 OK but with errors[] populated — Shopify's GraphQL convention
	// for malformed queries / missing scopes / throttling.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]string{{"message": "Field 'fees' doesn't exist"}},
		})
	}))
	defer srv.Close()

	_, err := newTestFetcher(srv).FetchForOrder(context.Background(), 1, "", "")
	if err == nil {
		t.Fatal("want error from graphql errors[], got nil")
	}
	if !strings.Contains(err.Error(), "graphql errors") {
		t.Errorf("error = %v", err)
	}
}

func TestParseTxnID(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"gid://shopify/OrderTransaction/13734118850892", 13734118850892},
		{"gid://shopify/OrderTransaction/1", 1},
		{"", 0},
		{"not-a-gid", 0},
		{"gid://shopify/OrderTransaction/", 0},
	}
	for _, c := range tests {
		got := parseTxnID(c.in)
		if got != c.want {
			t.Errorf("parseTxnID(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestNextLink(t *testing.T) {
	h := `<https://example.myshopify.com/admin/api/2025-04/orders.json?page_info=abc>; rel="next", <https://example.myshopify.com/admin/api/2025-04/orders.json?page_info=prev>; rel="previous"`
	got := nextLink(h)
	want := "https://example.myshopify.com/admin/api/2025-04/orders.json?page_info=abc"
	if got != want {
		t.Errorf("nextLink = %q, want %q", got, want)
	}
	if nextLink("") != "" {
		t.Error("empty header should return empty")
	}
	if nextLink(`<https://x/>; rel="previous"`) != "" {
		t.Error("no next rel should return empty")
	}
}
