//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

// --- Group 1: Webhook → DB ---

func TestWebhook_ValidOrder(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(5551001, "#BBQ1001")

	resp := ts.sendWebhook(t, "wh-valid-001", payload)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify order landed in DB as pending.
	orders, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing pending orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 pending order, got %d", len(orders))
	}
	if orders[0].OrderNumber != "#BBQ1001" {
		t.Errorf("expected order number #BBQ1001, got %s", orders[0].OrderNumber)
	}
	if orders[0].CustomerAccount != "WEBS01" {
		t.Errorf("expected customer WEBS01, got %s", orders[0].CustomerAccount)
	}
}

func TestWebhook_DuplicateIdempotent(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(5552001, "#BBQ2001")

	resp1 := ts.sendWebhook(t, "wh-dup-001", payload)
	resp1.Body.Close()

	resp2 := ts.sendWebhook(t, "wh-dup-001", payload)
	resp2.Body.Close()

	if resp1.StatusCode != http.StatusOK || resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected both 200, got %d and %d", resp1.StatusCode, resp2.StatusCode)
	}

	// Only one order in DB despite two sends.
	orders, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing orders: %v", err)
	}
	if len(orders) != 1 {
		t.Errorf("expected 1 order (idempotent), got %d", len(orders))
	}
}

func TestWebhook_InvalidHMAC(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(5553001, "#BBQ3001")

	req, _ := http.NewRequest("POST", ts.Server.URL+"/webhooks/orders/create",
		bytes.NewReader(payload),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Webhook-Id", "wh-bad-hmac")
	req.Header.Set("X-Shopify-Hmac-Sha256", "aW52YWxpZHNpZw==")

	resp, err := ts.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}

	// Nothing in DB.
	orders, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing orders: %v", err)
	}
	if len(orders) != 0 {
		t.Errorf("expected no orders after bad HMAC, got %d", len(orders))
	}
}

func TestWebhook_MalformedJSON(t *testing.T) {
	ts := newTestServer(t)
	badJSON := []byte(`{not valid json`)

	resp := ts.sendWebhook(t, "wh-bad-json", badJSON)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestWebhook_EmptyLineItems(t *testing.T) {
	ts := newTestServer(t)
	payload, _ := json.Marshal(map[string]any{
		"id":         999,
		"name":       "#BBQ9999",
		"line_items": []any{},
	})

	resp := ts.sendWebhook(t, "wh-no-lines", payload)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", resp.StatusCode)
	}
}

func TestWebhook_ZeroOrderID(t *testing.T) {
	ts := newTestServer(t)
	payload, _ := json.Marshal(map[string]any{
		"id":   0,
		"name": "#BBQ0000",
		"line_items": []map[string]any{
			{"sku": "X", "quantity": 1, "price": "10.00"},
		},
	})

	resp := ts.sendWebhook(t, "wh-zero-id", payload)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", resp.StatusCode)
	}
}

// --- Group 2: Full pipeline ---

func TestPipeline_WebhookToBatchSuccess(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(5554001, "#BBQ4001")

	// 1. Send webhook — order lands as pending.
	resp := ts.sendWebhook(t, "wh-pipeline-001", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook failed: %d", resp.StatusCode)
	}

	// 2. Run batch processor — mock SYSPRO returns success.
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		return &syspro.SalesOrderResult{
			Success:           true,
			SysproOrderNumber: "SO-12345",
		}, nil
	})

	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}

	// 3. Verify order is now submitted.
	submitted, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusSubmitted)
	if err != nil {
		t.Fatalf("listing submitted: %v", err)
	}
	if len(submitted) != 1 {
		t.Fatalf("expected 1 submitted order, got %d", len(submitted))
	}
	if submitted[0].OrderNumber != "#BBQ4001" {
		t.Errorf("expected #BBQ4001, got %s", submitted[0].OrderNumber)
	}

	// No pending orders remain.
	pending, _ := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after batch, got %d", len(pending))
	}
}

