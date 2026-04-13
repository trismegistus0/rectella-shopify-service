package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type mockShopify struct {
	port            int
	server          *http.Server
	inventoryCalls  atomic.Int64
	fulfilmentCalls atomic.Int64
}

func newMockShopify(port int) *mockShopify {
	m := &mockShopify{port: port}
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/api/2025-04/graphql.json", m.handleGraphQL)
	m.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return m
}

func (m *mockShopify) start() error {
	go func() { _ = m.server.ListenAndServe() }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", m.port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("mock Shopify did not start on port %d within 2s", m.port)
}

func (m *mockShopify) stop() { _ = m.server.Close() }

func (m *mockShopify) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Shopify-Access-Token") == "" {
		http.Error(w, `{"errors":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"errors":"bad request"}`, http.StatusBadRequest)
		return
	}

	var req struct {
		Query     string          `json:"query"`
		Variables json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"errors":"invalid json"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch {
	case strings.Contains(req.Query, "locations("):
		_, _ = fmt.Fprint(w, `{"data":{"locations":{"edges":[{"node":{"id":"gid://shopify/Location/mock-loc-1","name":"Mock Warehouse","isActive":true}}]}}}`)

	case strings.Contains(req.Query, "inventoryItems("):
		_, _ = fmt.Fprint(w, `{"data":{"inventoryItems":{"edges":[{"node":{"id":"gid://shopify/InventoryItem/mock-inv-1","sku":"CBBQ0001"}},{"node":{"id":"gid://shopify/InventoryItem/mock-inv-2","sku":"MBBQ0159"}}]}}}`)

	case strings.Contains(req.Query, "inventorySetQuantities"):
		m.inventoryCalls.Add(1)
		_, _ = fmt.Fprint(w, `{"data":{"inventorySetQuantities":{"inventoryAdjustmentGroup":{"reason":"correction","changes":[{"name":"available","delta":0,"quantityAfterChange":100}]},"userErrors":[]}}}`)

	case strings.Contains(req.Query, "fulfillmentOrders("):
		_, _ = fmt.Fprint(w, `{"data":{"order":{"fulfillmentOrders":{"edges":[{"node":{"id":"gid://shopify/FulfillmentOrder/mock-fo-1","status":"OPEN"}}]}}}}`)

	case strings.Contains(req.Query, "fulfillmentCreate"):
		m.fulfilmentCalls.Add(1)
		_, _ = fmt.Fprint(w, `{"data":{"fulfillmentCreate":{"fulfillment":{"id":"gid://shopify/Fulfillment/mock-ful-1","status":"SUCCESS"},"userErrors":[]}}}`)

	default:
		http.Error(w, `{"errors":"unknown query"}`, http.StatusBadRequest)
	}
}
