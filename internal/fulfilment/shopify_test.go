package fulfilment

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

func testClient(t *testing.T, handler http.HandlerFunc) *FulfilmentClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewFulfilmentClient("test.myshopify.com", "shpat_test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = srv.URL
	c.httpClient = srv.Client()
	return c
}

func shopifyHandler(t *testing.T, response string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Shopify-Access-Token") != "shpat_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(response))
	}
}

func TestGetFulfillmentOrderID_Success(t *testing.T) {
	resp := `{"data":{"order":{"fulfillmentOrders":{"edges":[{"node":{"id":"gid://shopify/FulfillmentOrder/111","status":"OPEN"}}]}}}}`
	c := testClient(t, shopifyHandler(t, resp))
	id, err := c.GetFulfillmentOrderID(context.Background(), 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "gid://shopify/FulfillmentOrder/111" {
		t.Errorf("got %q, want gid://shopify/FulfillmentOrder/111", id)
	}
}

func TestGetFulfillmentOrderID_NoOrders(t *testing.T) {
	resp := `{"data":{"order":{"fulfillmentOrders":{"edges":[]}}}}`
	c := testClient(t, shopifyHandler(t, resp))
	_, err := c.GetFulfillmentOrderID(context.Background(), 12345)
	if err == nil {
		t.Fatal("expected error for no fulfillment orders, got nil")
	}
	if !strings.Contains(err.Error(), "no open fulfillment orders") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestGetFulfillmentOrderID_SkipsClosed(t *testing.T) {
	resp := `{"data":{"order":{"fulfillmentOrders":{"edges":[{"node":{"id":"gid://shopify/FulfillmentOrder/100","status":"CLOSED"}},{"node":{"id":"gid://shopify/FulfillmentOrder/101","status":"CANCELLED"}},{"node":{"id":"gid://shopify/FulfillmentOrder/102","status":"OPEN"}}]}}}}`
	c := testClient(t, shopifyHandler(t, resp))
	id, err := c.GetFulfillmentOrderID(context.Background(), 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "gid://shopify/FulfillmentOrder/102" {
		t.Errorf("got %q, want gid://shopify/FulfillmentOrder/102", id)
	}
}

func TestCreateFulfillment_Success(t *testing.T) {
	resp := `{"data":{"fulfillmentCreate":{"fulfillment":{"id":"gid://shopify/Fulfillment/999","status":"SUCCESS"},"userErrors":[]}}}`
	c := testClient(t, shopifyHandler(t, resp))
	id, err := c.CreateFulfillment(context.Background(), FulfilmentInput{
		FulfillmentOrderID: "gid://shopify/FulfillmentOrder/111",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "gid://shopify/Fulfillment/999" {
		t.Errorf("got %q, want gid://shopify/Fulfillment/999", id)
	}
}

func TestCreateFulfillment_WithTracking(t *testing.T) {
	var capturedBody []byte
	handler := func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"fulfillmentCreate":{"fulfillment":{"id":"gid://shopify/Fulfillment/999","status":"SUCCESS"},"userErrors":[]}}}`))
	}
	c := testClient(t, handler)
	_, err := c.CreateFulfillment(context.Background(), FulfilmentInput{
		FulfillmentOrderID: "gid://shopify/FulfillmentOrder/111",
		TrackingNumber:     "TRACK123",
		Carrier:            "Royal Mail",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var req struct {
		Variables struct {
			Fulfillment map[string]any `json:"fulfillment"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(capturedBody, &req); err != nil {
		t.Fatalf("failed to unmarshal captured body: %v", err)
	}
	tracking, ok := req.Variables.Fulfillment["trackingInfo"].(map[string]any)
	if !ok {
		t.Fatal("expected trackingInfo in request variables")
	}
	if tracking["number"] != "TRACK123" {
		t.Errorf("tracking number = %v, want TRACK123", tracking["number"])
	}
	if tracking["company"] != "Royal Mail" {
		t.Errorf("tracking company = %v, want Royal Mail", tracking["company"])
	}
}

func TestCreateFulfillment_AlreadyFulfilled(t *testing.T) {
	resp := `{"data":{"fulfillmentCreate":{"fulfillment":null,"userErrors":[{"field":["fulfillmentOrderId"],"message":"This order has already been fulfilled"}]}}}`
	c := testClient(t, shopifyHandler(t, resp))
	id, err := c.CreateFulfillment(context.Background(), FulfilmentInput{
		FulfillmentOrderID: "gid://shopify/FulfillmentOrder/111",
	})
	if err != nil {
		t.Fatalf("expected nil error for already fulfilled, got: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty ID for already fulfilled, got %q", id)
	}
}

func TestCreateFulfillment_UserError(t *testing.T) {
	resp := `{"data":{"fulfillmentCreate":{"fulfillment":null,"userErrors":[{"field":["base"],"message":"Invalid fulfillment order"}]}}}`
	c := testClient(t, shopifyHandler(t, resp))
	_, err := c.CreateFulfillment(context.Background(), FulfilmentInput{
		FulfillmentOrderID: "gid://shopify/FulfillmentOrder/111",
	})
	if err == nil {
		t.Fatal("expected error for user error, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid fulfillment order") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFulfilmentClient_BaseURLOverride(t *testing.T) {
	resp := `{"data":{"order":{"fulfillmentOrders":{"edges":[{"node":{"id":"gid://shopify/FulfillmentOrder/111","status":"OPEN"}}]}}}}`
	c := testClient(t, shopifyHandler(t, resp))
	// testClient already overrides baseURL directly; verify constructor-level override works too.
	srv := httptest.NewServer(shopifyHandler(t, resp))
	defer srv.Close()
	c2 := NewFulfilmentClient("test.myshopify.com", "shpat_test",
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithFulfilmentBaseURL(srv.URL))
	c2.httpClient = srv.Client()
	id, err := c2.GetFulfillmentOrderID(context.Background(), 12345)
	if err != nil {
		t.Fatalf("unexpected error with WithFulfilmentBaseURL: %v", err)
	}
	_ = c
	if id != "gid://shopify/FulfillmentOrder/111" {
		t.Errorf("got %q, want gid://shopify/FulfillmentOrder/111", id)
	}
}

func TestCreateFulfillment_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewFulfilmentClient("test.myshopify.com", "shpat_test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = srv.URL
	c.httpClient = srv.Client()
	_, err := c.CreateFulfillment(context.Background(), FulfilmentInput{
		FulfillmentOrderID: "gid://shopify/FulfillmentOrder/111",
	})
	if err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
}
