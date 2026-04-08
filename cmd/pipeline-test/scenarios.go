package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

type scenario struct {
	name           string
	description    string
	webhookID      string
	expectHTTP     int
	expectDBStatus string // empty = no DB row expected
	payload        []byte
	hmacSignature  string
	isDuplicate    bool
	dupOriginal    string
}

func buildScenarios(webhookSecret string) []scenario {
	ts := time.Now().UnixNano()

	payload1 := buildOrderPayload(ts+1, "#PIPE-001", []lineItem{
		{SKU: "CBBQ0001", Qty: 1, Price: "24.99", Title: "Bar-Be-Quick Instant BBQ", Tax: "5.00"},
	})
	payload2 := buildOrderPayload(ts+2, "#PIPE-002", []lineItem{
		{SKU: "CBBQ0001", Qty: 1, Price: "24.99", Title: "Bar-Be-Quick Instant BBQ", Tax: "5.00"},
		{SKU: "MBBQ0159", Qty: 2, Price: "12.99", Title: "BBQ Firelighters", Tax: "5.20"},
	})
	payload5 := buildOrderPayload(ts+5, "#PIPE-005", []lineItem{
		{SKU: "", Qty: 1, Price: "10.00", Title: "Missing SKU Item", Tax: "2.00"},
	})

	wid1 := fmt.Sprintf("pipe-001-%d", ts)

	return []scenario{
		{
			name: "#PIPE-001", description: "Single line, happy path",
			webhookID: wid1, expectHTTP: 200, expectDBStatus: "submitted",
			payload: payload1, hmacSignature: signPayload(payload1, webhookSecret),
		},
		{
			name: "#PIPE-002", description: "Multi-line order",
			webhookID: fmt.Sprintf("pipe-002-%d", ts), expectHTTP: 200, expectDBStatus: "submitted",
			payload: payload2, hmacSignature: signPayload(payload2, webhookSecret),
		},
		{
			name: "#PIPE-003", description: "Duplicate webhook (same ID as #1)",
			webhookID: wid1, expectHTTP: 200, expectDBStatus: "",
			payload: payload1, hmacSignature: signPayload(payload1, webhookSecret),
			isDuplicate: true, dupOriginal: "#PIPE-001",
		},
		{
			name: "#PIPE-004", description: "Invalid HMAC",
			webhookID: fmt.Sprintf("pipe-004-%d", ts), expectHTTP: 401, expectDBStatus: "",
			payload: payload1, hmacSignature: "aW52YWxpZA==",
		},
		{
			name: "#PIPE-005", description: "Missing SKU",
			webhookID: fmt.Sprintf("pipe-005-%d", ts), expectHTTP: 422, expectDBStatus: "",
			payload: payload5, hmacSignature: signPayload(payload5, webhookSecret),
		},
	}
}

type lineItem struct {
	SKU, Price, Title, Tax string
	Qty                    int
}

func buildOrderPayload(id int64, name string, lines []lineItem) []byte {
	items := make([]map[string]interface{}, len(lines))
	for i, l := range lines {
		items[i] = map[string]interface{}{
			"sku": l.SKU, "quantity": l.Qty, "price": l.Price, "title": l.Title,
			"tax_lines": []map[string]interface{}{{"price": l.Tax, "rate": 0.2, "title": "VAT"}},
		}
	}
	order := map[string]interface{}{
		"id": id, "name": name, "created_at": time.Now().Format(time.RFC3339),
		"shipping_address": map[string]interface{}{
			"first_name": "Pipeline", "last_name": "Test",
			"address1": "1 Test Street", "city": "Burnley",
			"province": "Lancashire", "zip": "BB1 1AA", "country": "United Kingdom",
		},
		"gateway": "shopify_payments", "total_price": "29.99",
		"payment_gateway_names": []string{"shopify_payments"},
		"line_items":            items,
	}
	body, _ := json.Marshal(order)
	return body
}

func signPayload(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
