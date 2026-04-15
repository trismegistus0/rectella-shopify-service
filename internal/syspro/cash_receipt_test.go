package syspro

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestCashReceipt_PostingPeriod(t *testing.T) {
	r := CashReceipt{
		PostedAt: time.Date(2026, 4, 15, 12, 30, 0, 0, time.UTC),
	}
	if got := r.PostingPeriod(); got != "202604" {
		t.Errorf("PostingPeriod() = %q, want %q", got, "202604")
	}

	r.PostedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if got := r.PostingPeriod(); got != "202601" {
		t.Errorf("PostingPeriod() = %q, want %q", got, "202601")
	}
}

func TestPostCashReceipt_NotImplemented(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewEnetClient("http://unused", "op", "pw", "RILT", "", "WEBS", logger)

	r := CashReceipt{
		CustomerCode:  "WEBS01",
		InvoiceNumber: "#BBQ1010",
		Amount:        125.00,
		BankCharges:   3.75,
		Currency:      "GBP",
		PostedAt:      time.Now(),
	}
	_, err := c.PostCashReceipt(context.Background(), r)
	if !errors.Is(err, ErrCashReceiptNotImplemented) {
		t.Errorf("want ErrCashReceiptNotImplemented, got %v", err)
	}
}

func TestPostCashReceipt_Validation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewEnetClient("http://unused", "op", "pw", "RILT", "", "WEBS", logger)

	tests := []struct {
		name string
		r    CashReceipt
	}{
		{"missing customer", CashReceipt{InvoiceNumber: "#X", Amount: 10}},
		{"missing invoice", CashReceipt{CustomerCode: "WEBS01", Amount: 10}},
		{"zero amount", CashReceipt{CustomerCode: "WEBS01", InvoiceNumber: "#X", Amount: 0}},
		{"negative amount", CashReceipt{CustomerCode: "WEBS01", InvoiceNumber: "#X", Amount: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.PostCashReceipt(context.Background(), tc.r)
			if err == nil || errors.Is(err, ErrCashReceiptNotImplemented) {
				t.Errorf("want validation error, got %v", err)
			}
		})
	}
}
