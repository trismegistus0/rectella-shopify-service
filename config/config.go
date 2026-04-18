package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ShopifyWebhookSecret string
	ShopifyAPIKey        string
	ShopifyAPISecret     string
	ShopifyStoreURL      string

	SysproEnetURL          string
	SysproOperator         string
	SysproPassword         string
	SysproCompanyID        string
	SysproCompanyPassword  string
	SysproAllocationAction string
	SysproTaxCodeMap       string

	DatabaseURL string

	Port       string
	AdminToken string

	StockSyncInterval      time.Duration
	BatchInterval          time.Duration
	FulfilmentSyncInterval time.Duration
	ReconciliationInterval time.Duration // 0 = disabled
	PaymentsSyncInterval   time.Duration // 0 = disabled

	// ARSPAY (cash-receipt posting) configuration. Both required when
	// PaymentsSyncInterval > 0.
	//   ArspayCashBook    — SYSPRO cashbook code (e.g. "BANK1") that
	//                       receives the net of every receipt. Set up
	//                       in AR Setup → Banks; Liz owns the value.
	//   ArspayPaymentType — installation-specific payment-method code
	//                       (e.g. "01" for cheque, "EF" for EFT).
	//                       Configured in AR Setup → Browse on Payment
	//                       Codes; Liz/Sarah own the value.
	ArspayCashBook    string
	ArspayPaymentType string

	LogLevel slog.Level

	// Stock sync (optional — disabled if SysproSKUs is empty).
	ShopifyAccessToken string
	ShopifyLocationID  string
	ShopifyBaseURL     string // Override full GraphQL URL (testing/staging). Constructed from StoreURL if empty.
	SysproWarehouse    string
	SysproSKUs         []string

	// SQLServerDSN is the primary source for the WEBS warehouse stock-code
	// list (Sarah's view `bq_WEBS_Whs_QoH` on RIL-DB01). Empty = disabled,
	// the syncer falls through to Shopify-first lister then the static slice.
	SQLServerDSN string

	// Daily cash-receipt email to credit control. Disabled unless all
	// of SMTP_HOST/PORT/FROM and CREDIT_CONTROL_TO are set.
	SMTPHost        string
	SMTPPort        int
	SMTPUsername    string
	SMTPPassword    string
	SMTPFrom        string
	SMTPUseTLS      bool
	CreditControlTo []string
	DailyReportHour int // UTC, default 1 (= 01:00 GMT / 02:00 BST — per Sarah's spec)
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

		SysproEnetURL:         get("SYSPRO_ENET_URL"),
		SysproOperator:        get("SYSPRO_OPERATOR"),
		SysproPassword:        checkPlaceholder("SYSPRO_PASSWORD", os.Getenv("SYSPRO_PASSWORD")), // blank operator password is valid, PLACEHOLDER is not
		SysproCompanyID:       get("SYSPRO_COMPANY_ID"),
		SysproCompanyPassword: checkPlaceholder("SYSPRO_COMPANY_PASSWORD", os.Getenv("SYSPRO_COMPANY_PASSWORD")), // blank is valid for companies without a company-level password

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
	c.SQLServerDSN = checkPlaceholder("SQLSERVER_DSN", os.Getenv("SQLSERVER_DSN"))

	// SORTOI <AllocationAction> — primary-source enum confirmed by SYSPRO
	// itself (tried "S", got: "XML element 'allocationaction' has a value
	// of 'S'. It should be 'F / B / A'"). Valid values:
	//   F = Force / Fulfil — most aggressive, still ignored when the
	//       company-level Back orders preference is set to Manual
	//   B = Back order — explicit back-order (don't ship)
	//   A = Auto — let SYSPRO apply its normal allocation rules; honours
	//       the Setup Options > Preferences > Distribution > Sales Orders
	//       > Back orders preference
	// Default "A" is the resilient choice: once Sarah flips the Back
	// orders preference from Manual to Automatic at company level,
	// allocation starts working without another code deploy. "F" was
	// verified to also back-order against a company-preference override,
	// so it buys us nothing extra.
	c.SysproAllocationAction = os.Getenv("SYSPRO_ALLOCATION_ACTION")
	if c.SysproAllocationAction == "" {
		c.SysproAllocationAction = "A"
	}

	c.SysproTaxCodeMap = os.Getenv("SYSPRO_TAX_CODE_MAP")
	// Default: Rectella confirmed A=20%, B=5%, Z=0%.
	if c.SysproTaxCodeMap == "" {
		c.SysproTaxCodeMap = "0.20:A,0.05:B,0.00:Z"
	}

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

	// Payments sync default is OFF. Setting PAYMENTS_SYNC_INTERVAL starts
	// the polling loop. ARSPAY_CASH_BOOK and ARSPAY_PAYMENT_TYPE must
	// also be set or main.go refuses to wire the syncer.
	if raw := os.Getenv("PAYMENTS_SYNC_INTERVAL"); raw != "" {
		c.PaymentsSyncInterval, err = time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid duration for PAYMENTS_SYNC_INTERVAL: %w", err)
		}
	}
	c.ArspayCashBook = os.Getenv("ARSPAY_CASH_BOOK")
	c.ArspayPaymentType = os.Getenv("ARSPAY_PAYMENT_TYPE")

	// Daily cash-receipt email config. All-or-nothing: if any SMTP
	// field is set but not all, we still load — the daily report
	// wiring in main.go enables the job only when the full set is
	// present.
	c.SMTPHost = os.Getenv("SMTP_HOST")
	if p := os.Getenv("SMTP_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			c.SMTPPort = n
		} else {
			return nil, fmt.Errorf("invalid SMTP_PORT: %q", p)
		}
	}
	c.SMTPUsername = os.Getenv("SMTP_USERNAME")
	c.SMTPPassword = checkPlaceholder("SMTP_PASSWORD", os.Getenv("SMTP_PASSWORD"))
	c.SMTPFrom = os.Getenv("SMTP_FROM")
	c.SMTPUseTLS = os.Getenv("SMTP_USE_TLS") == "true"
	if raw := os.Getenv("CREDIT_CONTROL_TO"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				c.CreditControlTo = append(c.CreditControlTo, s)
			}
		}
	}
	c.DailyReportHour = 1
	if h := os.Getenv("DAILY_REPORT_HOUR"); h != "" {
		n, err := strconv.Atoi(h)
		if err != nil || n < 0 || n > 23 {
			return nil, fmt.Errorf("invalid DAILY_REPORT_HOUR: %q", h)
		}
		c.DailyReportHour = n
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
