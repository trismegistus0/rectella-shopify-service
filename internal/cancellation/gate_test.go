package cancellation

import (
	"testing"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

// stubSorqry is a trivial SorqryResult implementation for unit tests.
// It avoids pulling in the real syspro.SORQRYResult so the tests stay
// self-contained.
type stubSorqry struct {
	status string
}

func (s stubSorqry) GetOrderStatus() string { return s.status }

func TestClassify_NoSysproOrderNumber(t *testing.T) {
	got := Classify(model.Order{SysproOrderNumber: ""}, stubSorqry{status: "1"})
	if got != CancellablePreSYSPRO {
		t.Errorf("empty SysproOrderNumber should be CancellablePreSYSPRO, got %q", got)
	}
}

func TestClassify_NilSorqryResult(t *testing.T) {
	got := Classify(model.Order{SysproOrderNumber: "016005"}, nil)
	if got != ReviewAllocated {
		t.Errorf("nil sorqryResult should default to ReviewAllocated (safe), got %q", got)
	}
}

func TestClassify_StatusBranches(t *testing.T) {
	order := model.Order{SysproOrderNumber: "016005"}

	cases := []struct {
		status string
		want   Disposition
	}{
		// Cancellable in SYSPRO — no picking yet
		{"0", CancellableInSYSPRO},
		{"1", CancellableInSYSPRO},
		{"2", CancellableInSYSPRO},
		{"3", CancellableInSYSPRO},
		// Allocated but not picked — review
		{"4", ReviewAllocated},
		// Too late — picked / dispatched
		{"6", TooLatePicked},
		{"7", TooLatePicked},
		// Too late — invoiced / shipped
		{"8", TooLateInvoiced},
		{"9", TooLateInvoiced},
		// Already cancelled
		{"*", AlreadyCancelled},
		{`\`, AlreadyCancelled},
		// Unknown / future-delivery / credit-hold → safe default
		{"F", ReviewAllocated},
		{"S", ReviewAllocated},
		{"", ReviewAllocated},
		{"Z", ReviewAllocated},
	}

	for _, tc := range cases {
		t.Run("status="+tc.status, func(t *testing.T) {
			got := Classify(order, stubSorqry{status: tc.status})
			if got != tc.want {
				t.Errorf("status %q: got %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassify_StatusWithWhitespace(t *testing.T) {
	order := model.Order{SysproOrderNumber: "016005"}
	// SYSPRO pads status fields with trailing whitespace in some responses.
	got := Classify(order, stubSorqry{status: "  1  "})
	if got != CancellableInSYSPRO {
		t.Errorf("whitespace-padded status should trim to 1: got %q", got)
	}
}
