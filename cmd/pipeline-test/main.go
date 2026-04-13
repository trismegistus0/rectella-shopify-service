package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	live := flag.Bool("live", false, "Use real SYSPRO instead of mock")
	mockPort := flag.Int("mock-port", 19100, "Port for mock SYSPRO server")
	mockShopifyPort := flag.Int("mock-shopify-port", 19200, "Port for mock Shopify server")
	target := flag.String("target", "", "Service URL (default http://localhost:PORT)")
	timeout := flag.Duration("timeout", 60*time.Second, "Per-order timeout")
	noColor := flag.Bool("no-color", false, "Disable color output")
	cleanup := flag.Bool("cleanup", true, "Delete test orders from DB after run")
	flag.Parse()

	webhookSecret := requireEnvVar("SHOPIFY_WEBHOOK_SECRET")
	databaseURL := requireEnvVar("DATABASE_URL")
	if *target == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		*target = "http://localhost:" + port
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fatal("DB connection failed: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fatal("DB ping failed: %v", err)
	}

	if !*live {
		mock := newMockSyspro(*mockPort)
		if err := mock.start(); err != nil {
			fatal("Mock SYSPRO failed: %v", err)
		}
		defer mock.stop()
	}

	// Start mock Shopify.
	shopify := newMockShopify(*mockShopifyPort)
	if err := shopify.start(); err != nil {
		fatal("Mock Shopify failed: %v", err)
	}
	defer shopify.stop()

	scenarios := buildScenarios(webhookSecret)
	p := newPrinter(*noColor)
	httpClient := &http.Client{Timeout: 10 * time.Second}

	mode := fmt.Sprintf("mock (SYSPRO :%d, Shopify :%d)", *mockPort, *mockShopifyPort)
	if *live {
		mode = fmt.Sprintf("live SYSPRO (mock Shopify :%d)", *mockShopifyPort)
	}
	var orderCount int
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM orders").Scan(&orderCount)
	p.header(mode, *target, fmt.Sprintf("connected (%d existing orders)", orderCount))

	passed, failed := 0, 0
	var results []orderResult
	totalStart := time.Now()

	// Run order scenarios (1-2: happy path orders that reach "submitted").
	for i, s := range scenarios[:2] {
		ok, res := runScenario(ctx, p, httpClient, pool, s, i+1, len(scenarios), *target, *timeout)
		if ok {
			passed++
		} else {
			failed++
		}
		if res != nil {
			results = append(results, *res)
		}
	}

	// Run validation scenarios (3-5: duplicate, invalid HMAC, missing SKU).
	for i, s := range scenarios[2:5] {
		ok, _ := runScenario(ctx, p, httpClient, pool, s, i+3, len(scenarios), *target, *timeout)
		if ok {
			passed++
		} else {
			failed++
		}
	}

	// Scenario 6: Stock sync verification.
	p.scenarioStart(6, len(scenarios), "STOCK-SYNC", "Inventory syncer pushes to mock Shopify")
	syncDeadline := time.Now().Add(*timeout)
	syncOK := false
	for time.Now().Before(syncDeadline) {
		if shopify.inventoryCalls.Load() > 0 {
			syncOK = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if syncOK {
		p.check(fmt.Sprintf("inventorySetQuantities called %dx", shopify.inventoryCalls.Load()), "OK")
		p.pass()
		passed++
	} else {
		p.fail("no inventorySetQuantities calls received (stock sync did not run)")
		failed++
	}

	// Scenario 7: Fulfilment sync verification.
	// Wait for orders to reach "fulfilled" status (SORQRY returns status 9 -> Shopify fulfilment created).
	p.scenarioStart(7, len(scenarios), "FULFILMENT-SYNC", "Fulfilment syncer creates Shopify fulfilments")
	fulfilDeadline := time.Now().Add(*timeout)
	fulfilOK := false
	for time.Now().Before(fulfilDeadline) {
		var fulfilledCount int
		_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM orders WHERE order_number LIKE '#PIPE-%' AND status = 'fulfilled'").Scan(&fulfilledCount)
		if fulfilledCount > 0 {
			fulfilOK = true
			p.check(fmt.Sprintf("%d orders fulfilled", fulfilledCount), "OK")
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if fulfilOK {
		p.check(fmt.Sprintf("fulfillmentCreate called %dx", shopify.fulfilmentCalls.Load()), "OK")
		p.pass()
		passed++
	} else {
		p.fail("no orders reached 'fulfilled' status (fulfilment sync did not run)")
		failed++
	}

	p.summary(passed, failed, time.Since(totalStart), results)

	if *cleanup {
		_, _ = pool.Exec(ctx, "DELETE FROM order_lines WHERE order_id IN (SELECT id FROM orders WHERE order_number LIKE '#PIPE-%')")
		_, _ = pool.Exec(ctx, "DELETE FROM orders WHERE order_number LIKE '#PIPE-%'")
		_, _ = pool.Exec(ctx, "DELETE FROM webhook_events WHERE webhook_id LIKE 'pipe-%'")
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func requireEnvVar(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fatal("%s must be set", name)
	}
	return v
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func runScenario(ctx context.Context, p *printer, client *http.Client, pool *pgxpool.Pool, s scenario, index, total int, target string, timeout time.Duration) (bool, *orderResult) {
	p.scenarioStart(index, total, s.name, s.description)

	req, _ := http.NewRequestWithContext(ctx, "POST", target+"/webhooks/orders/create",
		strings.NewReader(string(s.payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Webhook-Id", s.webhookID)
	req.Header.Set("X-Shopify-Hmac-Sha256", s.hmacSignature)

	resp, err := client.Do(req)
	if err != nil {
		p.send(0)
		p.fail(fmt.Sprintf("HTTP error: %v", err))
		return false, nil
	}
	resp.Body.Close()
	p.send(resp.StatusCode)

	if resp.StatusCode != s.expectHTTP {
		p.fail(fmt.Sprintf("expected HTTP %d, got %d", s.expectHTTP, resp.StatusCode))
		return false, nil
	}

	if s.expectDBStatus == "" {
		if s.isDuplicate {
			var count int
			_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM orders WHERE order_number = $1", s.dupOriginal).Scan(&count)
			if count == 1 {
				p.check("no new DB row (idempotent)", "OK")
				p.pass()
				return true, nil
			}
			p.fail(fmt.Sprintf("expected 1 order for %s, found %d", s.dupOriginal, count))
			return false, nil
		}
		var count int
		_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM orders WHERE order_number = $1", s.name).Scan(&count)
		if count == 0 {
			p.check("no DB row", "OK")
			p.pass()
			return true, nil
		}
		p.fail(fmt.Sprintf("expected 0 rows, found %d", count))
		return false, nil
	}

	deadline := time.Now().Add(timeout)
	lastStatus := ""
	for time.Now().Before(deadline) {
		var status, sysproOrder string
		err := pool.QueryRow(ctx,
			"SELECT status, COALESCE(syspro_order_number, '') FROM orders WHERE order_number = $1", s.name,
		).Scan(&status, &sysproOrder)
		if err != nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}

		if status != lastStatus {
			detail := ""
			if sysproOrder != "" {
				detail = sysproOrder
			}
			p.stage(lastStatus, status, detail)
			lastStatus = status
		}
		if status == s.expectDBStatus {
			if s.expectDBStatus == "submitted" && sysproOrder == "" {
				p.fail("submitted but no syspro_order_number")
				return false, nil
			}
			p.pass()
			return true, &orderResult{name: s.name, status: status, sysproOrder: sysproOrder}
		}
		if status == "failed" || status == "dead_letter" {
			var lastErr string
			_ = pool.QueryRow(ctx, "SELECT COALESCE(last_error, '') FROM orders WHERE order_number = $1", s.name).Scan(&lastErr)
			p.fail(fmt.Sprintf("expected %s, got %s: %s", s.expectDBStatus, status, lastErr))
			return false, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	p.fail(fmt.Sprintf("timed out waiting for %s (last: %s)", s.expectDBStatus, lastStatus))
	return false, nil
}
