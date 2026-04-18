package payments

import (
	"strings"
	"testing"
	"time"
)

func TestBuildCSV_Empty(t *testing.T) {
	date := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	out, err := BuildCSV(date, nil)
	if err != nil {
		t.Fatalf("BuildCSV: %v", err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "Shopify Reference,Order Value,Charges,Receipt Value") {
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
	want1 := "#BBQ1001,£8.00,£1.12,£6.88"
	if lines[1] != want1 {
		t.Errorf("row 1 want %q, got %q", want1, lines[1])
	}
	want2 := "#BBQ1002,£75.00,£2.25,£72.75"
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
