package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/store"
)

// OrderStore is the persistence interface consumed by the webhook handler.
// *store.DB satisfies this implicitly.
type OrderStore interface {
	WebhookExists(ctx context.Context, webhookID string) (bool, error)
	CreateOrder(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error
}

// Handler processes inbound Shopify webhooks.
type Handler struct {
	store     OrderStore
	secret    string
	triggerCh chan<- struct{}
	logger    *slog.Logger
}

// NewHandler creates a webhook handler with the given store, HMAC secret, and logger.
// triggerCh signals the stock syncer when a new order arrives (nil disables triggering).
func NewHandler(store OrderStore, secret string, triggerCh chan<- struct{}, logger *slog.Logger) *Handler {
	return &Handler{
		store:     store,
		secret:    secret,
		triggerCh: triggerCh,
		logger:    logger,
	}
}

// Register adds webhook routes to the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/orders/create", h.handleOrderCreate)
}

const maxBodySize = 1 << 20 // 1 MB

func (h *Handler) handleOrderCreate(w http.ResponseWriter, r *http.Request) {
	// Read body with size limit.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		h.respond(w, http.StatusBadRequest, "error", "failed to read body")
		return
	}

	// Verify HMAC signature.
	signature := r.Header.Get("X-Shopify-Hmac-Sha256")
	if !VerifyHMAC(body, h.secret, signature) {
		h.logger.Warn("webhook HMAC verification failed", "remote_addr", r.RemoteAddr)
		h.respond(w, http.StatusUnauthorized, "error", "unauthorized")
		return
	}

	// Extract webhook ID for idempotency.
	webhookID := r.Header.Get("X-Shopify-Webhook-Id")
	if webhookID == "" {
		h.respond(w, http.StatusBadRequest, "error", "missing webhook ID")
		return
	}

	// Check for duplicate webhook.
	exists, err := h.store.WebhookExists(r.Context(), webhookID)
	if err != nil {
		h.logger.Error("checking webhook existence", "error", err, "webhook_id", webhookID)
		h.respond(w, http.StatusInternalServerError, "error", "internal error")
		return
	}
	if exists {
		h.logger.Info("duplicate webhook ignored", "webhook_id", webhookID)
		h.respond(w, http.StatusOK, "status", "ok")
		return
	}

	// Parse payload.
	var payload shopifyOrder
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger.Warn("malformed webhook payload", "error", err, "webhook_id", webhookID)
		h.respond(w, http.StatusBadRequest, "error", "malformed JSON")
		return
	}

	// Validate required fields.
	if payload.ID == 0 {
		h.logger.Warn("webhook missing order ID", "webhook_id", webhookID)
		h.respond(w, http.StatusUnprocessableEntity, "error", "missing order ID")
		return
	}
	if len(payload.LineItems) == 0 {
		h.logger.Warn("webhook has no line items", "webhook_id", webhookID, "shopify_order_id", payload.ID)
		h.respond(w, http.StatusUnprocessableEntity, "error", "no line items")
		return
	}

	// Validate line items.
	for i, li := range payload.LineItems {
		if li.SKU == "" {
			h.logger.Warn("webhook line item missing SKU", "webhook_id", webhookID, "shopify_order_id", payload.ID, "line", i+1)
			h.respond(w, http.StatusUnprocessableEntity, "error", fmt.Sprintf("line item %d: missing SKU", i+1))
			return
		}
		if li.Quantity <= 0 {
			h.logger.Warn("webhook line item invalid quantity", "webhook_id", webhookID, "shopify_order_id", payload.ID, "line", i+1, "quantity", li.Quantity)
			h.respond(w, http.StatusUnprocessableEntity, "error", fmt.Sprintf("line item %d: invalid quantity", i+1))
			return
		}
		if price, err := strconv.ParseFloat(li.Price, 64); err == nil && price < 0 {
			h.logger.Warn("webhook line item negative price", "webhook_id", webhookID, "shopify_order_id", payload.ID, "line", i+1, "price", price)
			h.respond(w, http.StatusUnprocessableEntity, "error", fmt.Sprintf("line item %d: negative price", i+1))
			return
		}
	}

	// Map to domain types.
	order, lines := mapOrder(payload, body)

	// Build webhook event.
	event := model.WebhookEvent{
		WebhookID: webhookID,
		Topic:     "orders/create",
	}

	// Persist.
	if err := h.store.CreateOrder(r.Context(), event, order, lines); err != nil {
		if errors.Is(err, store.ErrDuplicateWebhook) {
			h.logger.Info("duplicate webhook (race)", "webhook_id", webhookID)
			h.respond(w, http.StatusOK, "status", "ok")
			return
		}
		h.logger.Error("persisting order", "error", err, "webhook_id", webhookID)
		h.respond(w, http.StatusInternalServerError, "error", "internal error")
		return
	}

	h.logger.Info("order staged",
		"webhook_id", webhookID,
		"shopify_order_id", payload.ID,
		"order_number", payload.Name,
		"line_items", len(payload.LineItems),
	)

	// Signal stock syncer that a new order arrived (non-blocking).
	if h.triggerCh != nil {
		select {
		case h.triggerCh <- struct{}{}:
		default:
		}
	}

	h.respond(w, http.StatusOK, "status", "ok")
}