func TestPipeline_BatchBusinessError(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(5555001, "#BBQ5001")

	resp := ts.sendWebhook(t, "wh-pipeline-002", payload)
	resp.Body.Close()

	// SYSPRO rejects the order (business error — invalid SKU, etc).
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		return &syspro.SalesOrderResult{
			Success:      false,
			ErrorMessage: "Invalid stock code CBBQ0001",
		}, nil
	})

	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}

	failed, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusFailed)
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed order, got %d", len(failed))
	}
	if failed[0].LastError != "Invalid stock code CBBQ0001" {
		t.Errorf("expected error message, got %q", failed[0].LastError)
	}
}

func TestPipeline_BatchInfraErrorRetry(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(5556001, "#BBQ6001")

	resp := ts.sendWebhook(t, "wh-pipeline-003", payload)
	resp.Body.Close()

	// Infrastructure error — connection timeout.
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		return nil, errors.New("connection timeout")
	})

	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}

	// Order stays pending with attempts=1.
	pending, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending order, got %d", len(pending))
	}
	if pending[0].Attempts != 1 {
		t.Errorf("expected attempts=1, got %d", pending[0].Attempts)
	}
}

func TestPipeline_DeadLetterAfter3(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(5557001, "#BBQ7001")

	resp := ts.sendWebhook(t, "wh-pipeline-004", payload)
	resp.Body.Close()

	// Simulate 3 consecutive infra errors.
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		return nil, errors.New("connection timeout")
	})

	for i := 0; i < 3; i++ {
		if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
			t.Fatalf("ProcessBatch attempt %d: %v", i+1, err)
		}
	}

	// After 3 failures, order should be dead-lettered.
	deadLettered, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusDeadLetter)
	if err != nil {
		t.Fatalf("listing dead_letter: %v", err)
	}
	if len(deadLettered) != 1 {
		t.Fatalf("expected 1 dead_letter order, got %d", len(deadLettered))
	}
	if deadLettered[0].Attempts != 3 {
		t.Errorf("expected attempts=3, got %d", deadLettered[0].Attempts)
	}

	// No pending orders remain.
	pending, _ := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending, got %d", len(pending))
	}
}

// --- Group 3: Orders endpoint ---

func TestOrders_FilterByStatus(t *testing.T) {
	ts := newTestServer(t)

	// Insert two orders via webhooks.
	for i, name := range []string{"#BBQ-F1", "#BBQ-F2"} {
		payload := orderPayload(int64(6661000+i), name)
		resp := ts.sendWebhook(t, fmt.Sprintf("wh-filter-%d", i), payload)
		resp.Body.Close()
	}

	// Batch-submit one (mock succeeds).
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		if order.OrderNumber == "#BBQ-F1" {
			return &syspro.SalesOrderResult{Success: true, SysproOrderNumber: "SO-F1"}, nil
		}
		return &syspro.SalesOrderResult{Success: false, ErrorMessage: "test fail"}, nil
	})
	ts.Batch.ProcessBatch(context.Background())

	// Query submitted — should get 1.
	resp := ts.get(t, "/orders?status=submitted")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var submitted []map[string]any
	json.NewDecoder(resp.Body).Decode(&submitted)
	if len(submitted) != 1 {
		t.Errorf("expected 1 submitted, got %d", len(submitted))
	}

	// Query failed — should get 1.
	resp2 := ts.get(t, "/orders?status=failed")
	defer resp2.Body.Close()

	var failed []map[string]any
	json.NewDecoder(resp2.Body).Decode(&failed)
	if len(failed) != 1 {
		t.Errorf("expected 1 failed, got %d", len(failed))
	}
}

func TestOrders_InvalidStatus(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/orders?status=bogus")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid status, got %d", resp.StatusCode)
	}
}

func TestOrders_MissingStatus(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/orders")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing status, got %d", resp.StatusCode)
	}
}

