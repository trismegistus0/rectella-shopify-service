package syspro

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrCashReceiptNotImplemented is returned while the ARSPAY XML builder
// is stubbed. The payments syncer catches this and keeps the row as
// `pending` rather than flipping it to `failed` — stops the attempt
// counter growing while we wait on Sarah's spec + Liz's sign-off.
var ErrCashReceiptNotImplemented = errors.New("ARSPAY cash receipt posting not yet implemented")

// CashReceipt is the input to PostCashReceipt. Shape derived from the
// canonical SYSPRO ARSPAY flow: operator posts gross `Amount`, bank
// charges calculated as gross − net, SYSPRO auto-derives the cash book
// GL via AR module integration settings.
type CashReceipt struct {
	// CustomerCode is the SYSPRO customer (always "WEBS01" for Phase 1).
	CustomerCode string
	// InvoiceNumber is the Shopify order reference used to cross-match
	// the payment in SYSPRO (e.g. "#BBQ1010").
	InvoiceNumber string
	// Amount is the GROSS paid by the customer. This is what SYSPRO
	// expects in the <Amount> field, not the net after fees.
	Amount float64
	// BankCharges is gross − net. SYSPRO auto-routes this to the bank
	// charges GL code configured on the AR module.
	BankCharges float64
	// Currency defaults to GBP. Non-GBP is out of scope for Phase 1.
	Currency string
	// PaymentMethod surfaces the Shopify gateway name in SYSPRO for
	// cross-matching (e.g. "shopify_payments", "paypal").
	PaymentMethod string
	// PostedAt is the timestamp SYSPRO uses for the GL entry. Normally
	// the Shopify `processed_at` time.
	PostedAt time.Time
}

// PostingPeriod formats PostedAt into SYSPRO's YYYYMM posting-period
// string. Go's reference-date layout "200601" produces e.g. "202601"
// for January 2026 — this is not a literal, it is the magic layout
// equivalent to Python's %Y%m.
func (r CashReceipt) PostingPeriod() string {
	return r.PostedAt.Format("200601")
}

// PostCashReceipt submits a single cash receipt to SYSPRO via the
// ARSPAY business object.
//
// STUB: currently returns ErrCashReceiptNotImplemented. The wire format
// blocks on Sarah's field spec for ARSPAY on RILT, and the feature
// itself blocks on Liz's sign-off to lift Phase-1's manual-posting
// posture. Once those land, this method will:
//
//  1. Build ARSPAY XmlIn with Amount, BankCharges, PostingPeriod, etc
//  2. Call c.transaction(ctx, guid, "ARSPAY", paramsXML, dataXML)
//  3. Parse the returned receipt reference
//  4. Return the reference string so the caller can record it in
//     payment_postings.syspro_receipt_ref
//
// The payments syncer catches ErrCashReceiptNotImplemented specially
// and does not increment the attempt counter.
func (c *EnetClient) PostCashReceipt(ctx context.Context, r CashReceipt) (string, error) {
	if r.CustomerCode == "" || r.InvoiceNumber == "" {
		return "", fmt.Errorf("cash receipt missing required fields")
	}
	if r.Amount <= 0 {
		return "", fmt.Errorf("cash receipt amount must be positive, got %.2f", r.Amount)
	}
	return "", ErrCashReceiptNotImplemented
}
