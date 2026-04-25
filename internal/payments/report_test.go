package payments

import (
	"strings"
	"testing"
	"time"
)

func stripBOM(s string) string {
	return strings.TrimPrefix(s, string([]byte{0xEF, 0xBB, 0xBF}))
}

func TestValidateCashReceiptCSV_ZeroFees(t *testing.T) {
	txns := []ShopifyTransaction{
		{OrderNumber: "#A", Gross: 100, Fee: 0, Net: 100},
		{OrderNumber: "#B", Gross: 50, Fee: 0, Net: 50},
	}
	got := ValidateCashReceiptCSV(txns)
	if got == "" {
		t.Error("expected anomaly on zero-fees with non-empty txns")
	}
	if !strings.Contains(got, "zero processor fees") {
		t.Errorf("anomaly text doesn't mention fees: %q", got)
	}
}

func TestValidateCashReceiptCSV_NormalDay(t *testing.T) {
	txns := []ShopifyTransaction{
		{OrderNumber: "#A", Gross: 100, Fee: 2.50, Net: 97.50},
	}
	if got := ValidateCashReceiptCSV(txns); got != "" {
		t.Errorf("normal day flagged anomaly: %q", got)
	}
}

func TestValidateCashReceiptCSV_EmptyDay(t *testing.T) {
	if got := ValidateCashReceiptCSV(nil); got != "" {
		t.Errorf("empty day should not flag (legit closure day): %q", got)
	}
}

func TestValidateCashReceiptCSV_ZeroGross(t *testing.T) {
	txns := []ShopifyTransaction{
		{OrderNumber: "#A", Gross: 0, Fee: 0, Net: 0},
	}
	got := ValidateCashReceiptCSV(txns)
	if got == "" {
		t.Error("expected anomaly on zero-gross")
	}
	// zero-fee fires first; either is acceptable.
	if !strings.Contains(got, "zero") {
		t.Errorf("anomaly text expected to mention zero: %q", got)
	}
}

func TestBuildRangeCSV_HeaderAndDateColumn(t *testing.T) {
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC)
	txns := []ShopifyTransaction{
		{ID: 2, OrderNumber: "#BBQ1042", Gross: 27.98, Fee: 0.81, Net: 27.17,
			ProcessedAt: time.Date(2026, 4, 24, 14, 57, 56, 0, time.UTC)},
		{ID: 1, OrderNumber: "#BBQ1001", Gross: 8.00, Fee: 0.30, Net: 7.70,
			ProcessedAt: time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)},
	}
	out, err := BuildRangeCSV(start, end, txns)
	if err != nil {
		t.Fatalf("BuildRangeCSV: %v", err)
	}
	got := stripBOM(string(out))
	if !strings.HasPrefix(got, "Date,Customer (SYSPRO),Shopify Reference,Order Value,Charges,Receipt Value\n") {
		t.Errorf("wrong header: %q", got)
	}
	// Earliest first (sort stable on ProcessedAt).
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 rows, got %d lines:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[1], "2026-04-01,WEBS01,#BBQ1001,") {
		t.Errorf("first row wrong, got %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "2026-04-24,WEBS01,#BBQ1042,") {
		t.Errorf("second row wrong, got %q", lines[2])
	}
	if !strings.Contains(lines[1], "£8.00") || !strings.Contains(lines[1], "£0.30") {
		t.Errorf("row 1 missing £-prefixed amounts: %q", lines[1])
	}
}

func TestBuildRangeCSV_Empty(t *testing.T) {
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	out, err := BuildRangeCSV(start, end, nil)
	if err != nil {
		t.Fatalf("BuildRangeCSV: %v", err)
	}
	got := stripBOM(string(out))
	if strings.Count(got, "\n") != 1 {
		t.Errorf("empty range should have header only, got:\n%s", got)
	}
}

func TestBuildCSV_Empty(t *testing.T) {
	date := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	out, err := BuildCSV(date, nil)
	if err != nil {
		t.Fatalf("BuildCSV: %v", err)
	}
	got := string(out)
	// Strip the leading UTF-8 BOM so the header check matches Sarah's spec.
	got = strings.TrimPrefix(got, string([]byte{0xEF, 0xBB, 0xBF}))
	if !strings.HasPrefix(got, "Customer (SYSPRO),Shopify Reference,Order Value,Charges,Receipt Value") {
		t.Errorf("missing or wrong header (per Sarah's spec), got: %q", got)
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("empty CSV should have exactly 1 line (header), got:\n%s", got)
	}
}

func TestBuildCSV_HappyPath(t *testing.T) {
	date := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	txns := []ShopifyTransaction{
		{
			ID:             2,
			OrderNumber:    "#BBQ1002",
			CustomerEmail:  "b@example.com",
			Gross:          75.00,
			Fee:            2.25,
			Net:            72.75,
			Currency:       "GBP",
			ProcessedAt:    time.Date(2026, 4, 15, 11, 15, 0, 0, time.UTC),
			PaymentGateway: "shopify_payments",
		},
		{
			ID:             1,
			OrderNumber:    "#BBQ1001",
			CustomerEmail:  "a@example.com",
			Gross:          8.00,
			Fee:            1.12,
			Net:            6.88,
			Currency:       "GBP",
			ProcessedAt:    time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC),
			PaymentGateway: "shopify_payments",
		},
	}
	out, err := BuildCSV(date, txns)
	if err != nil {
		t.Fatalf("BuildCSV: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (header + 2 rows), got %d:\n%s", len(lines), out)
	}
	// Should be sorted by processed_at ascending — #BBQ1001 first.
	// Sarah's example: "#BBQ1001  £8.00  £1.12  £6.88" — verify
	// the format matches exactly (£ prefix, 2dp).
	want1 := "WEBS01,#BBQ1001,£8.00,£1.12,£6.88"
	if lines[1] != want1 {
		t.Errorf("row 1 want %q, got %q", want1, lines[1])
	}
	want2 := "WEBS01,#BBQ1002,£75.00,£2.25,£72.75"
	if lines[2] != want2 {
		t.Errorf("row 2 want %q, got %q", want2, lines[2])
	}
}

func TestSummariseTotals(t *testing.T) {
	txns := []ShopifyTransaction{
		{Gross: 100, Fee: 3, Net: 97},
		{Gross: 50, Fee: 1.5, Net: 48.5},
	}
	gross, fee, net, count := SummariseTotals(txns)
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if gross != 150 {
		t.Errorf("gross = %f, want 150", gross)
	}
	if fee != 4.5 {
		t.Errorf("fee = %f, want 4.5", fee)
	}
	if net != 145.5 {
		t.Errorf("net = %f, want 145.5", net)
	}
}
