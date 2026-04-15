package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/cancellation"
	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/store"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

// CancelStore is the subset of *store.DB the cancel handler uses.
// Narrow interface keeps the handler trivially testable.
type CancelStore interface {
	CancellationExists(ctx context.Context, webhookID string) (bool, error)
	GetOrderByShopifyID(ctx context.Context, shopifyOrderID int64) (*model.Order, error)
	CreateCancellation(ctx context.Context, c store.OrderCancellation) (int64, error)
}

// SorqryQuerier is the SYSPRO read path the cancel handler needs. The
// real implementation is *syspro.EnetClient.QueryDispatchedOrders, which
// accepts a slice and returns a map — we pass [orderNumber] and pick
// the first entry. Narrow interface so the handler unit tests can mock
// SORQRY without any session plumbing.
type SorqryQuerier interface {
	QueryDispatchedOrders(ctx context.Context, orderNumbers []string) (map[string]syspro.SORQRYResult, error)
}

// CancelHandler processes Shopify orders/cancelled webhooks and
// classifies them into ops dispositions. Phase 1 is classify-only: the
// handler records the disposition, logs at WARN level, and returns 200.
// It does NOT propagate the cancellation to SYSPRO — that's Phase 2.
type CancelHandler struct {
	store  CancelStore
	sorqry SorqryQuerier
	secret string
	logger *slog.Logger
}

// NewCancelHandler constructs a handler. `secret` is the Shopify webhook
// HMAC secret (same as orders/create — the custom app's client_secret).
func NewCancelHandler(store CancelStore, sorqry SorqryQuerier, secret string, logger *slog.Logger) *CancelHandler {
	return &CancelHandler{
		store:  store,
		sorqry: sorqry,
		secret: secret,
		logger: logger,
	}
}

// Register adds the cancel route to the given mux.
func (h *CancelHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/orders/cancelled", h.handle)
}

