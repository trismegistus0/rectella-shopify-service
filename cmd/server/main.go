package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/config"
	"github.com/trismegistus0/rectella-shopify-service/internal/batch"
	"github.com/trismegistus0/rectella-shopify-service/internal/fulfilment"
	"github.com/trismegistus0/rectella-shopify-service/internal/inventory"
	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/payments"
	"github.com/trismegistus0/rectella-shopify-service/internal/reconcile"
	"github.com/trismegistus0/rectella-shopify-service/internal/store"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
	"github.com/trismegistus0/rectella-shopify-service/internal/webhook"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Set up structured logging.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	slog.Info("starting rectella-shopify-service",
		"company", cfg.SysproCompanyID,
		"warehouse", cfg.SysproWarehouse,
		"skus", len(cfg.SysproSKUs),
		"batch_interval", cfg.BatchInterval,
		"stock_sync_interval", cfg.StockSyncInterval,
		"fulfilment_sync_interval", cfg.FulfilmentSyncInterval,
		"port", cfg.Port,
		"admin_auth", cfg.AdminToken != "",
		"log_level", cfg.LogLevel.String(),
	)

	if cfg.AdminToken == "" {
		slog.Warn("ADMIN_TOKEN is empty — admin endpoints (/orders, /orders/{id}/retry) are UNAUTHENTICATED")
	}

	// Connect to database.
	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()
	slog.Info("connected to database")

	// Run migrations.
	if err := store.Migrate(ctx, db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	// Reset stale 'processing' orders. An order only reaches processing
	// between MarkOrderProcessing and the terminal status update; if the
	// service was killed in that window, the row is stuck. Boot-time sweep
	// flips anything older than 10 minutes back to pending for retry.
	if reset, err := db.ResetStaleProcessing(ctx, 10*time.Minute); err != nil {
		slog.Warn("failed to reset stale processing orders", "error", err)
	} else if reset > 0 {
		slog.Info("reset stale processing orders", "count", reset)
	}

	// Set up HTTP routes.
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			slog.Error("health check failed", "error", err)
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

	// Instantiate SYSPRO e.net client.
	taxCodeMap := syspro.ParseTaxCodeMap(cfg.SysproTaxCodeMap)
	sysproClient := syspro.NewEnetClient(
		cfg.SysproEnetURL,
		cfg.SysproOperator,
		cfg.SysproPassword,
		cfg.SysproCompanyID,
		cfg.SysproCompanyPassword,
		cfg.SysproWarehouse,
		cfg.SysproAllocationAction,
		taxCodeMap,
		logger,
	)

	// Set up stock sync. Two modes:
	//   - dynamic:  SYSPRO_SKUS empty, SYSPRO_WAREHOUSE set → discover the
	//               SKU list from Shopify productVariants on each cycle
	//   - static:   SYSPRO_SKUS non-empty → sync exactly those SKUs
	// Either mode needs SHOPIFY_ACCESS_TOKEN + SYSPRO_WAREHOUSE.
	triggerCh := make(chan struct{}, 1)
	var syncCancel context.CancelFunc

	switch {
	case cfg.ShopifyAccessToken == "":
		slog.Warn("stock sync disabled: SHOPIFY_ACCESS_TOKEN missing")
	case cfg.SysproWarehouse == "":
		slog.Warn("stock sync disabled: SYSPRO_WAREHOUSE missing")
	default:
		var invOpts []inventory.ShopifyOption
		if cfg.ShopifyBaseURL != "" {
			invOpts = append(invOpts, inventory.WithBaseURL(cfg.ShopifyBaseURL))
		}
		shopifyClient := inventory.NewShopifyClient(
			cfg.ShopifyStoreURL,
			cfg.ShopifyAccessToken,
			cfg.ShopifyLocationID,
			cfg.SysproSKUs,
			logger,
			invOpts...,
		)

		// Lister precedence: SQL Server (Sarah's WEBS warehouse view) →
		// Shopify (productVariants pagination) → static slice from env.
		var lister inventory.SKULister
		listerMode := "static"
		if cfg.SQLServerDSN != "" {
			sqlLister, err := inventory.NewSQLServerLister(cfg.SQLServerDSN, logger)
			if err != nil {
				slog.Warn("sql server lister init failed, falling back", "error", err)
			} else if sqlLister != nil {
				if err := sqlLister.Ping(ctx); err != nil {
					slog.Warn("sql server lister ping failed, falling back", "error", err)
					_ = sqlLister.Close()
				} else {
					lister = sqlLister
					listerMode = "sql"
					defer sqlLister.Close() //nolint:errcheck // best-effort close at shutdown
				}
			}
		}
		if lister == nil && len(cfg.SysproSKUs) == 0 {
			lister = shopifyClient
			listerMode = "shopify"
		}
		slog.Info("stock sync enabled",
			"mode", listerMode,
			"warehouse", cfg.SysproWarehouse,
			"static_sku_count", len(cfg.SysproSKUs),
		)

		syncer := inventory.NewSyncer(
			sysproClient, // *EnetClient satisfies InventoryQuerier
			shopifyClient,
			db,
			lister,
			cfg.StockSyncInterval,
			cfg.SysproWarehouse,
			cfg.SysproSKUs,
			triggerCh,
			logger,
		)

		var syncCtx context.Context
		syncCtx, syncCancel = context.WithCancel(ctx)
		defer syncCancel()
		go syncer.Run(syncCtx)
	}

	// Start batch processor.
	batchProc := batch.New(db, sysproClient, cfg.BatchInterval, logger)
	batchCtx, batchCancel := context.WithCancel(ctx)
	defer batchCancel()
	go batchProc.Run(batchCtx)

	// Start reconciliation sweeper (catches orders missed by webhook delivery).
	// Disabled gracefully if ShopifyAccessToken or ReconciliationInterval unset.
	var reconcileCancel context.CancelFunc
	if cfg.ReconciliationInterval > 0 {
		sw := reconcile.New(db, cfg.ShopifyStoreURL, cfg.ShopifyAccessToken, cfg.ReconciliationInterval, logger)
		if sw != nil {
			var reconcileCtx context.Context
			reconcileCtx, reconcileCancel = context.WithCancel(ctx)
			defer reconcileCancel()
			go sw.Run(reconcileCtx)
		}
	} else {
		slog.Info("reconciliation sweep disabled (RECONCILIATION_INTERVAL unset)")
	}

	// Start daily cash-receipt email reporter. Disabled gracefully
	// unless SMTP config + recipients are all present. This exists as
	// the MVP stopgap while ARSPAY automation is blocked on Sarah's
	// spec + Liz's sign-off — credit control still gets a daily CSV.
	var reportCancel context.CancelFunc
	if cfg.SMTPHost != "" && cfg.SMTPPort != 0 && cfg.SMTPFrom != "" && len(cfg.CreditControlTo) > 0 && cfg.ShopifyAccessToken != "" {
		mailer := payments.NewMailer(payments.MailerConfig{
			Host:     cfg.SMTPHost,
			Port:     cfg.SMTPPort,
			Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword,
			From:     cfg.SMTPFrom,
			UseTLS:   cfg.SMTPUseTLS,
		})
		fetcher := payments.NewTransactionsFetcher(cfg.ShopifyStoreURL, cfg.ShopifyAccessToken, logger)
		reporter, err := payments.NewDailyReporter(payments.DailyReporterConfig{
			Source:     fetcher,
			Mailer:     mailer,
			Recipients: cfg.CreditControlTo,
			StoreName:  cfg.ShopifyStoreURL,
			Hour:       cfg.DailyReportHour,
			Logger:     logger,
		})
		if err != nil {
			slog.Warn("daily report disabled", "error", err)
		} else {
			var reportCtx context.Context
			reportCtx, reportCancel = context.WithCancel(ctx)
			defer reportCancel()
			go reporter.Run(reportCtx)
			slog.Info("daily report enabled", "hour_utc", cfg.DailyReportHour, "recipients", len(cfg.CreditControlTo))
		}
	} else {
		slog.Info("daily report disabled (SMTP or CREDIT_CONTROL_TO not configured)")
	}

	// Start payments syncer (ARSTPY cash receipts). Requires all three:
	// PAYMENTS_SYNC_INTERVAL, ARSPAY_CASH_BOOK, ARSPAY_PAYMENT_TYPE.
	// Disabled gracefully (no error) if any are missing — protects the
	// live service from booting with half-configured payment posting.
	var paymentsCancel context.CancelFunc
	switch {
	case cfg.PaymentsSyncInterval == 0:
		slog.Info("payments sync disabled (PAYMENTS_SYNC_INTERVAL unset)")
	case cfg.ArspayCashBook == "":
		slog.Warn("payments sync disabled (ARSPAY_CASH_BOOK unset — required when PAYMENTS_SYNC_INTERVAL is set)")
	case cfg.ArspayPaymentType == "":
		slog.Warn("payments sync disabled (ARSPAY_PAYMENT_TYPE unset — required when PAYMENTS_SYNC_INTERVAL is set)")
	default:
		paymentsSyncer := payments.NewSyncer(payments.SyncerConfig{
			Store:       db,
			Poster:      sysproClient,
			Interval:    cfg.PaymentsSyncInterval,
			Customer:    "WEBS01",
			Bank:        cfg.ArspayCashBook,
			PaymentType: cfg.ArspayPaymentType,
			Logger:      logger,
		})
		var paymentsCtx context.Context
		paymentsCtx, paymentsCancel = context.WithCancel(ctx)
		defer paymentsCancel()
		go paymentsSyncer.Run(paymentsCtx)
		slog.Info("payments sync enabled",
			"interval", cfg.PaymentsSyncInterval,
			"cashbook", cfg.ArspayCashBook,
			"payment_type", cfg.ArspayPaymentType,
		)
	}

	// Start fulfilment syncer (disabled if SHOPIFY_ACCESS_TOKEN missing).
	var fulfilmentCancel context.CancelFunc
	if cfg.ShopifyAccessToken != "" {
		var fulOpts []fulfilment.FulfilmentOption
		if cfg.ShopifyBaseURL != "" {
			fulOpts = append(fulOpts, fulfilment.WithFulfilmentBaseURL(cfg.ShopifyBaseURL))
		}
		fulfilmentClient := fulfilment.NewFulfilmentClient(
			cfg.ShopifyStoreURL,
			cfg.ShopifyAccessToken,
			logger,
			fulOpts...,
		)
		fulfilmentSyncer := fulfilment.NewFulfilmentSyncer(
			sysproClient,
			fulfilmentClient,
			db,
			cfg.FulfilmentSyncInterval,
			logger,
		)
		var fulfilmentCtx context.Context
		fulfilmentCtx, fulfilmentCancel = context.WithCancel(ctx)
		defer fulfilmentCancel()
		go fulfilmentSyncer.Run(fulfilmentCtx)
	} else {
		slog.Warn("SHOPIFY_ACCESS_TOKEN missing, fulfilment sync disabled")
	}

	// Register webhook handlers.
	wh := webhook.NewHandler(db, cfg.ShopifyWebhookSecret, triggerCh, logger)
	wh.Register(mux)

	// Cancellation webhook — classify-only gate, no SYSPRO mutation.
	// Shares the HMAC secret with orders/create.
	cancelHandler := webhook.NewCancelHandler(db, sysproClient, cfg.ShopifyWebhookSecret, logger)
	cancelHandler.Register(mux)

	// Admin auth check for operations endpoints.
	requireAdmin := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if cfg.AdminToken != "" {
				token := r.Header.Get("X-Admin-Token")
				if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AdminToken)) != 1 {
					slog.Warn("admin auth failed", "method", r.Method, "path", r.URL.Path) //nolint:gosec // G706: slog structured fields, not interpolated
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
					return
				}
			}
			next(w, r)
		}
	}

	// Retry endpoint — move failed/dead-lettered orders back to pending.
	mux.HandleFunc("POST /orders/{id}/retry", requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		idStr := r.PathValue("id")
		var orderID int64
		if _, err := fmt.Sscanf(idStr, "%d", &orderID); err != nil || orderID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid order ID"})
			return
		}

		if err := db.RetryOrder(r.Context(), orderID); err != nil {
			slog.Warn("retry order failed", "order_id", orderID, "error", err)
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		slog.Info("order retried", "order_id", orderID)
		json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	}))

	// Orders visibility endpoint.
	validStatuses := map[string]bool{
		"pending": true, "processing": true, "submitted": true,
		"fulfilled": true, "failed": true, "dead_letter": true, "cancelled": true,
	}
	mux.HandleFunc("GET /orders", requireAdmin(func(w http.ResponseWriter, r *http.Request) {
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
			slog.Error("listing orders", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
			return
		}

		type orderResponse struct {
			ID                int64             `json:"id"`
			ShopifyOrderID    int64             `json:"shopify_order_id"`
			OrderNumber       string            `json:"order_number"`
			Status            model.OrderStatus `json:"status"`
			CustomerAccount   string            `json:"customer_account"`
			SysproOrderNumber string            `json:"syspro_order_number,omitempty"`
			Attempts          int               `json:"attempts"`
			LastError         string            `json:"last_error,omitempty"`
			OrderDate         string            `json:"order_date"`
			CreatedAt         string            `json:"created_at"`
			UpdatedAt         string            `json:"updated_at"`
		}

		resp := make([]orderResponse, 0, len(orders))
		for _, o := range orders {
			resp = append(resp, orderResponse{
				ID:                o.ID,
				ShopifyOrderID:    o.ShopifyOrderID,
				OrderNumber:       o.OrderNumber,
				Status:            o.Status,
				CustomerAccount:   o.CustomerAccount,
				SysproOrderNumber: o.SysproOrderNumber,
				Attempts:          o.Attempts,
				LastError:         o.LastError,
				OrderDate:         o.OrderDate.Format("2006-01-02T15:04:05Z07:00"),
				CreatedAt:         o.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
				UpdatedAt:         o.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
			})
		}

		json.NewEncoder(w).Encode(resp)
	}))

	// Wrap mux with middleware: panic recovery → security headers → request logging.
	var handler http.Handler = mux
	handler = requestLogging(handler)
	handler = securityHeaders(handler)
	handler = panicRecovery(handler)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Start server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		slog.Info("received shutdown signal", "signal", sig)
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	}

	// Give the batch processor time to finish the current order before cancelling.
	// This prevents the scenario where SYSPRO accepts an order but the DB update
	// is cancelled mid-flight, causing the order to be resubmitted as a duplicate.
	slog.Info("draining batch processor (10s grace period)")
	time.AfterFunc(10*time.Second, batchCancel)

	// Drain stock syncer.
	if syncCancel != nil {
		slog.Info("draining stock syncer (10s grace period)")
		time.AfterFunc(10*time.Second, syncCancel)
	}

	// Drain fulfilment syncer.
	if fulfilmentCancel != nil {
		slog.Info("draining fulfilment syncer (10s grace period)")
		time.AfterFunc(10*time.Second, fulfilmentCancel)
	}

	// Drain payments syncer.
	if paymentsCancel != nil {
		slog.Info("draining payments syncer (10s grace period)")
		time.AfterFunc(10*time.Second, paymentsCancel)
	}

	// Drain daily report scheduler.
	if reportCancel != nil {
		slog.Info("draining daily report scheduler")
		time.AfterFunc(5*time.Second, reportCancel)
	}

	// Graceful HTTP shutdown with 15s deadline.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}

	// Ensure batch processor and syncers have stopped.
	batchCancel()
	if syncCancel != nil {
		syncCancel()
	}
	if fulfilmentCancel != nil {
		fulfilmentCancel()
	}
	if paymentsCancel != nil {
		paymentsCancel()
	}
	if reportCancel != nil {
		reportCancel()
	}

	slog.Info("server stopped cleanly")
	return nil
}

// panicRecovery catches panics in handlers and returns 500 instead of crashing.
func panicRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				slog.Error("panic recovered", "panic", rv, "method", r.Method, "path", r.URL.Path) //nolint:gosec // G706: slog emits structured JSON fields, not interpolated strings
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets standard security response headers.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// requestLogging logs method, path, status, and duration for every request.
func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start),
		}
		if wid := r.Header.Get("X-Shopify-Webhook-Id"); wid != "" {
			attrs = append(attrs, "webhook_id", wid)
		}
		slog.Info("request", attrs...) //nolint:gosec // G706: slog emits structured JSON fields, not interpolated strings
	})
}

// statusWriter wraps ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}
