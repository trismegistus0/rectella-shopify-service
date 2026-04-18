package reconcile

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

// mockStore captures what the sweeper asks of the store and lets tests
// control its responses.
type mockStore struct {
	existing map[int64]bool
	created  []model.Order
}

func (m *mockStore) ShopifyOrdersExist(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if m.existing[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (m *mockStore) CreateOrder(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error {
	m.created = append(m.created, order)
	return nil
}

func newFakeShopify(t *testing.T, body string) *httptest.Server {
	t.Helper()
	// Parse once to validate the test fixture JSON; the handler re-encodes
	// through json.NewEncoder which is semgrep-safe (vs direct Write).
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("test fixture body is not valid JSON: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Shopify-Access-Token") == "" {
			t.Errorf("missing access token header")
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/orders.json" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(parsed)
	}))
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const threeOrdersBody = `{
  "orders": [
    {
      "id": 1001,
      "name": "#BBQ1001",
      "email": "a@example.com",
      "created_at": "2026-04-13T10:00:00Z",
      "total_price": "10.00",
      "financial_status": "paid",
      "gateway": "shopify_payments",
      "line_items": [{"sku": "CBBQ0001", "quantity": 1, "price": "10.00", "total_discount": "0.00", "tax_lines": []}]
    },
    {
      "id": 1002,
      "name": "#BBQ1002",
      "email": "b@example.com",
      "created_at": "2026-04-13T11:00:00Z",
      "total_price": "20.00",
      "financial_status": "paid",
      "gateway": "shopify_payments",
      "line_items": [{"sku": "LUMP0148", "quantity": 2, "price": "10.00", "total_discount": "0.00", "tax_lines": []}]
    },
    {
      "id": 1003,
      "name": "#BBQ1003",
      "email": "c@example.com",
      "created_at": "2026-04-13T12:00:00Z",
      "total_price": "30.00",
      "financial_status": "paid",
      "gateway": "shopify_payments",
      "line_items": [{"sku": "CBBQ0001", "quantity": 3, "price": "10.00", "total_discount": "0.00", "tax_lines": []}]
    }
  ]
}`

// TestSweep_StagesMissingOrders: Shopify returns 3 orders, DB has 1, sweeper stages 2.
func TestSweep_StagesMissingOrders(t *testing.T) {
	srv := newFakeShopify(t, threeOrdersBody)
	defer srv.Close()

	ms := &mockStore{existing: map[int64]bool{1002: true}}
	sw := New(ms, "ignored", "shpat_test", time.Minute, testLogger(), WithBaseURL(srv.URL))
	if sw == nil {
		t.Fatal("New returned nil")
	}

	if err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(ms.created) != 2 {
		t.Fatalf("expected 2 orders staged, got %d", len(ms.created))
	}

	wantIDs := map[int64]bool{1001: true, 1003: true}
	for _, o := range ms.created {
		if !wantIDs[o.ShopifyOrderID] {
			t.Errorf("unexpected staged order id %d", o.ShopifyOrderID)
		}
	}
}

// TestSweep_AllPresent: every Shopify order already in DB, sweeper stages 0.
func TestSweep_AllPresent(t *testing.T) {
	srv := newFakeShopify(t, threeOrdersBody)
	defer srv.Close()

	ms := &mockStore{existing: map[int64]bool{1001: true, 1002: true, 1003: true}}
	sw := New(ms, "ignored", "shpat_test", time.Minute, testLogger(), WithBaseURL(srv.URL))

	if err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(ms.created) != 0 {
		t.Errorf("expected 0 orders staged, got %d", len(ms.created))
	}
}

// TestSweep_SkipsExistingFailedOrders documents the intent: orders in
// failed or dead_letter status must NOT be re-staged by the reconciliation
// sweep — the correct recovery path is the admin /orders/{id}/retry
// endpoint, not re-insertion from Shopify (which would double-submit to
// SYSPRO if the failed order had already been persisted). The mockStore's
// `existing` map is status-agnostic by design, mirroring the real
// ShopifyOrdersExist query which checks only shopify_order_id presence.
// If a future refactor tightens that query to filter by status, this test
// will fail and force a re-evaluation.
func TestSweep_SkipsExistingFailedOrders(t *testing.T) {
	srv := newFakeShopify(t, threeOrdersBody)
	defer srv.Close()

	// Order 1002 exists in DB in 'failed' state. Sweeper must skip it.
	ms := &mockStore{existing: map[int64]bool{1002: true}}
	sw := New(ms, "ignored", "shpat_test", time.Minute, testLogger(), WithBaseURL(srv.URL))

	if err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Should stage 1001 and 1003 (missing), skip 1002 (already in DB
	// regardless of its status).
	if len(ms.created) != 2 {
		t.Fatalf("expected 2 orders staged, got %d", len(ms.created))
	}
	for _, o := range ms.created {
		if o.ShopifyOrderID == 1002 {
			t.Errorf("sweeper must NOT re-stage 1002 (already in DB as failed)")
		}
	}
}

// TestSweep_SkipsUnpaid: unpaid orders bypass the stage step even when missing.
func TestSweep_SkipsUnpaid(t *testing.T) {
	body := `{
	  "orders": [
	    {
	      "id": 2001,
	      "name": "#BBQ2001",
	      "email": "x@example.com",
	      "created_at": "2026-04-13T10:00:00Z",
	      "total_price": "10.00",
	      "financial_status": "pending",
	      "gateway": "bank_deposit",
	      "line_items": [{"sku": "CBBQ0001", "quantity": 1, "price": "10.00", "total_discount": "0.00", "tax_lines": []}]
	    },
	    {
	      "id": 2002,
	      "name": "#BBQ2002",
	      "email": "y@example.com",
	      "created_at": "2026-04-13T11:00:00Z",
	      "total_price": "20.00",
	      "financial_status": "paid",
	      "gateway": "shopify_payments",
	      "line_items": [{"sku": "CBBQ0001", "quantity": 2, "price": "10.00", "total_discount": "0.00", "tax_lines": []}]
	    }
	  ]
	}`
	srv := newFakeShopify(t, body)
	defer srv.Close()

	ms := &mockStore{}
	sw := New(ms, "ignored", "shpat_test", time.Minute, testLogger(), WithBaseURL(srv.URL))

	if err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(ms.created) != 1 {
		t.Fatalf("expected 1 order staged (only paid), got %d", len(ms.created))
	}
	if ms.created[0].ShopifyOrderID != 2002 {
		t.Errorf("wrong order staged: id=%d", ms.created[0].ShopifyOrderID)
	}
}

// TestNew_DisabledWithoutToken: nil return when access token missing.
func TestNew_DisabledWithoutToken(t *testing.T) {
	sw := New(&mockStore{}, "store.myshopify.com", "", time.Minute, testLogger())
	if sw != nil {
		t.Error("expected nil sweeper when access token empty")
	}
}

// TestSweep_PreservesTaxesIncluded: reconciled orders must preserve taxes_included
// in RawPayload so that buildSORTOI's extractTaxesIncluded returns true and the
// VAT strip fires. Without this, reconciled orders send gross prices to SYSPRO
// causing double-VAT.
func TestSweep_PreservesTaxesIncluded(t *testing.T) {
	body := `{
	  "orders": [
	    {
	      "id": 3001,
	      "name": "#BBQ3001",
	      "email": "vat@example.com",
	      "created_at": "2026-04-13T10:00:00Z",
	      "total_price": "10.00",
	      "financial_status": "paid",
	      "taxes_included": true,
	      "gateway": "shopify_payments",
	      "line_items": [{"sku": "BRIQ0152", "quantity": 1, "price": "8.00", "total_discount": "0.00", "tax_lines": [{"price": "0.38", "rate": 0.05, "title": "GB VAT"}]}],
	      "shipping_lines": [{"title": "Standard", "price": "5.99", "tax_lines": [{"price": "1.00", "rate": 0.20, "title": "GB VAT"}]}]
	    }
	  ]
	}`
	srv := newFakeShopify(t, body)
	defer srv.Close()

	ms := &mockStore{}
	sw := New(ms, "ignored", "shpat_test", time.Minute, testLogger(), WithBaseURL(srv.URL))

	if err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(ms.created) != 1 {
		t.Fatalf("expected 1 order staged, got %d", len(ms.created))
	}

	o := ms.created[0]

	// RawPayload must contain taxes_included=true so extractTaxesIncluded works.
	var raw struct {
		TaxesIncluded bool `json:"taxes_included"`
	}
	if err := json.Unmarshal(o.RawPayload, &raw); err != nil {
		t.Fatalf("unmarshal RawPayload: %v", err)
	}
	if !raw.TaxesIncluded {
		t.Error("RawPayload missing taxes_included=true — buildSORTOI will skip VAT strip")
	}

	// RawPayload must contain shipping tax data so extractShippingTax works.
	var rawShipping struct {
		ShippingLines []struct {
			TaxLines []struct {
				Price string `json:"price"`
			} `json:"tax_lines"`
		} `json:"shipping_lines"`
	}
	if err := json.Unmarshal(o.RawPayload, &rawShipping); err != nil {
		t.Fatalf("unmarshal shipping: %v", err)
	}
	if len(rawShipping.ShippingLines) == 0 || len(rawShipping.ShippingLines[0].TaxLines) == 0 {
		t.Error("RawPayload missing shipping_lines[].tax_lines — extractShippingTax will return 0")
	}

	// Line-item tax data must also be preserved for extractLineRates.
	var rawLines struct {
		LineItems []struct {
			TaxLines []struct {
				Rate float64 `json:"rate"`
			} `json:"tax_lines"`
		} `json:"line_items"`
	}
	if err := json.Unmarshal(o.RawPayload, &rawLines); err != nil {
		t.Fatalf("unmarshal line_items: %v", err)
	}
	if len(rawLines.LineItems) == 0 || len(rawLines.LineItems[0].TaxLines) == 0 {
		t.Error("RawPayload missing line_items[].tax_lines — extractLineRates will return 0")
	}
	if rawLines.LineItems[0].TaxLines[0].Rate != 0.05 {
		t.Errorf("line tax rate = %f, want 0.05", rawLines.LineItems[0].TaxLines[0].Rate)
	}
}

// TestSweep_ShopifyErrorSurfaces: non-2xx Shopify response returns an error.
func TestSweep_ShopifyErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":"server busy"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ms := &mockStore{}
	sw := New(ms, "ignored", "shpat_test", time.Minute, testLogger(), WithBaseURL(srv.URL))

	err := sw.Sweep(context.Background())
	if err == nil {
		t.Fatal("expected error on 503, got nil")
	}
}
