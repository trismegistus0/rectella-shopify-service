package payments

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestFetcher(srv *httptest.Server) *TransactionsFetcher {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := NewTransactionsFetcher("unused", "test-token", logger)
	f.WithBaseURL(srv.URL)
	return f
}

func TestFetchForOrder_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders/12345/transactions.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Shopify-Access-Token") != "test-token" {
			t.Error("missing access token")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"transactions": [
				{
					"id": 888001,
					"order_id": 12345,
					"kind": "sale",
					"status": "success",
					"amount": "125.00",
					"currency": "GBP",
					"gateway": "shopify_payments",
					"processed_at": "2026-04-15T10:30:00Z",
					"receipt": {
						"charges": {
							"data": [
								{"balance_transaction": {"fee": 375}}
							]
						}
					}
				},
				{
					"id": 888002,
					"order_id": 12345,
					"kind": "refund",
					"status": "success",
					"amount": "10.00",
					"currency": "GBP",
					"gateway": "shopify_payments",
					"processed_at": "2026-04-15T11:00:00Z"
				},
				{
					"id": 888003,
					"order_id": 12345,
					"kind": "authorization",
					"status": "success",
					"amount": "125.00",
					"currency": "GBP",
					"gateway": "shopify_payments",
					"processed_at": "2026-04-15T10:29:00Z"
				}
			]
		}`))
	}))
	defer srv.Close()

	f := newTestFetcher(srv)
	txns, err := f.FetchForOrder(context.Background(), 12345, "#BBQ1010", "customer@example.com")
	if err != nil {
		t.Fatalf("FetchForOrder: %v", err)
	}
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction after filtering, got %d", len(txns))
	}
	got := txns[0]
	if got.ID != 888001 {
		t.Errorf("ID = %d, want 888001", got.ID)
	}
	if got.Gross != 125.00 {
		t.Errorf("Gross = %f, want 125.00", got.Gross)
	}
	if got.Fee != 3.75 {
		t.Errorf("Fee = %f, want 3.75", got.Fee)
	}
	if got.Net != 121.25 {
		t.Errorf("Net = %f, want 121.25", got.Net)
	}
	if got.OrderNumber != "#BBQ1010" {
		t.Errorf("OrderNumber = %q", got.OrderNumber)
	}
	if got.CustomerEmail != "customer@example.com" {
		t.Errorf("CustomerEmail = %q", got.CustomerEmail)
	}
}

func TestFetchForOrder_NonShopifyPayments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"transactions": [
				{
					"id": 999,
					"order_id": 500,
					"kind": "sale",
					"status": "success",
					"amount": "50.00",
					"currency": "GBP",
					"gateway": "paypal",
					"processed_at": "2026-04-15T10:30:00Z",
					"receipt": {}
				}
			]
		}`))
	}))
	defer srv.Close()

	f := newTestFetcher(srv)
	txns, err := f.FetchForOrder(context.Background(), 500, "#X", "")
	if err != nil {
		t.Fatalf("FetchForOrder: %v", err)
	}
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(txns))
	}
	if txns[0].Fee != 0 {
		t.Errorf("Fee = %f, want 0 when receipt missing", txns[0].Fee)
	}
	if txns[0].Net != 50.00 {
		t.Errorf("Net = %f, want 50.00", txns[0].Net)
	}
}

func TestFetchForOrder_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("kaboom"))
	}))
	defer srv.Close()

	f := newTestFetcher(srv)
	_, err := f.FetchForOrder(context.Background(), 1, "", "")
	if err == nil {
		t.Fatal("want error, got nil")
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
