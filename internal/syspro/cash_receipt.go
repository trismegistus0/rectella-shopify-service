package syspro

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CashReceipt is the input to PostCashReceipt. The middleware turns one
// settled Shopify transaction into one ARSTPY <Item><Payments> entry.
//
// SYSPRO 8 implementation note: the e.net business object name is
// **ARSTPY** (the .dll on disk and the value passed as
// `BusinessObject=`). The UI program with the friendly mnemonic
// "ARSPAY" is a separate thing. Calling BusinessObject=ARSPAY returns
// `e.net exception 100000` because no business object by that name is
// registered. See docs/handover.md §5 for the canonical XmlIn shape.
type CashReceipt struct {
	// CustomerCode is the SYSPRO AR customer to credit (always "WEBS01"
	// for Phase 1).
	CustomerCode string

	// Bank is the SYSPRO cashbook code (e.g. "BANK1") configured in
	// AR Setup. Set by the syncer from the ARSPAY_CASH_BOOK env var.
	Bank string

	// PaymentType is an installation-specific payment-method code
	// configured in AR Setup → Browse on Payment Codes (e.g. "01" for
	// cheque, "EF" for EFT). Set by the syncer from the
	// ARSPAY_PAYMENT_TYPE env var.
	PaymentType string

	// InvoiceNumber is the customer-facing reference. We use the
	// Shopify order name (e.g. "#BBQ1010") so credit control can
	// cross-match against Shopify on the customer's open AR account.
	// Maps to <CheckNumber>; SYSPRO max length 30.
	InvoiceNumber string

	// Amount is the GROSS paid by the customer. SYSPRO posts this to
	// the customer's open balance and routes (Amount − BankCharges) to
	// the cashbook. Maps to <CheckAmount>.
	Amount float64

	// BankCharges is the gateway fee (Shopify Payments / Stripe). SYSPRO
	// auto-routes this to the bank-charges GL configured against the
	// Bank record. Maps to <BankCharges>.
	BankCharges float64

	// Currency is informational only on a single-currency install.
	// Phase 1 is GBP only. Not emitted in the XmlIn.
	Currency string

	// PaymentMethod is the Shopify gateway name (e.g.
	// "shopify_payments", "paypal"). Logged for cross-matching but not
	// emitted in the XmlIn — SYSPRO uses PaymentType for that.
	PaymentMethod string

	// PostedAt is the Shopify settlement timestamp. Used as both
	// <CheckDate> (formatted CCYYMMDD) and the source of the SYSPRO
	// posting period (YYYYMM, derived from PostedAt.UTC()).
	PostedAt time.Time
}

// PostingPeriod formats PostedAt into SYSPRO's YYYYMM posting-period
// string. Go's reference-date layout "200601" produces e.g. "202604"
// for April 2026. Note: ARSTPY does NOT accept a per-transaction
// posting-period override — SYSPRO uses the operator's current open
// period at submission time. This helper is retained for logging and
// for the period-closed error path.
func (r CashReceipt) PostingPeriod() string {
	return r.PostedAt.UTC().Format("200601")
}

// arstpyParams maps to <PostArPayment><Parameters>...</Parameters>
// sent as XmlParameters on every ARSTPY call. The root element name
// MUST be PostArPayment (matching the BO), not Payments.
//
// IgnoreWarnings is Y/N (not the W enum used by SORTOI).
type arstpyParams struct {
	XMLName        xml.Name `xml:"PostArPayment"`
	ValidateOnly   string   `xml:"Parameters>ValidateOnly"`
	IgnoreWarnings string   `xml:"Parameters>IgnoreWarnings"`
}

// arstpyPaymentItem is the single <Item><Payments> node inside
// XmlIn. Field order follows the SYSPRO 8 canonical schema (see
// docs/handover.md §5 and the Perplexity research findings).
type arstpyPaymentItem struct {
	Customer    string  `xml:"Customer"`
	Bank        string  `xml:"Bank"`
	PaymentType string  `xml:"PaymentType"`
	CheckAmount string  `xml:"CheckAmount"`
	BankCharges string  `xml:"BankCharges,omitempty"`
	CheckNumber string  `xml:"CheckNumber,omitempty"`
	CheckDate   string  `xml:"CheckDate"`
	// InvoiceToPay omitted entirely — receipt posts on-account /
	// unallocated, which is correct for Shopify orders that aren't yet
	// invoiced in SYSPRO. Adding <InvoiceToPay> would require matching
	// the receipt to a specific SYSPRO invoice number, which we don't
	// have at posting time.
	_ float64 `xml:"-"`
}

// arstpyData maps to the full XmlIn document.
type arstpyData struct {
	XMLName               xml.Name          `xml:"PostArPayment"`
	XmlnsXsd              string            `xml:"xmlns:xsd,attr"`
	NoNamespaceSchemaLoc  string            `xml:"xsd:noNamespaceSchemaLocation,attr"`
	Payment               arstpyPaymentItem `xml:"Item>Payments"`
}

// arstpyResponse parses the response from a successful or failed ARSTPY
// post. SYSPRO returns HTTP 200 in both cases — Status=0 indicates
// success, non-zero is a business rejection (closed period, validation
// failure, etc.) with a human-readable Message.
//
// Different SYSPRO 8 ports surface the receipt reference under
// different element names (JournalNumber, CashJournal, Reference) — we
// capture all candidates and return the first non-empty one.
type arstpyResponse struct {
	XMLName       xml.Name `xml:"PostArPayment"`
	Status        string   `xml:"Status"`
	Message       string   `xml:"Message"`
	JournalNumber string   `xml:"JournalNumber"`
	CashJournal   string   `xml:"CashJournal"`
	Reference     string   `xml:"Reference"`
}