func (h *Handler) respond(w http.ResponseWriter, status int, key, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{key: value})
}

// mapOrder converts a Shopify webhook payload into domain Order and OrderLine types.
func mapOrder(p shopifyOrder, rawPayload []byte) (model.Order, []model.OrderLine) {
	order := model.Order{
		ShopifyOrderID:  p.ID,
		OrderNumber:     p.Name,
		Status:          model.OrderStatusPending,
		CustomerAccount: "WEBS01",
		ShipEmail:       p.Email,
		RawPayload:      rawPayload,
	}

	// Payment reference: use gateway, fall back to joined payment_gateway_names.
	if p.Gateway != "" {
		order.PaymentReference = p.Gateway
	} else if len(p.PaymentGatewayNames) > 0 {
		order.PaymentReference = strings.Join(p.PaymentGatewayNames, ", ")
	}

	// Payment amount.
	if v, err := strconv.ParseFloat(p.TotalPrice, 64); err == nil {
		order.PaymentAmount = v
	}

	// Order date.
	if t, err := time.Parse(time.RFC3339, p.CreatedAt); err == nil {
		order.OrderDate = t
	} else {
		order.OrderDate = time.Now()
	}

	// Shipping address (nil-safe).
	if p.ShippingAddress != nil {
		a := p.ShippingAddress
		order.ShipFirstName = a.FirstName
		order.ShipLastName = a.LastName
		order.ShipAddress1 = a.Address1
		order.ShipAddress2 = a.Address2
		order.ShipCity = a.City
		order.ShipProvince = a.Province
		order.ShipPostcode = a.Zip
		order.ShipCountry = a.Country
		order.ShipPhone = a.Phone
	}

	// Shipping amount (sum of all shipping lines).
	for _, sl := range p.ShippingLines {
		if v, err := strconv.ParseFloat(sl.Price, 64); err == nil {
			order.ShippingAmount += v
		}
	}

	// Line items.
	lines := make([]model.OrderLine, 0, len(p.LineItems))
	for _, li := range p.LineItems {
		line := model.OrderLine{
			SKU:      li.SKU,
			Quantity: li.Quantity,
		}
		if v, err := strconv.ParseFloat(li.Price, 64); err == nil {
			line.UnitPrice = v
		}
		if v, err := strconv.ParseFloat(li.TotalDiscount, 64); err == nil {
			line.Discount = v
		}
		// Sum tax from all tax lines.
		for _, t := range li.TaxLines {
			if v, err := strconv.ParseFloat(t.Price, 64); err == nil {
				line.Tax += v
			}
		}
		lines = append(lines, line)
	}

	return order, lines
}
