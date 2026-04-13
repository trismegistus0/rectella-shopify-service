package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/store"
)

// mockStore implements OrderStore with function fields for test control.
type mockStore struct {
	webhookExistsFn func(ctx context.Context, webhookID string) (bool, error)
	createOrderFn   func(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error
}

func (m *mockStore) WebhookExists(ctx context.Context, webhookID string) (bool, error) {
	if m.webhookExistsFn != nil {
		return m.webhookExistsFn(ctx, webhookID)
	}
	return false, nil
}

func (m *mockStore) CreateOrder(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error {
	if m.createOrderFn != nil {
		return m.createOrderFn(ctx, event, order, lines)
	}
	return nil
}

const testSecret = "test-webhook-secret"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func signBody(body string) string {
	return computeHMAC([]byte(body), testSecret)
}

func newRequest(body string, webhookID string, hmacSig string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/orders/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if webhookID != "" {
		req.Header.Set("X-Shopify-Webhook-Id", webhookID)
	}
	if hmacSig != "" {
		req.Header.Set("X-Shopify-Hmac-Sha256", hmacSig)
	}
	return req
}

var validPayload = `{
	"id": 5551234567890,
	"name": "#BBQ1001",
	"email": "john@example.com",
	"created_at": "2026-02-24T14:30:00Z",
	"total_price": "748.00",
	"gateway": "shopify_payments",
	"shipping_address": {
		"first_name": "John",
		"last_name": "Smith",
		"address1": "42 Bancroft Road",
		"address2": "",
		"city": "Burnley",
		"province": "Lancashire",
		"zip": "BB10 2TP",
		"country": "United Kingdom",
		"phone": "+441282478200"
	},
	"line_items": [
		{
			"sku": "CBBQ0001",
			"quantity": 1,
			"price": "599.00",
			"total_discount": "0.00",
			"tax_lines": [{"price": "119.80", "rate": 0.2, "title": "VAT"}]
		},
		{
			"sku": "MBBQ0159",
			"quantity": 1,
			"price": "149.00",
			"total_discount": "0.00",
			"tax_lines": [{"price": "29.80", "rate": 0.2, "title": "VAT"}]
		}
	]
}`

var noAddressPayload = `{
	"id": 5551234567890,
	"name": "#BBQ1002",
	"email": "john@example.com",
	"created_at": "2026-02-24T14:30:00Z",
	"total_price": "599.00",
	"gateway": "shopify_payments",
	"shipping_address": null,
	"line_items": [
		{
			"sku": "CBBQ0001",
			"quantity": 1,
			"price": "599.00",
			"total_discount": "0.00",
			"tax_lines": [{"price": "119.80", "rate": 0.2, "title": "VAT"}]
		}
	]
}`

func TestHandleOrderCreate(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		webhookID      string
		signBody       bool
		hmacOverride   string
		store          *mockStore
		wantStatus     int
		wantCreateCall bool
		checkOrder     func(t *testing.T, order model.Order, lines []model.OrderLine)
	}{
		{
			name:           "valid order",
			body:           validPayload,
			webhookID:      "wh-001",
			signBody:       true,
			store:          &mockStore{},
			wantStatus:     http.StatusOK,
			wantCreateCall: true,
			checkOrder: func(t *testing.T, order model.Order, lines []model.OrderLine) {
				if order.ShopifyOrderID != 5551234567890 {
					t.Errorf("ShopifyOrderID = %d, want 5551234567890", order.ShopifyOrderID)
				}
				if order.OrderNumber != "#BBQ1001" {
					t.Errorf("OrderNumber = %q, want %q", order.OrderNumber, "#BBQ1001")
				}
				if order.CustomerAccount != "WEBS01" {
					t.Errorf("CustomerAccount = %q, want %q", order.CustomerAccount, "WEBS01")
				}
				if order.PaymentReference != "shopify_payments" {
					t.Errorf("PaymentReference = %q, want %q", order.PaymentReference, "shopify_payments")
				}
				if order.PaymentAmount != 748.00 {
					t.Errorf("PaymentAmount = %f, want 748.00", order.PaymentAmount)
				}
				if order.ShipCity != "Burnley" {
					t.Errorf("ShipCity = %q, want %q", order.ShipCity, "Burnley")
				}
				if order.ShipPostcode != "BB10 2TP" {
					t.Errorf("ShipPostcode = %q, want %q", order.ShipPostcode, "BB10 2TP")
				}
				if order.Status != model.OrderStatusPending {
					t.Errorf("Status = %q, want %q", order.Status, model.OrderStatusPending)
				}
				if len(lines) != 2 {
					t.Fatalf("len(lines) = %d, want 2", len(lines))
				}
				if lines[0].SKU != "CBBQ0001" {
					t.Errorf("lines[0].SKU = %q, want %q", lines[0].SKU, "CBBQ0001")
				}
				if lines[0].UnitPrice != 599.00 {
					t.Errorf("lines[0].UnitPrice = %f, want 599.00", lines[0].UnitPrice)
				}
				if lines[0].Tax != 119.80 {
					t.Errorf("lines[0].Tax = %f, want 119.80", lines[0].Tax)
				}
				if lines[1].SKU != "MBBQ0159" {
					t.Errorf("lines[1].SKU = %q, want %q", lines[1].SKU, "MBBQ0159")
				}
			},
		},
		{
			name:         "invalid HMAC",
			body:         validPayload,
			webhookID:    "wh-002",
			hmacOverride: "aW52YWxpZA==",
			store:        &mockStore{},
			wantStatus:   http.StatusUnauthorized,
		},
		{
			name:       "missing HMAC header",
			body:       validPayload,
			webhookID:  "wh-003",
			store:      &mockStore{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:      "duplicate webhook",
			body:      validPayload,
			webhookID: "wh-004",
			signBody:  true,
			store: &mockStore{
				webhookExistsFn: func(ctx context.Context, webhookID string) (bool, error) {
					return true, nil
				},
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing webhook ID",
			body:       validPayload,
			signBody:   true,
			store:      &mockStore{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed JSON",
			body:       `{not json`,
			webhookID:  "wh-006",
			signBody:   true,
			store:      &mockStore{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty line_items",
			body:       `{"id": 123, "name": "#BBQ1003", "line_items": []}`,
			webhookID:  "wh-007",
			signBody:   true,
			store:      &mockStore{},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "zero order ID",
			body:       `{"id": 0, "name": "#BBQ1004", "line_items": [{"sku": "X", "quantity": 1, "price": "10.00"}]}`,
			webhookID:  "wh-008",
			signBody:   true,
			store:      &mockStore{},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:      "store error",
			body:      validPayload,
			webhookID: "wh-009",
			signBody:  true,
			store: &mockStore{
				createOrderFn: func(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error {
					return errors.New("database connection lost")
				},
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:      "duplicate webhook race condition",
			body:      validPayload,
			webhookID: "wh-011",
			signBody:  true,
			store: &mockStore{
				createOrderFn: func(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error {
					return store.ErrDuplicateWebhook
				},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "order with shipping lines",
			body: `{
				"id": 5551234567891,
				"name": "#BBQ1010",
				"email": "test@example.com",
				"created_at": "2026-02-24T14:30:00Z",
				"total_price": "604.99",
				"gateway": "shopify_payments",
				"line_items": [
					{"sku": "CBBQ0001", "quantity": 1, "price": "599.00", "total_discount": "0.00", "tax_lines": []}
				],
				"shipping_lines": [
					{"title": "Standard Shipping", "price": "5.99"}
				]
			}`,
			webhookID:      "wh-012",
			signBody:       true,
			store:          &mockStore{},
			wantStatus:     http.StatusOK,
			wantCreateCall: true,
			checkOrder: func(t *testing.T, order model.Order, lines []model.OrderLine) {
				if order.ShippingAmount != 5.99 {
					t.Errorf("ShippingAmount = %f, want 5.99", order.ShippingAmount)
				}
			},
		},
		{
			name:           "nil shipping address",
			body:           noAddressPayload,
			webhookID:      "wh-010",
			signBody:       true,
			store:          &mockStore{},
			wantStatus:     http.StatusOK,
			wantCreateCall: true,
			checkOrder: func(t *testing.T, order model.Order, lines []model.OrderLine) {
				if order.ShipFirstName != "" {
					t.Errorf("ShipFirstName = %q, want empty", order.ShipFirstName)
				}
				if order.ShipCity != "" {
					t.Errorf("ShipCity = %q, want empty", order.ShipCity)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var createdOrder model.Order
			var createdLines []model.OrderLine
			createCalled := false

			if tt.wantCreateCall {
				orig := tt.store.createOrderFn
				tt.store.createOrderFn = func(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error {
					createCalled = true
					createdOrder = order
					createdLines = lines
					if orig != nil {
						return orig(ctx, event, order, lines)
					}
					return nil
				}
			}

			h := NewHandler(tt.store, testSecret, nil, testLogger())

			var sig string
			if tt.signBody {
				sig = signBody(tt.body)
			} else if tt.hmacOverride != "" {
				sig = tt.hmacOverride
			}

			req := newRequest(tt.body, tt.webhookID, sig)
			rec := httptest.NewRecorder()

			mux := http.NewServeMux()
			h.Register(mux)
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			// Verify JSON response.
			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response is not valid JSON: %v", err)
			}

			if tt.wantCreateCall && !createCalled {
				t.Error("expected CreateOrder to be called, but it was not")
			}

			if tt.checkOrder != nil && createCalled {
				tt.checkOrder(t, createdOrder, createdLines)
			}
		})
	}
}
