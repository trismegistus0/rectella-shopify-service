package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ShopifyClient handles Shopify Admin API GraphQL calls for inventory management.
type ShopifyClient struct {
	storeURL             string
	accessToken          string
	configuredLocationID string
	skus                 []string

	mu         sync.Mutex
	locationID string            // resolved GID
	skuMap     map[string]string // SKU -> inventory item GID

	// baseURL is the full GraphQL endpoint. Overridden in tests.
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
}

// ShopifyOption configures optional ShopifyClient settings.
type ShopifyOption func(*ShopifyClient)

// WithBaseURL overrides the Shopify GraphQL endpoint URL (for testing/staging).
func WithBaseURL(url string) ShopifyOption {
	return func(c *ShopifyClient) { c.baseURL = url }
}

// NewShopifyClient creates a Shopify inventory client.
func NewShopifyClient(storeURL, accessToken, locationID string, skus []string, logger *slog.Logger, opts ...ShopifyOption) *ShopifyClient {
	c := &ShopifyClient{
		storeURL:             storeURL,
		accessToken:          accessToken,
		configuredLocationID: locationID,
		skus:                 skus,
		skuMap:               make(map[string]string),
		baseURL:              fmt.Sprintf("https://%s/admin/api/2025-04/graphql.json", strings.TrimRight(storeURL, "/")),
		httpClient:           &http.Client{Timeout: 30 * time.Second},
		logger:               logger,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *ShopifyClient) graphql(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling graphql request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("creating graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Access-Token", c.accessToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading graphql response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("parsing graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", gqlResp.Errors[0].Message)
	}
	return gqlResp.Data, nil
}

func (c *ShopifyClient) resolveLocation(ctx context.Context) error {
	if c.configuredLocationID != "" {
		c.locationID = c.configuredLocationID
		if !strings.HasPrefix(c.locationID, "gid://") {
			c.locationID = fmt.Sprintf("gid://shopify/Location/%s", c.locationID)
		}
		return nil
	}
	const q = `{ locations(first: 50) { edges { node { id name isActive } } } }`
	data, err := c.graphql(ctx, q, nil)
	if err != nil {
		return fmt.Errorf("querying locations: %w", err)
	}
	var result struct {
		Locations struct {
			Edges []struct {
				Node struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					IsActive bool   `json:"isActive"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing locations: %w", err)
	}
	for _, edge := range result.Locations.Edges {
		if edge.Node.IsActive {
			c.locationID = edge.Node.ID
			c.logger.Info("discovered Shopify location", "id", edge.Node.ID, "name", edge.Node.Name)
			return nil
		}
	}
	return fmt.Errorf("no active Shopify locations found")
}

func (c *ShopifyClient) resolveInventoryItems(ctx context.Context) error {
	var parts []string
	for _, sku := range c.skus {
		if _, ok := c.skuMap[sku]; !ok {
			parts = append(parts, fmt.Sprintf("sku:'%s'", sku))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	skuQuery := strings.Join(parts, " OR ")
	q := fmt.Sprintf(`{ inventoryItems(first: 50, query: %q) { edges { node { id sku } } } }`, skuQuery)
	data, err := c.graphql(ctx, q, nil)
	if err != nil {
		return fmt.Errorf("querying inventory items: %w", err)
	}
	var result struct {
		InventoryItems struct {
			Edges []struct {
				Node struct {
					ID  string `json:"id"`
					SKU string `json:"sku"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"inventoryItems"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing inventory items: %w", err)
	}
	for _, edge := range result.InventoryItems.Edges {
		c.skuMap[edge.Node.SKU] = edge.Node.ID
	}
	for _, sku := range c.skus {
		if _, ok := c.skuMap[sku]; !ok {
			c.logger.Warn("SKU not found in Shopify inventory", "sku", sku)
		}
	}
	return nil
}

// SetInventoryLevels pushes stock quantities to Shopify.
func (c *ShopifyClient) SetInventoryLevels(ctx context.Context, quantities map[string]int) error {
	c.mu.Lock()
	if c.locationID == "" {
		if err := c.resolveLocation(ctx); err != nil {
			c.mu.Unlock()
			return fmt.Errorf("resolving location: %w", err)
		}
	}
	if len(c.skuMap) < len(c.skus) {
		if err := c.resolveInventoryItems(ctx); err != nil {
			c.logger.Warn("resolving inventory items", "error", err)
		}
	}
	locationID := c.locationID
	skuMap := make(map[string]string, len(c.skuMap))
	for k, v := range c.skuMap {
		skuMap[k] = v
	}
	c.mu.Unlock()

	type quantityInput struct {
		InventoryItemID string `json:"inventoryItemId"`
		LocationID      string `json:"locationId"`
		Quantity        int    `json:"quantity"`
	}
	var items []quantityInput
	for sku, qty := range quantities {
		itemID, ok := skuMap[sku]
		if !ok {
			c.logger.Warn("skipping SKU without inventory item ID", "sku", sku)
			continue
		}
		items = append(items, quantityInput{
			InventoryItemID: itemID,
			LocationID:      locationID,
			Quantity:        qty,
		})
	}
	if len(items) == 0 {
		c.logger.Warn("no resolved SKUs for inventory update")
		return nil
	}

	const mutation = `mutation inventorySetQuantities($input: InventorySetQuantitiesInput!) {
  inventorySetQuantities(input: $input) {
    inventoryAdjustmentGroup {
      reason
      changes { name delta quantityAfterChange }
    }
    userErrors { code field message }
  }
}`
	variables := map[string]any{
		"input": map[string]any{
			"name":                  "available",
			"reason":                "correction",
			"ignoreCompareQuantity": true,
			"quantities":            items,
		},
	}
	data, err := c.graphql(ctx, mutation, variables)
	if err != nil {
		return fmt.Errorf("inventory set quantities: %w", err)
	}
	var result struct {
		InventorySetQuantities struct {
			UserErrors []struct {
				Code    string   `json:"code"`
				Field   []string `json:"field"`
				Message string   `json:"message"`
			} `json:"userErrors"`
		} `json:"inventorySetQuantities"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing set quantities response: %w", err)
	}
	if len(result.InventorySetQuantities.UserErrors) > 0 {
		ue := result.InventorySetQuantities.UserErrors[0]
		return fmt.Errorf("inventory user error: [%s] %s", ue.Code, ue.Message)
	}
	return nil
	return nil
}
