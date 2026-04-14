package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	ShopifyWebhookSecret string
	ShopifyAPIKey        string
	ShopifyAPISecret     string
	ShopifyStoreURL      string

	SysproEnetURL   string
	SysproOperator  string
	SysproPassword  string
	SysproCompanyID string

	DatabaseURL string

	Port       string
	AdminToken string

	StockSyncInterval      time.Duration
	BatchInterval          time.Duration
	FulfilmentSyncInterval time.Duration
	ReconciliationInterval time.Duration // 0 = disabled

	LogLevel slog.Level

	// Stock sync (optional — disabled if SysproSKUs is empty).
	ShopifyAccessToken string
	ShopifyLocationID  string
	ShopifyBaseURL     string // Override full GraphQL URL (testing/staging). Constructed from StoreURL if empty.
	SysproWarehouse    string
	SysproSKUs         []string
}

func Load() (*Config, error) {
	var missing []string
	var placeholder []string

	// get returns the value of an env var and records it as missing if empty.
	// Values starting with "PLACEHOLDER" are also flagged — these are emitted
	// by Bicep/IaC when a secret was not populated at deploy time, and we
	// want to fail fast rather than boot with broken auth.
	get := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		} else if strings.HasPrefix(v, "PLACEHOLDER") {
			placeholder = append(placeholder, key)
		}
		return v
	}

	// checkPlaceholder guards optional-at-boot values (like SYSPRO_PASSWORD
	// which can legitimately be empty in local tests) against the same
	// PLACEHOLDER footgun.
	checkPlaceholder := func(key, v string) string {
		if strings.HasPrefix(v, "PLACEHOLDER") {
			placeholder = append(placeholder, key)
		}
		return v
	}

	c := &Config{
		ShopifyWebhookSecret: get("SHOPIFY_WEBHOOK_SECRET"),
		ShopifyAPIKey:        os.Getenv("SHOPIFY_API_KEY"),
		ShopifyAPISecret:     os.Getenv("SHOPIFY_API_SECRET"),
		ShopifyStoreURL:      get("SHOPIFY_STORE_URL"),

		SysproEnetURL:   get("SYSPRO_ENET_URL"),
		SysproOperator:  get("SYSPRO_OPERATOR"),
		SysproPassword:  checkPlaceholder("SYSPRO_PASSWORD", os.Getenv("SYSPRO_PASSWORD")), // blank password is valid, PLACEHOLDER is not
		SysproCompanyID: get("SYSPRO_COMPANY_ID"),

		DatabaseURL: get("DATABASE_URL"),
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	c.Port = port
	c.AdminToken = os.Getenv("ADMIN_TOKEN")
	c.ShopifyAccessToken = os.Getenv("SHOPIFY_ACCESS_TOKEN")
	c.ShopifyLocationID = os.Getenv("SHOPIFY_LOCATION_ID")
	c.ShopifyBaseURL = os.Getenv("SHOPIFY_BASE_URL")
	c.SysproWarehouse = os.Getenv("SYSPRO_WAREHOUSE")

	// Parse comma-separated SKU list.
	if raw := os.Getenv("SYSPRO_SKUS"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				c.SysproSKUs = append(c.SysproSKUs, s)
			}
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %v", missing)
	}
	if len(placeholder) > 0 {
		return nil, fmt.Errorf("environment variables contain PLACEHOLDER values (real values not populated): %v", placeholder)
	}

	var err error

	c.StockSyncInterval, err = parseDuration("STOCK_SYNC_INTERVAL", "15m")
	if err != nil {
		return nil, err
	}

	c.BatchInterval, err = parseDuration("BATCH_INTERVAL", "5m")
	if err != nil {
		return nil, err
	}

	c.FulfilmentSyncInterval, err = parseDuration("FULFILMENT_SYNC_INTERVAL", "30m")
	if err != nil {
		return nil, err
	}

	// Reconciliation sweep default is OFF (zero duration). Operators opt in
	// by setting RECONCILIATION_INTERVAL to something like "24h" in App Service.
	if raw := os.Getenv("RECONCILIATION_INTERVAL"); raw != "" {
		c.ReconciliationInterval, err = time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid duration for RECONCILIATION_INTERVAL: %w", err)
		}
	}

	c.LogLevel, err = parseLogLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		return nil, err
	}

	return c, nil
}

func parseDuration(key, defaultVal string) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		v = defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s: %w", key, err)
	}
	return d, nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid LOG_LEVEL: %q (must be debug, info, warn, or error)", s)
	}
}
