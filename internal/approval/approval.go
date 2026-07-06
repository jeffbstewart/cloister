// Package approval is the shared vocabulary for approval-gating: a gated
// mutation held pending a human decision.  The scribe stages the change and
// registers it here; the state service is the pull-only decision authority;
// the status pages set the decision.
package approval

import "github.com/jeffbstewart/cloister/internal/runid"

// Decision is the lifecycle state of a gated op.
type Decision string

const (
	Pending  Decision = "pending"          // awaiting a human
	Approved Decision = "approved"         // human said yes → scribe applies
	Rejected Decision = "rejected"         // human said no → scribe discards
	Timeout  Decision = "rejected_timeout" // no decision within the approval timeout
)

// Resolved reports whether the decision is final (the scribe can stop waiting).
func (d Decision) Resolved() bool { return d != Pending && d != "" }

// Record is the state service's account of one gated op.  The diff itself lives
// in the diff store (keyed by the same opId); this is the small, listable
// metadata + decision the status UI renders and the scribe pulls.
type Record struct {
	OpID      runid.ID `json:"opId"`
	Tool      string   `json:"tool"`
	Path      string   `json:"path"`
	CreatedAt string   `json:"createdAt"` // RFC3339, the state service's clock
	Decision  Decision `json:"decision"`
	DecidedAt string   `json:"decidedAt,omitempty"` // when it left Pending
}
