package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// setRequiredEnv sets all required env vars to valid values.
// Returns a cleanup function (though t.Setenv handles cleanup automatically).
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SHOPIFY_WEBHOOK_SECRET", "test-secret")
	t.Setenv("SHOPIFY_API_KEY", "test-key")
	t.Setenv("SHOPIFY_API_SECRET", "test-api-secret")
	t.Setenv("SHOPIFY_STORE_URL", "test.myshopify.com")
	t.Setenv("SYSPRO_ENET_URL", "http://192.168.3.150:31002/SYSPROWCFService/Rest")
	t.Setenv("SYSPRO_OPERATOR", "admin")
	t.Setenv("SYSPRO_COMPANY_ID", "RILT")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/testdb?sslmode=disable")
}

func TestLoad_AllRequiredVars(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ShopifyWebhookSecret != "test-secret" {
		t.Errorf("ShopifyWebhookSecret = %q, want %q", cfg.ShopifyWebhookSecret, "test-secret")
	}
	if cfg.SysproEnetURL != "http://192.168.3.150:31002/SYSPROWCFService/Rest" {
		t.Errorf("SysproEnetURL = %q", cfg.SysproEnetURL)
	}
	if cfg.SysproCompanyID != "RILT" {
		t.Errorf("SysproCompanyID = %q, want %q", cfg.SysproCompanyID, "RILT")
	}
}

func TestLoad_MissingRequiredVars(t *testing.T) {
	// Explicitly unset all required vars (CI may have some set).
	for _, key := range []string{
		"SHOPIFY_WEBHOOK_SECRET",
		"SHOPIFY_STORE_URL", "SYSPRO_ENET_URL", "SYSPRO_OPERATOR",
		"SYSPRO_COMPANY_ID", "DATABASE_URL",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err == nil {
		t.Fatal("expected error for missing vars, got nil")
	}
	if cfg != nil {
		t.Fatal("expected nil config on error")
	}

	// Should mention multiple missing vars.
	for _, key := range []string{"SHOPIFY_WEBHOOK_SECRET", "SYSPRO_ENET_URL", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %q, got: %v", key, err)
		}
	}
}

func TestLoad_BlankPasswordIsValid(t *testing.T) {
	setRequiredEnv(t)
	// SYSPRO_PASSWORD intentionally not set — should NOT appear in missing vars.

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SysproPassword != "" {
		t.Errorf("SysproPassword = %q, want empty", cfg.SysproPassword)
	}
}

func TestLoad_Defaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want %q", cfg.Port, "8080")
	}
	if cfg.StockSyncInterval != 15*time.Minute {
		t.Errorf("StockSyncInterval = %v, want 15m", cfg.StockSyncInterval)
	}
	if cfg.BatchInterval != 5*time.Minute {
		t.Errorf("BatchInterval = %v, want 5m", cfg.BatchInterval)
	}
	if cfg.FulfilmentSyncInterval != 30*time.Minute {
		t.Errorf("FulfilmentSyncInterval = %v, want 30m", cfg.FulfilmentSyncInterval)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.AdminToken != "" {
		t.Errorf("AdminToken = %q, want empty", cfg.AdminToken)
	}
}

func TestLoad_CustomPort(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORT", "9080")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "9080" {
		t.Errorf("Port = %q, want %q", cfg.Port, "9080")
	}
}

