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
	// UTF-8 BOM so Excel renders the £ glyphs correctly. Without this
	// the body bytes (£ = 0xC2 0xA3 in UTF-8) get decoded as Windows-1252
	// and show up as "Â£".
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(&buf)
	header := []string{
		"Customer (SYSPRO)",
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
		// "WEBS01  #BBQ1001  £8.00  £1.12  £6.88"). The Customer column
		// is always WEBS01 — single-customer Phase 1 invariant — so credit
		// control can post each row directly to that account in ARSPAY.
		row := []string{
			"WEBS01",
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

// ValidateCashReceiptCSV runs row-level sanity checks on a transaction
// slice and returns a human-readable anomaly description if anything
// looks suspicious — empty string means OK.
//
// Existence rationale: the £0-fees-for-10-days bug (2026-04-15 → 04-25)
// shipped a plausible-looking email every morning and was only caught
// by a human eyeballing the CSV. This guard fires the same banner an
// operator would have spotted, attached to the email subject + body.
//
// Rules (kept conservative — false positives erode trust):
//
//  1. Non-empty txns + sum of fees == 0 → fee extraction is broken.
//     Today every Rectella gateway (Shopify Payments, PayPal) returns
//     a non-zero fee. A clean zero-fee day is not a real scenario.
//
//  2. Non-empty txns + sum of gross == 0 → amount parse broken or
//     listing returned only refunds (which we filter, but defence
//     in depth).
//
// Empty txns slice is NOT an anomaly — it's a legitimate zero-day
// (closure, holiday). The reporter still sends the email so credit
// control can see the job is alive.
func ValidateCashReceiptCSV(txns []ShopifyTransaction) string {
	if len(txns) == 0 {
		return ""
	}
	var gross, fee float64
	for _, t := range txns {
		gross += t.Gross
		fee += t.Fee
	}
	if fee == 0 {
		return fmt.Sprintf("%d transactions but zero processor fees in total — likely a fee-extraction bug. Cross-check Shopify admin → Finances → Payouts.", len(txns))
	}
	if gross == 0 {
		return fmt.Sprintf("%d transactions but zero gross total — listing or amount parse broken.", len(txns))
	}
	return ""
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

// BuildRangeCSV serialises a multi-day slice of transactions into a CSV
// with a leading "Date" column. Used by the operator backfill flow
// (`cmd/send-report --from --to`) to send one bulk email covering a
// validation window. Daily reports keep using BuildCSV for back-compat.
//
// `start` and `end` are kept on the signature for parity with BuildCSV
// and future header-row enrichment; they are not currently used in the
// row data (each row carries its own ProcessedAt timestamp).
func BuildRangeCSV(start, end time.Time, txns []ShopifyTransaction) ([]byte, error) {
	_ = start
	_ = end
	sorted := make([]ShopifyTransaction, len(txns))
	copy(sorted, txns)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].ProcessedAt.Equal(sorted[j].ProcessedAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].ProcessedAt.Before(sorted[j].ProcessedAt)
	})

	var buf bytes.Buffer
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(&buf)
	header := []string{
		"Date",
		"Customer (SYSPRO)",
		"Shopify Reference",
		"Order Value",
		"Charges",
		"Receipt Value",
	}
	if err := w.Write(header); err != nil {
		return nil, fmt.Errorf("writing csv header: %w", err)
	}
	for _, t := range sorted {
		row := []string{
			t.ProcessedAt.UTC().Format("2006-01-02"),
			"WEBS01",
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
