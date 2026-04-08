//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"codeberg.org/speeder091/rectella-shopify-service/internal/model"
	"codeberg.org/speeder091/rectella-shopify-service/internal/syspro"
)

// --- Group 5: Extended batch & pipeline scenarios ---

func TestBatch_EmptySalesOrderResponse(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(7771001, "#BBQ-EMPTY-SO")

	resp := ts.sendWebhook(t, "wh-empty-so-001", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook failed: %d", resp.StatusCode)
	}

	// Mock SYSPRO: validation succeeds but SalesOrder number is empty.
	// The real-world scenario where SORTOI validates but doesn't echo back
	// a generated SO number. parseSORTOIResponse falls back to CustomerPoNumber,
	// which is the Shopify order name (#BBQ-EMPTY-SO).
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		return &syspro.SalesOrderResult{
			Success:           true,
			SysproOrderNumber: order.OrderNumber, // simulates CustomerPoNumber fallback
		}, nil
	})

	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}

	// Verify order is submitted with the CustomerPoNumber as the SYSPRO reference.
	submitted, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusSubmitted)
	if err != nil {
		t.Fatalf("listing submitted: %v", err)
	}
	if len(submitted) != 1 {
		t.Fatalf("expected 1 submitted order, got %d", len(submitted))
	}
	if submitted[0].SysproOrderNumber != "#BBQ-EMPTY-SO" {
		t.Errorf("expected syspro_order_number=#BBQ-EMPTY-SO, got %q", submitted[0].SysproOrderNumber)
	}
	if submitted[0].Status != model.OrderStatusSubmitted {
		t.Errorf("expected status=submitted, got %q", submitted[0].Status)
	}

	// No pending orders remain.
	pending, _ := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after batch, got %d", len(pending))
	}
}

func TestBatch_InfraErrorRetriesAndDeadLetters(t *testing.T) {
	ts := newTestServer(t)
	payload := orderPayload(7772001, "#BBQ-INFRA-DL")

	resp := ts.sendWebhook(t, "wh-infra-dl-001", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook failed: %d", resp.StatusCode)
	}

	// Always return an infrastructure error.
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		return nil, errors.New("connection refused")
	})

	// Attempt 1: pending → processing → back to pending (attempts=1).
	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch attempt 1: %v", err)
	}
	pending, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing pending after attempt 1: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending after attempt 1, got %d", len(pending))
	}
	if pending[0].Attempts != 1 {
		t.Errorf("expected attempts=1 after first failure, got %d", pending[0].Attempts)
	}
	if pending[0].LastError != "connection refused" {
		t.Errorf("expected last_error='connection refused', got %q", pending[0].LastError)
	}

	// Attempt 2: still pending with attempts=2.
	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch attempt 2: %v", err)
	}
	pending, err = ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing pending after attempt 2: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending after attempt 2, got %d", len(pending))
	}
	if pending[0].Attempts != 2 {
		t.Errorf("expected attempts=2 after second failure, got %d", pending[0].Attempts)
	}

	// Attempt 3: dead-lettered.
	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch attempt 3: %v", err)
	}
	pending, _ = ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after 3 failures, got %d", len(pending))
	}

	deadLettered, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusDeadLetter)
	if err != nil {
		t.Fatalf("listing dead_letter: %v", err)
	}
	if len(deadLettered) != 1 {
		t.Fatalf("expected 1 dead_letter order, got %d", len(deadLettered))
	}
	if deadLettered[0].Attempts != 3 {
		t.Errorf("expected attempts=3 on dead-lettered order, got %d", deadLettered[0].Attempts)
	}
	if deadLettered[0].LastError != "connection refused" {
		t.Errorf("expected last_error='connection refused', got %q", deadLettered[0].LastError)
	}
}

func TestBatch_BusinessErrorMarksFailedContinuesBatch(t *testing.T) {
	ts := newTestServer(t)

	// Submit 3 orders via webhooks.
	orders := []struct {
		shopifyID int64
		name      string
		webhookID string
	}{
		{7773001, "#BBQ-BIZ-OK1", "wh-biz-ok1"},
		{7773002, "#BBQ-BIZ-FAIL", "wh-biz-fail"},
		{7773003, "#BBQ-BIZ-OK2", "wh-biz-ok2"},
	}
	for _, o := range orders {
		payload := orderPayload(o.shopifyID, o.name)
		resp := ts.sendWebhook(t, o.webhookID, payload)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("webhook for %s failed: %d", o.name, resp.StatusCode)
		}
	}

	// Verify all 3 are pending before batch.
	pending, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing pending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 pending orders, got %d", len(pending))
	}

	// Mock: first succeeds, second fails with business error, third succeeds.
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		switch order.OrderNumber {
		case "#BBQ-BIZ-FAIL":
			return &syspro.SalesOrderResult{
				Success:      false,
				ErrorMessage: "Invalid stock code",
			}, nil
		default:
			return &syspro.SalesOrderResult{
				Success:           true,
				SysproOrderNumber: "SO-" + order.OrderNumber,
			}, nil
		}
	})

	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}

	// First and third should be submitted.
	submitted, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusSubmitted)
	if err != nil {
		t.Fatalf("listing submitted: %v", err)
	}
	if len(submitted) != 2 {
		t.Fatalf("expected 2 submitted orders, got %d", len(submitted))
	}

	// Verify both successful orders are present (order is newest first from ListOrdersByStatus).
	submittedNames := make(map[string]bool)
	for _, s := range submitted {
		submittedNames[s.OrderNumber] = true
	}
	if !submittedNames["#BBQ-BIZ-OK1"] {
		t.Error("expected #BBQ-BIZ-OK1 to be submitted")
	}
	if !submittedNames["#BBQ-BIZ-OK2"] {
		t.Error("expected #BBQ-BIZ-OK2 to be submitted")
	}

	// Second should be failed.
	failed, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusFailed)
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed order, got %d", len(failed))
	}
	if failed[0].OrderNumber != "#BBQ-BIZ-FAIL" {
		t.Errorf("expected #BBQ-BIZ-FAIL to be failed, got %s", failed[0].OrderNumber)
	}
	if failed[0].LastError != "Invalid stock code" {
		t.Errorf("expected last_error='Invalid stock code', got %q", failed[0].LastError)
	}

	// No pending orders remain.
	pending, _ = ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after batch, got %d", len(pending))
	}
}

