//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/batch"
	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/store"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
	"github.com/trismegistus0/rectella-shopify-service/internal/webhook"
)

// newTestServerWithRetry creates a test server that includes the POST /orders/{id}/retry
// endpoint, mirroring the production setup in main.go. This is a separate constructor
// rather than modifying newTestServer, to avoid changing existing tests.
func newTestServerWithRetry(t *testing.T) *testServer {
	t.Helper()
	ctx := t.Context()

	db, err := store.New(ctx, testDBURL)
	if err != nil {
		t.Fatalf("connecting to test DB: %v", err)
	}

	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	_, err = db.Pool.Exec(ctx, "TRUNCATE order_lines, orders, webhook_events RESTART IDENTITY CASCADE")
	if err != nil {
		t.Fatalf("truncating tables: %v", err)
	}

	secret := "integration-test-secret"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mock := &mockSysproClient{
		defaultResult: &syspro.SalesOrderResult{
			Success:           true,
			SysproOrderNumber: "SO-TEST-001",
		},
	}

	batchProc := batch.New(db, mock, time.Minute, logger)

	mux := http.NewServeMux()

	// Health endpoints.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	})

	// Webhook handler.
	wh := webhook.NewHandler(db, secret, nil, logger)
	wh.Register(mux)

	// Orders endpoint.
	validStatuses := map[string]bool{
		"pending": true, "processing": true, "submitted": true,
		"failed": true, "dead_letter": true, "cancelled": true,
	}
	mux.HandleFunc("GET /orders", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		statusFilter := r.URL.Query().Get("status")
		if statusFilter == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "status query parameter required"})
			return
		}
		if !validStatuses[statusFilter] {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid status value"})
			return
		}

		orders, err := db.ListOrdersByStatus(r.Context(), model.OrderStatus(statusFilter))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
			return
		}

		type orderResponse struct {
			ID              int64             `json:"id"`
			ShopifyOrderID  int64             `json:"shopify_order_id"`
			OrderNumber     string            `json:"order_number"`
			Status          model.OrderStatus `json:"status"`
			CustomerAccount string            `json:"customer_account"`
			Attempts        int               `json:"attempts"`
			LastError       string            `json:"last_error,omitempty"`
			OrderDate       string            `json:"order_date"`
			CreatedAt       string            `json:"created_at"`
			UpdatedAt       string            `json:"updated_at"`
		}

		resp := make([]orderResponse, 0, len(orders))
		for _, o := range orders {
			resp = append(resp, orderResponse{
				ID:              o.ID,
				ShopifyOrderID:  o.ShopifyOrderID,
				OrderNumber:     o.OrderNumber,
				Status:          o.Status,
				CustomerAccount: o.CustomerAccount,
				Attempts:        o.Attempts,
				LastError:       o.LastError,
				OrderDate:       o.OrderDate.Format(time.RFC3339),
				CreatedAt:       o.CreatedAt.Format(time.RFC3339),
				UpdatedAt:       o.UpdatedAt.Format(time.RFC3339),
			})
		}

		json.NewEncoder(w).Encode(resp)
	})

	// Retry endpoint — mirrors main.go.
	mux.HandleFunc("POST /orders/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		idStr := r.PathValue("id")
		var orderID int64
		if _, err := fmt.Sscanf(idStr, "%d", &orderID); err != nil || orderID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid order ID"})
			return
		}

		if err := db.RetryOrder(r.Context(), orderID); err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	})

	srv := httptest.NewServer(mux)

	t.Cleanup(func() {
		srv.Close()
		db.Close()
	})

	return &testServer{
		DB:     db,
		Server: srv,
		Batch:  batchProc,
		Mock:   mock,
		Secret: secret,
	}
}

// post performs a POST request against the test server with an optional JSON body.
func (ts *testServer) post(t *testing.T, path string, body any) *http.Response {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest("POST", ts.Server.URL+path, reader)
	if err != nil {
		t.Fatalf("creating POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}
