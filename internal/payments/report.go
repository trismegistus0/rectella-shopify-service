package payments

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
	"time"
)

// BuildCSV serialises a slice of ShopifyTransaction into a credit-control
// friendly CSV. Columns are exactly what Sarah specified for the
// Rectella daily report (2026-04-17): one row per settled payment with
// the four fields credit control needs to post a manual cash receipt
// in SYSPRO ARSPAY UI.
//
// The `date` argument is unused but preserved on the signature for
// callers that may want to add a column in future or for logging.
func BuildCSV(date time.Time, txns []ShopifyTransaction) ([]byte, error) {
	_ = date // intentionally unused — kept for signature stability
	// Stable ordering: processed_at ascending, then transaction ID.
	sorted := make([]ShopifyTransaction, len(txns))
	copy(sorted, txns)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].ProcessedAt.Equal(sorted[j].ProcessedAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].ProcessedAt.Before(sorted[j].ProcessedAt)
	})

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	header := []string{
		"Shopify Reference",
		"Order Value",
		"Charges",
		"Receipt Value",
	}
	if err := w.Write(header); err != nil {
		return nil, fmt.Errorf("writing csv header: %w", err)
	}
	for _, t := range sorted {
		// £ prefix matches Sarah's specified format (e.g.
		// "#BBQ1001  £8.00  £1.12  £6.88"). Excel renders the cell as
		// the literal string "£8.00" — credit control can sum the
		// numeric portion via SUMPRODUCT or by stripping the prefix.
		row := []string{
			t.OrderNumber,
			fmt.Sprintf("£%.2f", t.Gross),
			fmt.Sprintf("£%.2f", t.Fee),
			fmt.Sprintf("£%.2f", t.Net),
		}
		if err := w.Write(row); err != nil {
			return nil, fmt.Errorf("writing csv row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("csv writer: %w", err)
	}
	return buf.Bytes(), nil
}

// SummariseTotals returns gross/fee/net sums for the txns slice. Used
// by the email body so credit control sees the totals in plain text
// without opening the attachment.
func SummariseTotals(txns []ShopifyTransaction) (gross, fee, net float64, count int) {
	for _, t := range txns {
		gross += t.Gross
		fee += t.Fee
		net += t.Net
	}
	return gross, fee, net, len(txns)
}
