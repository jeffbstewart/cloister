// Package approval is the shared vocabulary for approval-gating: an operation
// held pending a human decision — a workspace mutation, a research request or
// response, anything a policy routes past a human.  The requesting worker
// stages the operation and registers it here; the state service is the
// pull-only decision authority; the status pages set the decision.
package approval

import (
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

// Decision is the lifecycle state of a gated op.
type Decision string

const (
	Pending  Decision = "pending"          // awaiting a human
	Approved Decision = "approved"         // human said yes → the worker applies
	Rejected Decision = "rejected"         // human said no → the worker discards
	Timeout  Decision = "rejected_timeout" // no decision within the approval timeout
)

// Resolved reports whether the decision is final (the worker can stop waiting).
func (d Decision) Resolved() bool { return d != Pending && d != "" }

// Record is the state service's account of one gated op.  The op's payload
// (e.g. a mutation's diff) lives in its own store, keyed by the same opId;
// this is the small, listable metadata + decision the status UI renders and
// the worker pulls.
type Record struct {
	OpID      runid.ID  `json:"opId"`
	Tool      string    `json:"tool"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"` // stamped by the state service's clock
	Decision  Decision  `json:"decision"`
	DecidedAt time.Time `json:"decidedAt,omitzero"` // when it left Pending
}
