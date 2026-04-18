package syspro

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleReceipt() CashReceipt {
	return CashReceipt{
		CustomerCode:  "WEBS01",
		Bank:          "BANK1",
		PaymentType:   "01",
		InvoiceNumber: "#BBQ1010",
		Amount:        119.99,
		BankCharges:   2.45,
		Currency:      "GBP",
		PaymentMethod: "shopify_payments",
		PostedAt:      time.Date(2026, 4, 17, 14, 30, 0, 0, time.UTC),
	}
}

func TestBuildARSTPYParams(t *testing.T) {
	cases := []struct {
		name     string
		validate bool
		want     string
	}{
		{"post mode", false, "<ValidateOnly>N</ValidateOnly>"},
		{"validate mode", true, "<ValidateOnly>Y</ValidateOnly>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildARSTPYParams(tc.validate)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("expected XML to contain %q, got:\n%s", tc.want, got)
			}
			if !strings.Contains(got, "<IgnoreWarnings>Y</IgnoreWarnings>") {
				t.Errorf("expected IgnoreWarnings=Y (not W), got:\n%s", got)
			}
			if !strings.HasPrefix(got, `<?xml`) {
				t.Errorf("expected XML declaration prefix, got:\n%s", got)
			}
			if !strings.Contains(got, "<PostArPayment>") {
				t.Errorf("expected root <PostArPayment>, got:\n%s", got)
			}
		})
	}
}

func TestBuildARSTPYData_HappyPath(t *testing.T) {
	r := sampleReceipt()
	got, err := BuildARSTPYData(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wants := []string{
		`<PostArPayment`,
		`xmlns:xsd="http://www.w3.org/2000/10/XMLSchema-instance"`,
		`xsd:noNamespaceSchemaLocation="ARSTPY.XSD"`,
		`<Item>`,
		`<Payments>`,
		`<Customer>WEBS01</Customer>`,
		`<Bank>BANK1</Bank>`,
		`<PaymentType>01</PaymentType>`,
		`<CheckAmount>119.99</CheckAmount>`,
		`<BankCharges>2.45</BankCharges>`,
		`<CheckNumber>#BBQ1010</CheckNumber>`,
		`<CheckDate>20260417</CheckDate>`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}

	// On-account / unallocated receipt — must NOT emit <InvoiceToPay>.
	if strings.Contains(got, "InvoiceToPay") {
		t.Errorf("unexpected <InvoiceToPay> — receipt should be on-account:\n%s", got)
	}
}

func TestBuildARSTPYData_DateUTC(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	r := sampleReceipt()
	r.PostedAt = time.Date(2026, 1, 1, 0, 30, 0, 0, loc)
	got, err := BuildARSTPYData(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := r.PostedAt.UTC().Format("20060102")
	if !strings.Contains(got, "<CheckDate>"+want+"</CheckDate>") {
		t.Errorf("expected <CheckDate>%s</CheckDate> (UTC), got:\n%s", want, got)
	}
}

func TestBuildARSTPYData_TruncatesCheckNumber(t *testing.T) {
	r := sampleReceipt()
	r.InvoiceNumber = strings.Repeat("X", 60)
	got, err := BuildARSTPYData(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "<CheckNumber>" + strings.Repeat("X", 30) + "</CheckNumber>"
	if !strings.Contains(got, want) {
		t.Errorf("expected truncation to 30 chars, got:\n%s", got)
	}
}

func TestBuildARSTPYData_ZeroBankCharges(t *testing.T) {
	r := sampleReceipt()
	r.BankCharges = 0
	got, err := BuildARSTPYData(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "<BankCharges>0.00</BankCharges>") {
		t.Errorf("expected <BankCharges>0.00</BankCharges>, got:\n%s", got)
	}
}

func TestBuildARSTPYData_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*CashReceipt)
		want string
	}{
		{"missing customer", func(r *CashReceipt) { r.CustomerCode = "" }, "CustomerCode required"},
		{"missing bank", func(r *CashReceipt) { r.Bank = "" }, "Bank"},
		{"missing payment type", func(r *CashReceipt) { r.PaymentType = "" }, "PaymentType required"},
		{"missing reference", func(r *CashReceipt) { r.InvoiceNumber = "" }, "InvoiceNumber required"},
		{"zero amount", func(r *CashReceipt) { r.Amount = 0 }, "Amount must be positive"},
		{"negative amount", func(r *CashReceipt) { r.Amount = -1 }, "Amount must be positive"},
		{"zero PostedAt", func(r *CashReceipt) { r.PostedAt = time.Time{} }, "PostedAt required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleReceipt()
			tc.mod(&r)
			_, err := BuildARSTPYData(r)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error to mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestParseARSTPYResponse_Success(t *testing.T) {
	cases := []struct {
		name string
		xml  string
		want string
	}{
		{
			"with JournalNumber",
			`<PostArPayment><Status>0</Status><JournalNumber>CR000123</JournalNumber></PostArPayment>`,
			"CR000123",
		},
		{
			"with CashJournal",
			`<PostArPayment><Status>0</Status><CashJournal>CJ-456</CashJournal></PostArPayment>`,
			"CJ-456",
		},
		{
			"with Reference",
			`<PostArPayment><Status>0</Status><Reference>REF-789</Reference></PostArPayment>`,
			"REF-789",
		},
		{
			"empty status, no reference (clean import)",
			`<PostArPayment></PostArPayment>`,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseARSTPYResponse(tc.xml)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestParseARSTPYResponse_BusinessErrors(t *testing.T) {
	cases := []struct {
		name        string
		xml         string
		wantContain string
	}{
		{
			"explicit error message",
			`<PostArPayment><Status>1</Status><Message>Posting period 202604 is not open</Message></PostArPayment>`,
			"Posting period 202604 is not open",
		},
		{
			"non-zero status, empty message",
			`<PostArPayment><Status>-1</Status></PostArPayment>`,
			"status -1",
		},
		{
			"bare ERROR string",
			`ERROR: e.net exception raised: 100000`,
			"100000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := ParseARSTPYResponse(tc.xml)
			if err == nil {
				t.Fatalf("expected error, got nil (ref=%q)", ref)
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("expected error to mention %q, got: %v", tc.wantContain, err)
			}
		})
	}
}

func TestIsPeriodClosedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"period closed", errors.New("ARSTPY rejected: Posting period 202604 is closed"), true},
		{"period not open", errors.New("Period 04/2026 is not open"), true},
		{"unrelated error", errors.New("connection refused"), false},
		{"period mentioned but not closed", errors.New("posting period missing"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPeriodClosedError(tc.err); got != tc.want {
				t.Errorf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestCashReceipt_PostingPeriod(t *testing.T) {
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), "202601"},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), "202612"},
		{time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC), "202604"},
	}
	for _, tc := range cases {
		r := CashReceipt{PostedAt: tc.t}
		if got := r.PostingPeriod(); got != tc.want {
			t.Errorf("PostedAt %v: want %q, got %q", tc.t, tc.want, got)
		}
	}
}
