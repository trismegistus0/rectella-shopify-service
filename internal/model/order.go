package model

import "time"

type OrderStatus string

const (
	OrderStatusPending    OrderStatus = "pending"
	OrderStatusProcessing OrderStatus = "processing"
	OrderStatusSubmitted  OrderStatus = "submitted"
	OrderStatusFailed     OrderStatus = "failed"
	OrderStatusDeadLetter OrderStatus = "dead_letter"
	OrderStatusFulfilled  OrderStatus = "fulfilled"
	OrderStatusCancelled  OrderStatus = "cancelled"
)

type Order struct {
	ID              int64
	ShopifyOrderID  int64
	OrderNumber     string
	Status          OrderStatus
	CustomerAccount string // always "WEBS01"

	// Ship-to address
	ShipFirstName string
	ShipLastName  string
	ShipAddress1  string
	ShipAddress2  string
	ShipCity      string
	ShipProvince  string
	ShipPostcode  string
	ShipCountry   string
	ShipPhone     string
	ShipEmail     string

	// Payment
	PaymentReference string
	PaymentAmount    float64

	// Shipping
	ShippingAmount float64

	// Raw webhook payload
	RawPayload []byte

	// SYSPRO reference
	SysproOrderNumber string

	// Queue management
	Attempts  int
	LastError string

	OrderDate time.Time
	CreatedAt time.Time
	UpdatedAt time.Time

	// Fulfilment tracking
	FulfilledAt         *time.Time // nil until Shopify fulfilment created
	ShopifyFulfilmentID string     // Shopify GID, empty until fulfilled
}

type OrderLine struct {
	ID        int64
	OrderID   int64
	SKU       string
	Quantity  int
	UnitPrice float64
	Discount  float64
	Tax       float64
}

// OrderWithLines pairs an order with its line items for batch processing.
type OrderWithLines struct {
	Order Order
	Lines []OrderLine
}

type WebhookEvent struct {
	ID         int64
	WebhookID  string
	Topic      string
	ReceivedAt time.Time
}
