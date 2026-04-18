// Package reconcile implements a periodic Shopify-to-database reconciliation
// sweep that catches orders missed by webhook delivery.
//
// Shopify retries failed webhook deliveries for 48 hours then drops them. If
// the service is down (deploy, outage, bad config) during that window, real
// customer orders can be lost silently. The sweeper closes this gap by
// polling the Shopify Admin REST API for recent orders and staging any that
// aren't already in our database. Idempotency is guaranteed by the existing
// webhook_events + shopify_order_id unique constraints.
package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

// OrderStore is the persistence surface the sweeper needs. *store.DB
// satisfies this implicitly via existing methods.
type OrderStore interface {
	ShopifyOrdersExist(ctx context.Context, shopifyOrderIDs []int64) (map[int64]bool, error)
	CreateOrder(ctx context.Context, event model.WebhookEvent, order model.Order, lines []model.OrderLine) error
}

// Sweeper fetches recent Shopify orders and stages any that haven't already
// been persisted.
type Sweeper struct {
	store           OrderStore
	storeURL        string // e.g. rectella.myshopify.com
	accessToken     string
	lookback        time.Duration // how far back to query each cycle
	interval        time.Duration // ticker interval
	httpClient      *http.Client
	logger          *slog.Logger
	baseURLOverride string // test hook

	mu sync.Mutex
}

// Option configures an optional Sweeper field.
type Option func(*Sweeper)

// WithLookback overrides the default 48h window. Useful for tests.
func WithLookback(d time.Duration) Option {
	return func(s *Sweeper) { s.lookback = d }
}

// WithHTTPClient overrides the default HTTP client. Useful for tests.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Sweeper) { s.httpClient = c }
}

// WithBaseURL overrides the Shopify base URL (for httptest).
// When set, the sweeper uses the URL as-is instead of constructing
// it from storeURL. The URL should include the /admin/api/<version> path.
func WithBaseURL(raw string) Option {
	return func(s *Sweeper) { s.baseURLOverride = raw }
}

func (s *Sweeper) resolveBaseURL() string {
	if s.baseURLOverride != "" {
		return strings.TrimRight(s.baseURLOverride, "/")
	}
	return fmt.Sprintf("https://%s/admin/api/2025-04", s.storeURL)
}

// New constructs a Sweeper. Returns nil if accessToken is empty (the sweep
// is disabled gracefully in that case, matching the service's degrade-gracefully
// pattern for stock and fulfilment sync).
func New(store OrderStore, storeURL, accessToken string, interval time.Duration, logger *slog.Logger, opts ...Option) *Sweeper {
	if accessToken == "" {
		logger.Warn("SHOPIFY_ACCESS_TOKEN missing, reconciliation sweep disabled")
		return nil
	}
	s := &Sweeper{
		store:       store,
		storeURL:    storeURL,
		accessToken: accessToken,
		lookback:    48 * time.Hour,
		interval:    interval,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		logger:      logger,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Run starts the periodic sweep loop. Blocks until ctx is cancelled.
// The first sweep fires IMMEDIATELY on startup (not after the first
// interval tick). This is load-bearing: the recovery path for a
// "paid-later after initial unpaid skip" order relies on a sweep running
// promptly after service restart. Subsequent sweeps fall back to the
// configured interval.
func (s *Sweeper) Run(ctx context.Context) {
	s.logger.Info("reconciliation sweeper started",
		"interval", s.interval,
		"lookback", s.lookback,
	)

	// First sweep — immediate.
	s.tick(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("reconciliation sweeper stopping")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Sweeper) tick(ctx context.Context) {
	if !s.mu.TryLock() {
		s.logger.Debug("reconciliation sweep already running, skipping tick")
		return
	}
	defer s.mu.Unlock()

	sweepCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if err := s.Sweep(sweepCtx); err != nil {
		s.logger.Error("reconciliation sweep failed", "error", err)
	}
}

// Sweep runs a single reconciliation cycle: fetch recent orders from Shopify,
// diff against the database, stage any gaps. Exported for ops-triggered runs.
func (s *Sweeper) Sweep(ctx context.Context) error {
	since := time.Now().Add(-s.lookback).UTC()

	orders, err := s.listOrders(ctx, since)
	if err != nil {
		return fmt.Errorf("listing shopify orders: %w", err)
	}

	if len(orders) == 0 {
		s.logger.Info("reconciliation sweep: no recent orders in shopify", "since", since.Format(time.RFC3339))
		return nil
	}

	ids := make([]int64, 0, len(orders))
	for _, o := range orders {
		ids = append(ids, o.ID)
	}
	existing, err := s.store.ShopifyOrdersExist(ctx, ids)
	if err != nil {
		return fmt.Errorf("checking existing orders: %w", err)
	}

	var staged, skippedUnpaid, alreadyPresent int
	for _, so := range orders {
		if existing[so.ID] {
			alreadyPresent++
			continue
		}
		if !isPaidStatus(so.FinancialStatus) {
			skippedUnpaid++
			s.logger.Info("reconciliation: skipping unpaid order",
				"shopify_order_id", so.ID,
				"order_number", so.Name,
				"financial_status", so.FinancialStatus,
			)
			continue
		}
		if err := s.stageOrder(ctx, so); err != nil {
			s.logger.Warn("reconciliation: failed to stage order",
				"shopify_order_id", so.ID,
				"order_number", so.Name,
				"error", err,
			)
			continue
		}
		staged++
	}

	s.logger.Info("reconciliation sweep complete",
		"found", len(orders),
		"already_present", alreadyPresent,
		"skipped_unpaid", skippedUnpaid,
		"staged", staged,
	)
	return nil
}

func (s *Sweeper) listOrders(ctx context.Context, since time.Time) ([]shopifyOrder, error) {
	q := url.Values{}
	q.Set("status", "any")
	q.Set("created_at_min", since.Format(time.RFC3339))
	q.Set("limit", "250")
	q.Set("fields", "id,name,email,created_at,total_price,financial_status,taxes_included,shipping_address,line_items,shipping_lines,gateway,payment_gateway_names")

	reqURL := s.resolveBaseURL() + "/orders.json?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("X-Shopify-Access-Token", s.accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET orders.json: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("shopify returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload listOrdersResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return payload.Orders, nil
}

func (s *Sweeper) stageOrder(ctx context.Context, so shopifyOrder) error {
	order, lines := so.toDomain()

	event := model.WebhookEvent{
		WebhookID: fmt.Sprintf("reconcile-%d-%d", so.ID, time.Now().Unix()),
		Topic:     "orders/reconcile",
	}

	if err := s.store.CreateOrder(ctx, event, order, lines); err != nil {
		return err
	}
	s.logger.Info("reconciliation: staged missing order",
		"shopify_order_id", so.ID,
		"order_number", so.Name,
	)
	return nil
}

// isPaidStatus mirrors the webhook handler: only paid / partially_paid flow
// through to SYSPRO. Everything else (pending, unpaid, refunded, voided,
// expired, empty) is skipped.
func isPaidStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "paid", "partially_paid":
		return true
	default:
		return false
	}
}

// ---------- Shopify REST response types ----------

type listOrdersResponse struct {
	Orders []shopifyOrder `json:"orders"`
}

type shopifyOrder struct {
	ID                  int64                 `json:"id"`
	Name                string                `json:"name"`
	Email               string                `json:"email"`
	CreatedAt           string                `json:"created_at"`
	TotalPrice          string                `json:"total_price"`
	FinancialStatus     string                `json:"financial_status"`
	TaxesIncluded       bool                  `json:"taxes_included"`
	Gateway             string                `json:"gateway"`
	PaymentGatewayNames []string              `json:"payment_gateway_names"`
	ShippingAddress     *shopifyAddress       `json:"shipping_address"`
	LineItems           []shopifyLineItem     `json:"line_items"`
	ShippingLines       []shopifyShippingLine `json:"shipping_lines"`
}

type shopifyAddress struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Address1  string `json:"address1"`
	Address2  string `json:"address2"`
	City      string `json:"city"`
	Province  string `json:"province"`
	Zip       string `json:"zip"`
	Country   string `json:"country"`
	Phone     string `json:"phone"`
}