// BuildARSTPYParams returns the canonical XmlParameters for posting a
// single cash receipt. validateOnly=true sends <ValidateOnly>Y</…> so
// SYSPRO lints the XML without committing — used by cmd/arspaytest in
// validate mode to iterate on the schema without polluting AR.
func BuildARSTPYParams(validateOnly bool) (string, error) {
	flag := "N"
	if validateOnly {
		flag = "Y"
	}
	p := arstpyParams{
		ValidateOnly:   flag,
		IgnoreWarnings: "Y",
	}
	body, err := xml.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal ARSTPY params: %w", err)
	}
	return xml.Header + string(body), nil
}

// BuildARSTPYData returns the XmlIn for a single cash receipt. Returns
// an error if any required field is missing or invalid — fail-loud at
// the boundary so the syncer can mark the row failed and stop retrying
// against a malformed input.
func BuildARSTPYData(r CashReceipt) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	data := arstpyData{
		XmlnsXsd:             "http://www.w3.org/2000/10/XMLSchema-instance",
		NoNamespaceSchemaLoc: "ARSTPY.XSD",
		Payment: arstpyPaymentItem{
			Customer:    r.CustomerCode,
			Bank:        r.Bank,
			PaymentType: r.PaymentType,
			CheckAmount: fmt.Sprintf("%.2f", r.Amount),
			BankCharges: fmt.Sprintf("%.2f", r.BankCharges),
			CheckNumber: truncate(r.InvoiceNumber, 30),
			CheckDate:   r.PostedAt.UTC().Format("20060102"),
		},
	}
	body, err := xml.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal ARSTPY data: %w", err)
	}
	return xml.Header + string(body), nil
}

func (r CashReceipt) validate() error {
	switch {
	case r.CustomerCode == "":
		return errors.New("CustomerCode required")
	case r.Bank == "":
		return errors.New("Bank (cashbook code) required")
	case r.PaymentType == "":
		return errors.New("PaymentType required")
	case r.InvoiceNumber == "":
		return errors.New("InvoiceNumber required")
	case r.Amount <= 0:
		return fmt.Errorf("Amount must be positive, got %.2f", r.Amount)
	case r.PostedAt.IsZero():
		return errors.New("PostedAt required")
	}
	return nil
}

// ParseARSTPYResponse extracts the receipt reference from a successful
// ARSTPY response or surfaces the SYSPRO error message on rejection.
//
// Returns: (receiptRef, err). receiptRef is the journal/cash-receipt
// number SYSPRO assigned (empty if SYSPRO didn't return one — some
// clean-import responses don't). err is non-nil for business
// rejections (Status != "0" || != "").
func ParseARSTPYResponse(respXML string) (string, error) {
	var r arstpyResponse
	if err := xml.Unmarshal([]byte(respXML), &r); err != nil {
		// SYSPRO sometimes returns a bare error string ("ERROR: e.net
		// exception raised: 100000") rather than wrapped XML. Detect
		// and return as a business error rather than a parse failure.
		trimmed := strings.TrimSpace(respXML)
		if strings.HasPrefix(trimmed, "ERROR") {
			return "", fmt.Errorf("ARSTPY rejected: %s", trimmed)
		}
		return "", fmt.Errorf("parse ARSTPY response: %w (body: %s)", err, trimmed)
	}
	if r.Status != "" && r.Status != "0" {
		msg := strings.TrimSpace(r.Message)
		if msg == "" {
			msg = "SYSPRO returned status " + r.Status
		}
		return "", fmt.Errorf("ARSTPY rejected: %s", msg)
	}
	for _, candidate := range []string{r.JournalNumber, r.CashJournal, r.Reference} {
		if c := strings.TrimSpace(candidate); c != "" {
			return c, nil
		}
	}
	return "", nil
}

// IsPeriodClosedError reports whether err is a SYSPRO "posting period
// closed" rejection. The payments syncer uses this to retry the receipt
// against the current open period (ARSTPY has no per-transaction period
// override — period is driven by the operator's current open period).
func IsPeriodClosedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "period") {
		return false
	}
	return strings.Contains(msg, "closed") || strings.Contains(msg, "not open")
}

// PostCashReceipt submits a single cash receipt to SYSPRO via the
// ARSTPY business object. Performs a full logon → ARSTPY transaction →
// logoff cycle under sessionMu, mirroring SubmitSalesOrder.
//
// Returns the receipt reference SYSPRO assigned (may be empty) and any
// error. Business errors (closed period, validation rejection) come
// back as plain errors with the SYSPRO message; use IsPeriodClosedError
// to detect the period-closed special case.
func (c *EnetClient) PostCashReceipt(ctx context.Context, r CashReceipt) (string, error) {
	paramsXML, err := BuildARSTPYParams(false)
	if err != nil {
		return "", fmt.Errorf("building ARSTPY params: %w", err)
	}
	dataXML, err := BuildARSTPYData(r)
	if err != nil {
		return "", fmt.Errorf("building ARSTPY data: %w", err)
	}

	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()

	guid, err := c.logon(ctx)
	if err != nil {
		return "", fmt.Errorf("syspro logon: %w", err)
	}
	defer func() {
		if lerr := c.logoff(ctx, guid); lerr != nil {
			c.logger.Warn("syspro logoff failed", "error", lerr)
		}
	}()

	c.logger.Debug("submitting ARSTPY",
		"customer", r.CustomerCode,
		"reference", r.InvoiceNumber,
		"amount", r.Amount,
		"bank_charges", r.BankCharges,
		"period", r.PostingPeriod(),
	)

	respXML, err := c.transaction(ctx, guid, "ARSTPY", paramsXML, dataXML)
	if err != nil {
		return "", fmt.Errorf("syspro ARSTPY transaction: %w", err)
	}

	return ParseARSTPYResponse(respXML)
}
