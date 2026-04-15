package webhook

// Shopify webhook JSON payload types. Unexported -- only used for unmarshalling
// inbound webhooks within this package.

type shopifyOrder struct {
	ID                  int64                 `json:"id"`
	Name                string                `json:"name"`
	Email               string                `json:"email"`
	CreatedAt           string                `json:"created_at"`
	TotalPrice          string                `json:"total_price"`
	FinancialStatus     string                `json:"financial_status"`
	Gateway             string                `json:"gateway"`
	PaymentGatewayNames []string              `json:"payment_gateway_names"`
	ShippingAddress     *shopifyAddress       `json:"shipping_address"`
	LineItems           []shopifyLineItem     `json:"line_items"`
	ShippingLines       []shopifyShippingLine `json:"shipping_lines"`
	// Populated only by orders/cancelled webhooks. Both are absent/empty on
	// orders/create.
	CancelledAt  *string `json:"cancelled_at"`
	CancelReason string  `json:"cancel_reason"`
}

type shopifyShippingLine struct {
	Title string `json:"title"`
	Price string `json:"price"`
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