func TestOrders_NoRawPayload(t *testing.T) {
	ts := newTestServer(t)

	payload := orderPayload(6670001, "#BBQ-NR1")
	resp := ts.sendWebhook(t, "wh-no-raw", payload)
	resp.Body.Close()

	resp2 := ts.get(t, "/orders?status=pending")
	defer resp2.Body.Close()

	body, _ := io.ReadAll(resp2.Body)
	var orders []map[string]any
	json.Unmarshal(body, &orders)

	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}

	// The response DTO should not include raw_payload.
	if _, found := orders[0]["raw_payload"]; found {
		t.Error("response contains raw_payload — should be excluded from the API")
	}
}

// --- Group 4: Health endpoints ---

func TestHealth_OK(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/health")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %q", body["status"])
	}
}

// TestResetStaleProcessing verifies the boot-time sweep that recovers orders
// stuck in 'processing' after a service crash. This is defect N from the
// overnight plan: MarkOrderProcessing transitions pending->processing before
// SYSPRO is called; if the service dies in that window the row is stuck
// forever. The boot sweep flips stale rows back to pending for retry.
func TestResetStaleProcessing(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(5559999, "#BBQ9999")

	resp := ts.sendWebhook(t, "wh-stale-001", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook failed: %d", resp.StatusCode)
	}

	// Find the order and mark it processing, then backdate its updated_at
	// so it looks stale.
	pending, _ := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if len(pending) == 0 {
		t.Fatal("expected at least 1 pending order after webhook")
	}
	var staleID int64
	for _, o := range pending {
		if o.OrderNumber == "#BBQ9999" {
			staleID = o.ID
			break
		}
	}
	if staleID == 0 {
		t.Fatal("could not find #BBQ9999 in pending orders")
	}

	ok, err := ts.DB.MarkOrderProcessing(context.Background(), staleID)
	if err != nil || !ok {
		t.Fatalf("MarkOrderProcessing failed: ok=%v err=%v", ok, err)
	}

	// Backdate updated_at to simulate a stale row (older than 10 min threshold).
	_, err = ts.DB.Pool.Exec(context.Background(),
		`UPDATE orders SET updated_at = NOW() - interval '1 hour' WHERE id = $1`,
		staleID,
	)
	if err != nil {
		t.Fatalf("backdating updated_at: %v", err)
	}

	// Call the sweep with a 10-minute threshold.
	reset, err := ts.DB.ResetStaleProcessing(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("ResetStaleProcessing: %v", err)
	}
	if reset != 1 {
		t.Errorf("expected 1 row reset, got %d", reset)
	}

	// Verify it's back to pending with attempts incremented.
	pendingAfter, _ := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	var found *model.Order
	for i := range pendingAfter {
		if pendingAfter[i].ID == staleID {
			found = &pendingAfter[i]
			break
		}
	}
	if found == nil {
		t.Fatal("stale order did not return to pending")
	}
	if found.Attempts < 1 {
		t.Errorf("expected attempts >= 1 after reset, got %d", found.Attempts)
	}

	// Fresh processing rows (not stale) must NOT be reset.
	ts2 := newTestServer(t)
	payload2 := orderPayload(5559998, "#BBQ9998")
	resp2 := ts2.sendWebhook(t, "wh-stale-002", payload2)
	resp2.Body.Close()
	pending2, _ := ts2.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if len(pending2) != 1 {
		t.Fatalf("setup: expected 1 pending, got %d", len(pending2))
	}
	_, err = ts2.DB.MarkOrderProcessing(context.Background(), pending2[0].ID)
	if err != nil {
		t.Fatalf("MarkOrderProcessing: %v", err)
	}
	// No backdating — row is fresh.
	reset2, err := ts2.DB.ResetStaleProcessing(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("ResetStaleProcessing: %v", err)
	}
	if reset2 != 0 {
		t.Errorf("expected 0 fresh rows reset, got %d", reset2)
	}
}

func TestReady_OK(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/ready")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ready" {
		t.Errorf("expected status ready, got %q", body["status"])
	}
}
