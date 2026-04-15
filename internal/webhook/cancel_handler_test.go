package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trismegistus0/rectella-shopify-service/internal/cancellation"
	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/store"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

// --- fakes ---

type fakeCancelStore struct {
	exists        bool
	existsErr     error
	localOrder    *model.Order
	lookupErr     error
	createCalls   int
	createdRecord store.OrderCancellation
	createErr     error
}

func (f *fakeCancelStore) CancellationExists(ctx context.Context, webhookID string) (bool, error) {
	return f.exists, f.existsErr
}

func (f *fakeCancelStore) GetOrderByShopifyID(ctx context.Context, shopifyOrderID int64) (*model.Order, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if f.localOrder == nil {
		return nil, store.ErrOrderNotFound
	}
	return f.localOrder, nil
}

func (f *fakeCancelStore) CreateCancellation(ctx context.Context, c store.OrderCancellation) (int64, error) {
	f.createCalls++
	f.createdRecord = c
	if f.createErr != nil {
		return 0, f.createErr
	}
	return 42, nil
}

type fakeSorqryQuerier struct {
	statusByOrder map[string]string
	err           error
}

func (f *fakeSorqryQuerier) QueryDispatchedOrders(ctx context.Context, orderNumbers []string) (map[string]syspro.SORQRYResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]syspro.SORQRYResult, len(orderNumbers))
	for _, n := range orderNumbers {
		if status, ok := f.statusByOrder[n]; ok {
			out[n] = syspro.SORQRYResult{SalesOrder: n, OrderStatus: status}
		}
	}
	return out, nil
}

// --- helpers ---
//
// `testSecret` and `signBody` are shared with `handler_test.go` in the
// same package; defined once there.

func newCancelRequest(t *testing.T, body []byte, webhookID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/orders/cancelled", strings.NewReader(string(body)))
	req.Header.Set("X-Shopify-Hmac-Sha256", signBody(string(body)))
	req.Header.Set("X-Shopify-Webhook-Id", webhookID)
	return req
}

func fireHandler(t *testing.T, st CancelStore, sq SorqryQuerier, body []byte, webhookID string) (*httptest.ResponseRecorder, *CancelHandler) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewCancelHandler(st, sq, testSecret, logger)
	rec := httptest.NewRecorder()
	h.handle(rec, newCancelRequest(t, body, webhookID))
	return rec, h
}

func cancelBody(t *testing.T, shopifyID int64, reason string) []byte {
	t.Helper()
	p := map[string]any{
		"id":            shopifyID,
		"name":          fmt.Sprintf("#BBQ%d", shopifyID),
		"cancelled_at":  "2026-04-15T14:00:00Z",
		"cancel_reason": reason,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// --- tests ---

func TestCancelHandler_HMACFail(t *testing.T) {
	st := &fakeCancelStore{}
	sq := &fakeSorqryQuerier{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewCancelHandler(st, sq, testSecret, logger)

	body := cancelBody(t, 1001, "customer")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/orders/cancelled", strings.NewReader(string(body)))
	req.Header.Set("X-Shopify-Hmac-Sha256", "not-a-valid-signature")
	req.Header.Set("X-Shopify-Webhook-Id", "wh-fail")

	rec := httptest.NewRecorder()
	h.handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if st.createCalls != 0 {
		t.Errorf("should not persist on HMAC failure")
	}
}

func TestCancelHandler_NoLocalOrder_PreSYSPRO(t *testing.T) {
	st := &fakeCancelStore{} // localOrder=nil → ErrOrderNotFound
	sq := &fakeSorqryQuerier{}
	body := cancelBody(t, 1001, "customer")

	rec, _ := fireHandler(t, st, sq, body, "wh-preorder")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if st.createCalls != 1 {
		t.Fatalf("expected 1 create call, got %d", st.createCalls)
	}
	if st.createdRecord.Disposition != string(cancellation.CancellablePreSYSPRO) {
		t.Errorf("disposition = %q, want cancellable_pre_syspro", st.createdRecord.Disposition)
	}
}

func TestCancelHandler_LocalOrderNoSysproNumber_PreSYSPRO(t *testing.T) {
	st := &fakeCancelStore{
		localOrder: &model.Order{ID: 77, ShopifyOrderID: 1001, SysproOrderNumber: ""},
	}
	sq := &fakeSorqryQuerier{}
	body := cancelBody(t, 1001, "customer")

	rec, _ := fireHandler(t, st, sq, body, "wh-local-no-syspro")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if st.createdRecord.Disposition != string(cancellation.CancellablePreSYSPRO) {
		t.Errorf("disposition = %q, want cancellable_pre_syspro", st.createdRecord.Disposition)
	}
	if st.createdRecord.OrderID == nil || *st.createdRecord.OrderID != 77 {
		t.Errorf("expected OrderID=77 on record, got %v", st.createdRecord.OrderID)
	}
}

func TestCancelHandler_StatusBranches(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantDisp cancellation.Disposition
	}{
		{"status0_safe", "0", cancellation.CancellableInSYSPRO},
		{"status1_safe", "1", cancellation.CancellableInSYSPRO},
		{"status4_allocated", "4", cancellation.ReviewAllocated},
		{"status6_picked", "6", cancellation.TooLatePicked},
		{"status7_picked", "7", cancellation.TooLatePicked},
		{"status8_invoiced", "8", cancellation.TooLateInvoiced},
		{"status9_complete", "9", cancellation.TooLateInvoiced},
		{"status_cancelled", `\`, cancellation.AlreadyCancelled},
		{"status_unknown", "Z", cancellation.ReviewAllocated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeCancelStore{
				localOrder: &model.Order{ID: 1, ShopifyOrderID: 5000, SysproOrderNumber: "016020"},
			}
			sq := &fakeSorqryQuerier{statusByOrder: map[string]string{"016020": tc.status}}
			body := cancelBody(t, 5000, "customer")

			rec, _ := fireHandler(t, st, sq, body, "wh-"+tc.name)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
			}
			if st.createdRecord.Disposition != string(tc.wantDisp) {
				t.Errorf("disposition = %q, want %q", st.createdRecord.Disposition, tc.wantDisp)
			}
			if st.createdRecord.SysproOrderStatus != tc.status {
				t.Errorf("syspro_order_status = %q, want %q", st.createdRecord.SysproOrderStatus, tc.status)
			}
		})
	}
}

func TestCancelHandler_SorqryFailure_ReviewAllocated(t *testing.T) {
	st := &fakeCancelStore{
		localOrder: &model.Order{ID: 1, ShopifyOrderID: 5000, SysproOrderNumber: "016020"},
	}
	sq := &fakeSorqryQuerier{err: errors.New("vpn dropped")}
	body := cancelBody(t, 5000, "customer")

	rec, _ := fireHandler(t, st, sq, body, "wh-sorqry-fail")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if st.createdRecord.Disposition != string(cancellation.ReviewAllocated) {
		t.Errorf("SORQRY failure should default to ReviewAllocated, got %q", st.createdRecord.Disposition)
	}
}

func TestCancelHandler_DuplicateWebhook(t *testing.T) {
	st := &fakeCancelStore{exists: true}
	sq := &fakeSorqryQuerier{}
	body := cancelBody(t, 1001, "customer")

	rec, _ := fireHandler(t, st, sq, body, "wh-dup")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if st.createCalls != 0 {
		t.Errorf("duplicate should not create a second row")
	}
}
