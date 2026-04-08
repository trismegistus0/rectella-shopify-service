//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"codeberg.org/speeder091/rectella-shopify-service/internal/batch"
	"codeberg.org/speeder091/rectella-shopify-service/internal/model"
	"codeberg.org/speeder091/rectella-shopify-service/internal/store"
	"codeberg.org/speeder091/rectella-shopify-service/internal/syspro"
	"codeberg.org/speeder091/rectella-shopify-service/internal/webhook"
	"github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// testDBURL is set by TestMain to the Postgres container's connection string.
var testDBURL string

// testPort is a non-default Postgres port to avoid conflicts with the project's
// dev Postgres (which uses 5432 via docker-compose).
const testPort = "5433"

func TestMain(m *testing.M) {
	// Disable Ryuk (container reaper) — it uses Docker bridge networking which is
	// broken on kernel 6.18+ with nftables. We terminate the container ourselves.
	os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")

	ctx := context.Background()

	pgContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "postgres:16",
			Env: map[string]string{
				"POSTGRES_USER":     "test",
				"POSTGRES_PASSWORD": "test",
				"POSTGRES_DB":       "rectella_integration",
				"PGPORT":            testPort,
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.NetworkMode = "host"
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		log.Fatalf("starting postgres container: %v", err)
	}

	testDBURL = fmt.Sprintf("postgres://test:test@localhost:%s/rectella_integration?sslmode=disable", testPort)

	code := m.Run()

	if err := pgContainer.Terminate(ctx); err != nil {
		log.Printf("terminating postgres container: %v", err)
	}
	os.Exit(code)
}

// testServer wraps everything needed to test the full pipeline.
type testServer struct {
	DB     *store.DB
	Server *httptest.Server
	Batch  *batch.Processor
	Mock   *mockSysproClient
	Secret string
}

// newTestServer creates a fully wired test server backed by the shared Postgres container.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ctx := context.Background()

	db, err := store.New(ctx, testDBURL)
	if err != nil {
		t.Fatalf("connecting to test DB: %v", err)
	}

	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	// Truncate data tables for a clean test.
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

	// Orders endpoint (mirrors main.go).
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

// sendWebhook sends a signed webhook request to the test server.
func (ts *testServer) sendWebhook(t *testing.T, webhookID string, payload []byte) *http.Response {
	t.Helper()

	mac := hmac.New(sha256.New, []byte(ts.Secret))
	mac.Write(payload)
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest("POST", ts.Server.URL+"/webhooks/orders/create", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("creating webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Webhook-Id", webhookID)
	req.Header.Set("X-Shopify-Hmac-Sha256", sig)

	resp, err := ts.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("sending webhook: %v", err)
	}
	return resp
}

// get performs a GET request against the test server.
func (ts *testServer) get(t *testing.T, path string) *http.Response {
	t.Helper()

	resp, err := ts.Server.Client().Get(ts.Server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// orderPayload builds a minimal Shopify order JSON for testing.
func orderPayload(shopifyID int64, orderName string) []byte {
	p := map[string]any{
		"id":          shopifyID,
		"name":        orderName,
		"email":       "test@example.com",
		"created_at":  "2026-03-15T10:00:00Z",
		"total_price": "149.00",
		"gateway":     "shopify_payments",
		"shipping_address": map[string]string{
			"first_name": "Test",
			"last_name":  "User",
			"address1":   "1 Test Street",
			"city":       "Burnley",
			"province":   "Lancashire",
			"zip":        "BB10 1AA",
			"country":    "United Kingdom",
		},
		"line_items": []map[string]any{
			{
				"sku":            "CBBQ0001",
				"quantity":       1,
				"price":          "149.00",
				"total_discount": "0.00",
				"tax_lines": []map[string]any{
					{"price": "29.80", "rate": 0.2, "title": "VAT"},
				},
			},
		},
	}
	b, _ := json.Marshal(p)
	return b
}

// mockSysproClient implements syspro.Client with controllable behaviour.
type mockSysproClient struct {
	mu            sync.Mutex
	submitFunc    func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error)
	openErr       error
	defaultResult *syspro.SalesOrderResult
}

func (c *mockSysproClient) OpenSession(ctx context.Context) (syspro.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openErr != nil {
		return nil, c.openErr
	}
	return &mockSysproSession{client: c}, nil
}

func (c *mockSysproClient) SubmitSalesOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
	c.mu.Lock()
	fn := c.submitFunc
	def := c.defaultResult
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, order, lines)
	}
	return def, nil
}

// setSubmitFunc configures the mock's per-order behaviour. Thread-safe.
func (c *mockSysproClient) setSubmitFunc(fn func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submitFunc = fn
}

type mockSysproSession struct {
	client *mockSysproClient
}

func (s *mockSysproSession) SubmitOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
	s.client.mu.Lock()
	fn := s.client.submitFunc
	def := s.client.defaultResult
	s.client.mu.Unlock()
	if fn != nil {
		return fn(ctx, order, lines)
	}
	return def, nil
}

func (s *mockSysproSession) Close(ctx context.Context) error {
	return nil
}
