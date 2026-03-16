package config

import (
	"fmt"
	"log/slog"
	"os"
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

	StockSyncInterval time.Duration
	BatchInterval     time.Duration

	LogLevel slog.Level
}

func Load() (*Config, error) {
	var missing []string

	get := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	c := &Config{
		ShopifyWebhookSecret: get("SHOPIFY_WEBHOOK_SECRET"),
		ShopifyAPIKey:        get("SHOPIFY_API_KEY"),
		ShopifyAPISecret:     get("SHOPIFY_API_SECRET"),
		ShopifyStoreURL:      get("SHOPIFY_STORE_URL"),

		SysproEnetURL:   get("SYSPRO_ENET_URL"),
		SysproOperator:  get("SYSPRO_OPERATOR"),
		SysproPassword:  os.Getenv("SYSPRO_PASSWORD"), // blank password is valid
		SysproCompanyID: get("SYSPRO_COMPANY_ID"),

		DatabaseURL: get("DATABASE_URL"),
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	c.Port = port
	c.AdminToken = os.Getenv("ADMIN_TOKEN")

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %v", missing)
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
