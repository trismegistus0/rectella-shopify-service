// Package cancellation implements the classify-only gate for Shopify
// cancellation webhooks.
//
// Shopify's orders/cancelled webhook is a fire-and-forget notification:
// the order is ALREADY cancelled on Shopify's side, the customer has
// usually ALREADY been refunded, and Shopify has ALREADY restocked its
// own inventory snapshot. The only question for the middleware is
// whether to propagate the cancellation to the downstream SYSPRO
// sales order — which depends entirely on how far the physical
// warehouse workflow has progressed.
//
// Phase 1 (this package) does NOT propagate cancellations. It
// classifies the cancellation into one of six Dispositions based on
// the SYSPRO OrderStatus at time of webhook, persists the decision to
// the `order_cancellations` table, and alerts operations. Phase 2
// will add the actual cancel-in-SYSPRO action for the safe branches.
//
// Rationale: for any SYSPRO state past "allocated but not picked"
// (status 4), automation is unsafe because the physical action
// (stock already on a pallet, parcel already with the carrier,
// invoice already posted) cannot be modelled in an ERP reversal
// transaction. Ops must intervene case by case.
//
// SYSPRO OrderStatus codes (verified against
// https://help.syspro.com/syspro-7-update-1/sorpen.htm):
//
//	0  In process (entered, not yet released)
//	1  Open order              — safe to cancel in SYSPRO
//	2  Open back order         — safe to cancel in SYSPRO
//	3  Released back order     — safe to cancel in SYSPRO
//	4  In warehouse (allocated, not yet picked) — REVIEW: stock is
//	                            reserved and needs to be released
//	6  Ready to be dispatched (picking slip printed) — TOO LATE
//	7  Ready to be dispatched (stock prepared)       — TOO LATE
//	8  Ready to invoice (shipped, awaiting invoice)  — TOO LATE
//	9  Complete (fully invoiced)                     — TOO LATE
//	F  Forward (scheduled future delivery)
//	S  Suspense (credit hold)
//	*  Cancelled during entry                        — already cancelled
//	\  Cancelled after entry                         — already cancelled
package cancellation

import (
	"strings"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
	"github.com/trismegistus0/rectella-shopify-service/internal/syspro"
)

// Disposition is the classifier output for a cancellation webhook. Each
// value maps to a distinct ops workflow.
type Disposition string

const (
	// CancellablePreSYSPRO: order was never submitted to SYSPRO. Local
	// status is pending/failed/dead_letter. Safe to mark cancelled
	// locally with no SYSPRO side effect.
	CancellablePreSYSPRO Disposition = "cancellable_pre_syspro"

	// CancellableInSYSPRO: SYSPRO order exists and is in a state where
	// it can be safely cancelled (status 0-3). No stock has been
	// picked, no invoice has been posted. Phase 1 opens an ops ticket
	// rather than auto-cancelling.
	CancellableInSYSPRO Disposition = "cancellable_in_syspro"

	// ReviewAllocated: SYSPRO order is at status 4 (in warehouse, stock
	// allocated but not picked). Stock reservation needs to be released
	// before the order can be cancelled — a human judgement call.
	ReviewAllocated Disposition = "review_allocated"

	// TooLatePicked: SYSPRO order is at status 6 or 7 (picking slip
	// printed or ready to dispatch). The warehouse has physically
	// touched the goods. Do not auto-cancel; ops must intercept the
	// dispatch or accept the loss.
	TooLatePicked Disposition = "too_late_picked"

	// TooLateInvoiced: SYSPRO order is at status 8 or 9 (shipped and/or
	// invoiced). The customer has the goods or is about to. Ops must
	// initiate an RMA / return flow.
	TooLateInvoiced Disposition = "too_late_invoiced"

	// AlreadyCancelled: SYSPRO order is already marked cancelled
	// (status * or \). Idempotent no-op.
	AlreadyCancelled Disposition = "already_cancelled"
)

// SorqryResult is the narrow interface the classifier needs from a
// SORQRY response. Declared locally to keep this package free of a
// hard dependency on syspro's exact response struct — any caller that
// can produce a `.OrderStatus` string can run the classifier.
type SorqryResult interface {
	GetOrderStatus() string
}

// Classify inspects the local order row and the SYSPRO order state and
// returns the appropriate Disposition. Pure function — no I/O, no
// mutations. Accepts a nil sorqryResult (if the SORQRY call failed or
// couldn't be made) and returns ReviewAllocated as the safe default:
// when we don't know the state, never assume it's cancellable.
func Classify(localOrder model.Order, sorqryResult SorqryResult) Disposition {
	if localOrder.SysproOrderNumber == "" {
		return CancellablePreSYSPRO
	}
	if sorqryResult == nil {
		return ReviewAllocated
	}
	status := strings.TrimSpace(sorqryResult.GetOrderStatus())
	switch status {
	case "\\", "*":
		return AlreadyCancelled
	case "0", "1", "2", "3":
		return CancellableInSYSPRO
	case "4":
		return ReviewAllocated
	case "6", "7":
		return TooLatePicked
	case "8", "9":
		return TooLateInvoiced
	default:
		// Unknown status (F forward, S suspense, or any code we don't
		// explicitly handle) → never assume safe.
		return ReviewAllocated
	}
}

// SyspoResultAdapter wraps a *syspro.SORQRYResult so it satisfies the
// local SorqryResult interface. Kept here (not in the syspro package)
// so the syspro package has no dependency on cancellation.
type SyspoResultAdapter struct {
	Result *syspro.SORQRYResult
}

// GetOrderStatus returns the SYSPRO order status from the wrapped
// SORQRY result. Empty string if the wrapper or its inner result is
// nil.
func (a SyspoResultAdapter) GetOrderStatus() string {
	if a.Result == nil {
		return ""
	}
	return a.Result.OrderStatus
}