func (h *CancelHandler) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		h.respond(w, http.StatusBadRequest, "error", "failed to read body")
		return
	}

	// HMAC verify — same function as orders/create.
	signature := r.Header.Get("X-Shopify-Hmac-Sha256")
	if !VerifyHMAC(body, h.secret, signature) {
		h.logger.Warn("cancel webhook HMAC verification failed", "remote_addr", r.RemoteAddr)
		h.respond(w, http.StatusUnauthorized, "error", "unauthorized")
		return
	}

	webhookID := r.Header.Get("X-Shopify-Webhook-Id")
	if webhookID == "" {
		h.respond(w, http.StatusBadRequest, "error", "missing webhook ID")
		return
	}

	// Idempotency — Shopify retries the same webhook under the same id
	// if we 5xx. Short-circuit on duplicate.
	exists, err := h.store.CancellationExists(r.Context(), webhookID)
	if err != nil {
		h.logger.Error("checking cancellation existence", "error", err, "webhook_id", webhookID)
		h.respond(w, http.StatusInternalServerError, "error", "internal error")
		return
	}
	if exists {
		h.logger.Info("duplicate cancellation webhook ignored", "webhook_id", webhookID)
		h.respond(w, http.StatusOK, "status", "duplicate")
		return
	}

	var payload shopifyOrder
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger.Warn("malformed cancel webhook payload", "error", err, "webhook_id", webhookID)
		h.respond(w, http.StatusBadRequest, "error", "malformed JSON")
		return
	}
	if payload.ID == 0 {
		h.logger.Warn("cancel webhook missing order ID", "webhook_id", webhookID)
		h.respond(w, http.StatusUnprocessableEntity, "error", "missing order ID")
		return
	}

	// Look up the local order row to see if we ever forwarded this to
	// SYSPRO. If not, the cancellation is safe to classify as "pre
	// SYSPRO" and we're done.
	localOrder, err := h.store.GetOrderByShopifyID(r.Context(), payload.ID)
	if err != nil && !errors.Is(err, store.ErrOrderNotFound) {
		h.logger.Error("looking up local order", "error", err, "shopify_order_id", payload.ID)
		h.respond(w, http.StatusInternalServerError, "error", "internal error")
		return
	}

	var (
		disposition       cancellation.Disposition
		sysproOrderNumber string
		sysproOrderStatus string
		orderIDPtr        *int64
	)

	switch {
	case errors.Is(err, store.ErrOrderNotFound):
		disposition = cancellation.CancellablePreSYSPRO
	case localOrder.SysproOrderNumber == "":
		// Local row exists but never reached SYSPRO.
		disposition = cancellation.CancellablePreSYSPRO
		orderIDPtr = &localOrder.ID
	default:
		orderIDPtr = &localOrder.ID
		sysproOrderNumber = localOrder.SysproOrderNumber

		// Call SORQRY for the specific SYSPRO order. Bound to 20s to
		// keep the webhook within Shopify's 5s soft / 10s hard delivery
		// window — if SYSPRO is slow, we'd rather return 500 and let
		// Shopify retry than hold the connection.
		qctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		results, qerr := h.sorqry.QueryDispatchedOrders(qctx, []string{sysproOrderNumber})
		if qerr != nil {
			h.logger.Warn("SORQRY failed during cancel classification",
				"error", qerr,
				"syspro_order", sysproOrderNumber,
			)
			// Treat SORQRY failure as "unknown state" — classifier
			// defaults to ReviewAllocated on nil input, which is the
			// safest fallback (never auto-cancel something we can't
			// inspect).
			disposition = cancellation.Classify(*localOrder, nil)
		} else {
			result, ok := results[sysproOrderNumber]
			if !ok {
				// SORQRY returned but didn't include our order — treat
				// the same as a failed lookup.
				disposition = cancellation.Classify(*localOrder, nil)
			} else {
				sysproOrderStatus = strings.TrimSpace(result.OrderStatus)
				disposition = cancellation.Classify(*localOrder, cancellation.SyspoResultAdapter{Result: &result})
			}
		}
	}

	// Parse the cancel-specific fields from the payload.
	var cancelledAt *time.Time
	if payload.CancelledAt != nil && *payload.CancelledAt != "" {
		if t, perr := time.Parse(time.RFC3339, *payload.CancelledAt); perr == nil {
			cancelledAt = &t
		}
	}

	record := store.OrderCancellation{
		OrderID:             orderIDPtr,
		ShopifyOrderID:      payload.ID,
		ShopifyOrderNumber:  payload.Name,
		SysproOrderNumber:   sysproOrderNumber,
		ShopifyCancelReason: payload.CancelReason,
		ShopifyCancelledAt:  cancelledAt,
		SysproOrderStatus:   sysproOrderStatus,
		Disposition:         string(disposition),
		WebhookID:           webhookID,
		RawPayload:          body,
	}

	if _, err := h.store.CreateCancellation(r.Context(), record); err != nil {
		if errors.Is(err, store.ErrDuplicateCancellation) {
			h.logger.Info("duplicate cancellation (race)", "webhook_id", webhookID)
			h.respond(w, http.StatusOK, "status", "duplicate")
			return
		}
		h.logger.Error("persisting cancellation", "error", err, "webhook_id", webhookID)
		h.respond(w, http.StatusInternalServerError, "error", "internal error")
		return
	}

	// Escalation log line — ops tails this to see dispositions live.
	// Level chosen to match severity of the disposition.
	level := slog.LevelInfo
	switch disposition {
	case cancellation.ReviewAllocated, cancellation.TooLatePicked:
		level = slog.LevelWarn
	case cancellation.TooLateInvoiced:
		level = slog.LevelError
	}
	h.logger.Log(r.Context(), level, "cancellation classified",
		"webhook_id", webhookID,
		"shopify_order_id", payload.ID,
		"shopify_order_number", payload.Name,
		"syspro_order", sysproOrderNumber,
		"syspro_order_status", sysproOrderStatus,
		"cancel_reason", payload.CancelReason,
		"disposition", disposition,
	)

	h.respond(w, http.StatusOK, "status", "classified")
}

func (h *CancelHandler) respond(w http.ResponseWriter, status int, key, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{key: value})
}