type shopifyLineItem struct {
	SKU           string       `json:"sku"`
	Quantity      int          `json:"quantity"`
	Price         string       `json:"price"`
	TotalDiscount string       `json:"total_discount"`
	TaxLines      []shopifyTax `json:"tax_lines"`
}

type shopifyTax struct {
	Price string  `json:"price"`
	Rate  float64 `json:"rate"`
	Title string  `json:"title"`
}

type shopifyShippingLine struct {
	Title    string       `json:"title"`
	Price    string       `json:"price"`
	TaxLines []shopifyTax `json:"tax_lines"`
}

func (so shopifyOrder) toDomain() (model.Order, []model.OrderLine) {
	rawPayload, _ := json.Marshal(so) // best-effort; parseable by definition

	order := model.Order{
		ShopifyOrderID:  so.ID,
		OrderNumber:     so.Name,
		Status:          model.OrderStatusPending,
		CustomerAccount: "WEBS01",
		ShipEmail:       so.Email,
		RawPayload:      rawPayload,
	}

	if so.Gateway != "" {
		order.PaymentReference = so.Gateway
	} else if len(so.PaymentGatewayNames) > 0 {
		order.PaymentReference = strings.Join(so.PaymentGatewayNames, ", ")
	}

	if v, err := strconv.ParseFloat(so.TotalPrice, 64); err == nil {
		order.PaymentAmount = v
	}

	if t, err := time.Parse(time.RFC3339, so.CreatedAt); err == nil {
		order.OrderDate = t
	} else {
		order.OrderDate = time.Now()
	}

	if a := so.ShippingAddress; a != nil {
		order.ShipFirstName = a.FirstName
		order.ShipLastName = a.LastName
		order.ShipAddress1 = a.Address1
		order.ShipAddress2 = a.Address2
		order.ShipCity = a.City
		order.ShipProvince = a.Province
		order.ShipPostcode = a.Zip
		order.ShipCountry = a.Country
		order.ShipPhone = a.Phone
	}

	for _, sl := range so.ShippingLines {
		if v, err := strconv.ParseFloat(sl.Price, 64); err == nil {
			order.ShippingAmount += v
		}
	}

	lines := make([]model.OrderLine, 0, len(so.LineItems))
	for _, li := range so.LineItems {
		line := model.OrderLine{SKU: li.SKU, Quantity: li.Quantity}
		if v, err := strconv.ParseFloat(li.Price, 64); err == nil {
			line.UnitPrice = v
		}
		if v, err := strconv.ParseFloat(li.TotalDiscount, 64); err == nil {
			line.Discount = v
		}
		for _, t := range li.TaxLines {
			if v, err := strconv.ParseFloat(t.Price, 64); err == nil {
				line.Tax += v
			}
		}
		lines = append(lines, line)
	}

	return order, lines
}
