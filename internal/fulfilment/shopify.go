package fulfilment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// FulfilmentClient handles Shopify Admin API GraphQL calls for fulfilment operations.
type FulfilmentClient struct {
	storeURL    string
	accessToken string
	baseURL     string
	httpClient  *http.Client
	logger      *slog.Logger
}

// NewFulfilmentClient creates a Shopify fulfilment client.
func NewFulfilmentClient(storeURL, accessToken string, logger *slog.Logger) *FulfilmentClient {
	return &FulfilmentClient{
		storeURL:    storeURL,
		accessToken: accessToken,
		baseURL:     fmt.Sprintf("https://%s/admin/api/2025-04/graphql.json", strings.TrimRight(storeURL, "/")),
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		logger:      logger,
	}
}

type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *FulfilmentClient) graphql(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
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

// GetFulfillmentOrderID returns the first OPEN fulfillment order GID for a Shopify order.
func (c *FulfilmentClient) GetFulfillmentOrderID(ctx context.Context, shopifyOrderID int64) (string, error) {
	const q = `query($id: ID!) {
  order(id: $id) {
    fulfillmentOrders(first: 10) {
      edges { node { id status } }
    }
  }
}`
	variables := map[string]any{
		"id": fmt.Sprintf("gid://shopify/Order/%d", shopifyOrderID),
	}
	data, err := c.graphql(ctx, q, variables)
	if err != nil {
		return "", fmt.Errorf("querying fulfillment orders: %w", err)
	}
	var result struct {
		Order struct {
			FulfillmentOrders struct {
				Edges []struct {
					Node struct {
						ID     string `json:"id"`
						Status string `json:"status"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"fulfillmentOrders"`
		} `json:"order"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing fulfillment orders: %w", err)
	}
	for _, edge := range result.Order.FulfillmentOrders.Edges {
		status := edge.Node.Status
		if status == "CLOSED" || status == "CANCELLED" {
			continue
		}
		return edge.Node.ID, nil
	}
	return "", fmt.Errorf("no open fulfillment orders found for order %d", shopifyOrderID)
}

// FulfilmentInput describes a fulfilment to create in Shopify.
type FulfilmentInput struct {
	FulfillmentOrderID string
	TrackingNumber     string // empty = no tracking
	Carrier            string // empty = no tracking
}

// CreateFulfillment creates a fulfilment in Shopify. Returns the fulfillment GID on success.
// If the order is already fulfilled, it logs at debug level and returns ("", nil).
func (c *FulfilmentClient) CreateFulfillment(ctx context.Context, input FulfilmentInput) (string, error) {
	const mutation = `mutation fulfillmentCreate($fulfillment: FulfillmentInput!) {
  fulfillmentCreate(fulfillment: $fulfillment) {
    fulfillment { id status }
    userErrors { field message }
  }
}`
	fulfillment := map[string]any{
		"notifyCustomer": true,
		"lineItemsByFulfillmentOrder": []map[string]any{
			{"fulfillmentOrderId": input.FulfillmentOrderID},
		},
	}
	if input.TrackingNumber != "" {
		fulfillment["trackingInfo"] = map[string]any{
			"number":  input.TrackingNumber,
			"company": input.Carrier,
		}
	}
	variables := map[string]any{
		"fulfillment": fulfillment,
	}
	data, err := c.graphql(ctx, mutation, variables)
	if err != nil {
		return "", fmt.Errorf("creating fulfillment: %w", err)
	}
	var result struct {
		FulfillmentCreate struct {
			Fulfillment *struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"fulfillment"`
			UserErrors []struct {
				Field   []string `json:"field"`
				Message string   `json:"message"`
			} `json:"userErrors"`
		} `json:"fulfillmentCreate"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing fulfillment response: %w", err)
	}
	for _, ue := range result.FulfillmentCreate.UserErrors {
		msg := strings.ToLower(ue.Message)
		if strings.Contains(msg, "already fulfilled") || strings.Contains(msg, "already been fulfilled") {
			c.logger.Debug("order already fulfilled, treating as success",
				"fulfillmentOrderId", input.FulfillmentOrderID,
				"message", ue.Message,
			)
			return "", nil
		}
	}
	if len(result.FulfillmentCreate.UserErrors) > 0 {
		return "", fmt.Errorf("fulfillment user error: %s", result.FulfillmentCreate.UserErrors[0].Message)
	}
	if result.FulfillmentCreate.Fulfillment != nil {
		return result.FulfillmentCreate.Fulfillment.ID, nil
	}
	return "", nil
}