func TestLoad_CustomIntervals(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("STOCK_SYNC_INTERVAL", "30s")
	t.Setenv("BATCH_INTERVAL", "1m")
	t.Setenv("FULFILMENT_SYNC_INTERVAL", "10m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StockSyncInterval != 30*time.Second {
		t.Errorf("StockSyncInterval = %v, want 30s", cfg.StockSyncInterval)
	}
	if cfg.BatchInterval != 1*time.Minute {
		t.Errorf("BatchInterval = %v, want 1m", cfg.BatchInterval)
	}
	if cfg.FulfilmentSyncInterval != 10*time.Minute {
		t.Errorf("FulfilmentSyncInterval = %v, want 10m", cfg.FulfilmentSyncInterval)
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BATCH_INTERVAL", "banana")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
	if !strings.Contains(err.Error(), "BATCH_INTERVAL") {
		t.Errorf("error should mention BATCH_INTERVAL, got: %v", err)
	}
}

func TestLoad_SKUParsing(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"simple", "CBBQ0001,CBBQ0002", []string{"CBBQ0001", "CBBQ0002"}},
		{"whitespace", " CBBQ0001 , CBBQ0002 , CBBQ0003 ", []string{"CBBQ0001", "CBBQ0002", "CBBQ0003"}},
		{"trailing comma", "CBBQ0001,CBBQ0002,", []string{"CBBQ0001", "CBBQ0002"}},
		{"single", "CBBQ0001", []string{"CBBQ0001"}},
		{"empty", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			if tt.raw != "" {
				t.Setenv("SYSPRO_SKUS", tt.raw)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cfg.SysproSKUs) != len(tt.want) {
				t.Fatalf("SysproSKUs len = %d, want %d", len(cfg.SysproSKUs), len(tt.want))
			}
			for i, want := range tt.want {
				if cfg.SysproSKUs[i] != want {
					t.Errorf("SysproSKUs[%d] = %q, want %q", i, cfg.SysproSKUs[i], want)
				}
			}
		})
	}
}

func TestLoad_OptionalVars(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SHOPIFY_ACCESS_TOKEN", "shpat_test123")
	t.Setenv("SHOPIFY_LOCATION_ID", "gid://shopify/Location/12345")
	t.Setenv("SYSPRO_WAREHOUSE", "WH01")
	t.Setenv("ADMIN_TOKEN", "admin-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ShopifyAccessToken != "shpat_test123" {
		t.Errorf("ShopifyAccessToken = %q", cfg.ShopifyAccessToken)
	}
	if cfg.ShopifyLocationID != "gid://shopify/Location/12345" {
		t.Errorf("ShopifyLocationID = %q", cfg.ShopifyLocationID)
	}
	if cfg.SysproWarehouse != "WH01" {
		t.Errorf("SysproWarehouse = %q", cfg.SysproWarehouse)
	}
	if cfg.AdminToken != "admin-secret" {
		t.Errorf("AdminToken = %q", cfg.AdminToken)
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input   string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelInfo, false},
		{"info", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"trace", 0, true},
		{"INFO", 0, true},
		{"warning", 0, true},
	}

	for _, tt := range tests {
		t.Run("level_"+tt.input, func(t *testing.T) {
			got, err := parseLogLevel(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoad_ShopifyBaseURL(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SHOPIFY_BASE_URL", "http://localhost:19200/admin/api/2025-04/graphql.json")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ShopifyBaseURL != "http://localhost:19200/admin/api/2025-04/graphql.json" {
		t.Errorf("ShopifyBaseURL = %q, want mock URL", cfg.ShopifyBaseURL)
	}
}

func TestLoad_APIKeyAPISecretOptional(t *testing.T) {
	// SHOPIFY_API_KEY and SHOPIFY_API_SECRET are not consumed by any code path.
	// They should be optional, not block startup if unset.
	t.Setenv("SHOPIFY_WEBHOOK_SECRET", "test-secret")
	t.Setenv("SHOPIFY_STORE_URL", "test.myshopify.com")
	t.Setenv("SYSPRO_ENET_URL", "http://192.168.3.150:31002/SYSPROWCFService/Rest")
	t.Setenv("SYSPRO_OPERATOR", "admin")
	t.Setenv("SYSPRO_COMPANY_ID", "RILT")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/testdb?sslmode=disable")
	// Deliberately NOT setting SHOPIFY_API_KEY or SHOPIFY_API_SECRET.
	t.Setenv("SHOPIFY_API_KEY", "")
	t.Setenv("SHOPIFY_API_SECRET", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load should succeed without API key/secret, got: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOG_LEVEL", "trace")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid log level")
	}
	if !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Errorf("error should mention LOG_LEVEL, got: %v", err)
	}
}
