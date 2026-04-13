package inventory

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeShopify struct {
	server          *httptest.Server
	calls           int
	lastQuery       string
	lastVariables   map[string]any
	locationResp    string
	inventoryResp   string
	setQuantityResp string
}

func newFakeShopify(t *testing.T) *fakeShopify {
	t.Helper()
	f := &fakeShopify{
		locationResp:    `{"data":{"locations":{"edges":[{"node":{"id":"gid://shopify/Location/123","name":"Main","isActive":true}}]}}}`,
		inventoryResp:   `{"data":{"inventoryItems":{"edges":[{"node":{"id":"gid://shopify/InventoryItem/1001","sku":"CBBQ0001"}},{"node":{"id":"gid://shopify/InventoryItem/1002","sku":"CBBQ0002"}}]}}}`,
		setQuantityResp: `{"data":{"inventorySetQuantities":{"inventoryAdjustmentGroup":{"reason":"correction","changes":[{"name":"available","delta":10,"quantityAfterChange":120}]},"userErrors":[]}}}`,
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := r.Header.Get("X-Shopify-Access-Token")
		if token != "shpat_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		f.lastQuery = req.Query
		f.lastVariables = req.Variables
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "locations("):
			w.Write([]byte(f.locationResp))
		case strings.Contains(req.Query, "inventoryItems("):
			w.Write([]byte(f.inventoryResp))
		case strings.Contains(req.Query, "inventorySetQuantities"):
			w.Write([]byte(f.setQuantityResp))
		default:
			http.Error(w, "unknown query", http.StatusBadRequest)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeShopify) client(t *testing.T, skus []string) *ShopifyClient {
	t.Helper()
	c := NewShopifyClient("test.myshopify.com", "shpat_test", "", skus,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = f.server.URL
	c.httpClient = f.server.Client()
	return c
}

func TestShopifyClient_SetInventoryLevels_Success(t *testing.T) {
	fake := newFakeShopify(t)
	c := fake.client(t, []string{"CBBQ0001", "CBBQ0002"})
	err := c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 120, "CBBQ0002": 45})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.calls != 3 {
		t.Errorf("expected 3 API calls (location + inventory items + set), got %d", fake.calls)
	}
	if !strings.Contains(fake.lastQuery, "inventorySetQuantities") {
		t.Errorf("last query should be setQuantities mutation")
	}
}

func TestShopifyClient_SetInventoryLevels_CachesLocation(t *testing.T) {
	fake := newFakeShopify(t)
	c := fake.client(t, []string{"CBBQ0001"})
	_ = c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100})
	callsAfterFirst := fake.calls
	_ = c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 110})
	if fake.calls-callsAfterFirst != 1 {
		t.Errorf("expected 1 call on second sync (cached), got %d", fake.calls-callsAfterFirst)
	}
}

func TestShopifyClient_SetInventoryLevels_ConfiguredLocation(t *testing.T) {
	fake := newFakeShopify(t)
	c := NewShopifyClient("test.myshopify.com", "shpat_test", "456", []string{"CBBQ0001"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = fake.server.URL
	c.httpClient = fake.server.Client()
	_ = c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100})
	if fake.calls != 2 {
		t.Errorf("expected 2 API calls (skip location discovery), got %d", fake.calls)
	}
}

func TestShopifyClient_SetInventoryLevels_UnresolvedSKU(t *testing.T) {
	fake := newFakeShopify(t)
	c := fake.client(t, []string{"CBBQ0001", "CBBQ0002"})
	err := c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100, "CBBQ9999": 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestShopifyClient_SetInventoryLevels_NoSKUsResolved(t *testing.T) {
	fake := newFakeShopify(t)
	fake.inventoryResp = `{"data":{"inventoryItems":{"edges":[]}}}`
	c := fake.client(t, []string{"UNKNOWN"})
	err := c.SetInventoryLevels(context.Background(), map[string]int{"UNKNOWN": 50})
	if err != nil {
		t.Fatalf("unexpected error (should succeed with warning): %v", err)
	}
}

func TestShopifyClient_SetInventoryLevels_UserErrors(t *testing.T) {
	fake := newFakeShopify(t)
	fake.setQuantityResp = `{"data":{"inventorySetQuantities":{"inventoryAdjustmentGroup":null,"userErrors":[{"code":"UNTRACKED","field":["quantities","0"],"message":"Item is not tracked"}]}}}`
	c := fake.client(t, []string{"CBBQ0001"})
	err := c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100})
	if err == nil {
		t.Fatal("expected error when Shopify returns userErrors, got nil")
	}
	if !strings.Contains(err.Error(), "UNTRACKED") {
		t.Errorf("error should contain error code, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Item is not tracked") {
		t.Errorf("error should contain message, got: %v", err)
	}
}

func TestShopifyClient_BaseURLOverride(t *testing.T) {
	fake := newFakeShopify(t)
	c := NewShopifyClient("test.myshopify.com", "shpat_test", "", []string{"CBBQ0001"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = fake.server.URL
	c.httpClient = fake.server.Client()
	// With override, the client should use the provided URL, not construct from storeURL.
	// This test already works because unit tests override baseURL directly.
	// The real change is accepting baseURL via constructor for out-of-process usage.
	err := c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Now test the constructor-level override.
	c2 := NewShopifyClient("test.myshopify.com", "shpat_test", "", []string{"CBBQ0001"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithBaseURL(fake.server.URL))
	c2.httpClient = fake.server.Client()
	err = c2.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 50})
	if err != nil {
		t.Fatalf("unexpected error with WithBaseURL: %v", err)
	}
}

func TestShopifyClient_SetInventoryLevels_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewShopifyClient("test.myshopify.com", "shpat_test", "", []string{"CBBQ0001"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = srv.URL
	c.httpClient = srv.Client()
	err := c.SetInventoryLevels(context.Background(), map[string]int{"CBBQ0001": 100})
	if err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
}