func TestRetryEndpoint_RequeuesDeadLetter(t *testing.T) {
	ts := newTestServerWithRetry(t)

	// Create an order via webhook.
	payload := orderPayload(7774001, "#BBQ-RETRY-DL")
	resp := ts.sendWebhook(t, "wh-retry-dl-001", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook failed: %d", resp.StatusCode)
	}

	// Force order to dead_letter via infra errors (3 attempts).
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		return nil, errors.New("connection timeout")
	})
	for i := 0; i < 3; i++ {
		ts.Batch.ProcessBatch(context.Background())
	}

	// Verify it's dead-lettered.
	deadLettered, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusDeadLetter)
	if err != nil {
		t.Fatalf("listing dead_letter: %v", err)
	}
	if len(deadLettered) != 1 {
		t.Fatalf("expected 1 dead_letter, got %d", len(deadLettered))
	}
	orderID := deadLettered[0].ID

	// Hit POST /orders/{id}/retry.
	resp = ts.post(t, fmt.Sprintf("/orders/%d/retry", orderID), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("retry endpoint returned %d: %s", resp.StatusCode, body)
	}

	var retryResp map[string]string
	json.NewDecoder(resp.Body).Decode(&retryResp)
	if retryResp["status"] != "queued" {
		t.Errorf("expected status=queued, got %q", retryResp["status"])
	}

	// Verify order is back to pending with attempts reset.
	pending, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending after retry, got %d", len(pending))
	}
	if pending[0].Attempts != 0 {
		t.Errorf("expected attempts=0 after retry, got %d", pending[0].Attempts)
	}

	// Now make SYSPRO succeed and run the batch — order should be submitted.
	ts.Mock.setSubmitFunc(func(ctx context.Context, order model.Order, lines []model.OrderLine) (*syspro.SalesOrderResult, error) {
		return &syspro.SalesOrderResult{
			Success:           true,
			SysproOrderNumber: "SO-RETRIED",
		}, nil
	})

	if err := ts.Batch.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch after retry: %v", err)
	}

	submitted, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusSubmitted)
	if err != nil {
		t.Fatalf("listing submitted: %v", err)
	}
	if len(submitted) != 1 {
		t.Fatalf("expected 1 submitted after retry batch, got %d", len(submitted))
	}
	if submitted[0].SysproOrderNumber != "SO-RETRIED" {
		t.Errorf("expected syspro_order_number=SO-RETRIED, got %q", submitted[0].SysproOrderNumber)
	}
}

func TestConcurrentWebhooks_NoDuplicateOrders(t *testing.T) {
	ts := newTestServer(t)

	const count = 10
	var wg sync.WaitGroup
	wg.Add(count)

	results := make([]int, count)
	for i := 0; i < count; i++ {
		go func(idx int) {
			defer wg.Done()
			shopifyID := int64(7780001 + idx)
			orderName := fmt.Sprintf("#BBQ-CONC-%03d", idx)
			webhookID := fmt.Sprintf("wh-conc-%03d", idx)
			payload := orderPayload(shopifyID, orderName)

			resp := ts.sendWebhook(t, webhookID, payload)
			results[idx] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}

	wg.Wait()

	// All webhooks should succeed.
	for i, code := range results {
		if code != http.StatusOK {
			t.Errorf("webhook %d returned %d, expected 200", i, code)
		}
	}

	// Verify exactly 10 orders in DB, no duplicates.
	pending, err := ts.DB.ListOrdersByStatus(context.Background(), model.OrderStatusPending)
	if err != nil {
		t.Fatalf("listing pending: %v", err)
	}
	if len(pending) != count {
		t.Fatalf("expected %d pending orders, got %d", count, len(pending))
	}

	// Verify all order names are unique.
	seen := make(map[string]bool, count)
	for _, o := range pending {
		if seen[o.OrderNumber] {
			t.Errorf("duplicate order number: %s", o.OrderNumber)
		}
		seen[o.OrderNumber] = true
	}
}
