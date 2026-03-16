package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"codeberg.org/speeder091/rectella-shopify-service/config"
	"codeberg.org/speeder091/rectella-shopify-service/internal/batch"
	"codeberg.org/speeder091/rectella-shopify-service/internal/model"
	"codeberg.org/speeder091/rectella-shopify-service/internal/store"
	"codeberg.org/speeder091/rectella-shopify-service/internal/syspro"
	"codeberg.org/speeder091/rectella-shopify-service/internal/webhook"
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

	slog.Info("starting rectella-shopify-service")

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

	// Set up HTTP routes.
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			slog.Error("health check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy", "error": err.Error()})
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
	sysproClient := syspro.NewEnetClient(
		cfg.SysproEnetURL,
		cfg.SysproOperator,
		cfg.SysproPassword,
		cfg.SysproCompanyID,
		logger,
	)

	// Start batch processor.
	batchProc := batch.New(db, sysproClient, cfg.BatchInterval, logger)
	batchCtx, batchCancel := context.WithCancel(ctx)
	defer batchCancel()
	go batchProc.Run(batchCtx)

	// Register webhook handlers.
	wh := webhook.NewHandler(db, cfg.ShopifyWebhookSecret, logger)
	wh.Register(mux)

	// Retry endpoint — move failed/dead-lettered orders back to pending.
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
			slog.Warn("retry order failed", "order_id", orderID, "error", err)
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		slog.Info("order retried", "order_id", orderID)
		json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	})

	// Orders visibility endpoint.
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
				OrderDate:       o.OrderDate.Format("2006-01-02T15:04:05Z07:00"),
				CreatedAt:       o.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
				UpdatedAt:       o.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
			})
		}

		json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
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

	// Graceful HTTP shutdown with 15s deadline.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}

	// Ensure batch processor has stopped.
	batchCancel()

	slog.Info("server stopped cleanly")
	return nil
}
