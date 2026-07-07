package scholar

import (
	"context"
	"log"
	"time"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// ApprovalClient is the scholar's pull-only view of the state service's approval
// store: register a pending op, long-poll its decision, and
// withdraw it if the caller vanishes or the gate times out.  The statesink client
// satisfies it; tests substitute a stub.  The scholar never SETS a decision —
// that is the operator's, via the status UI (one-way glass).
type ApprovalClient interface {
	RegisterPending(opID runid.ID, tool, path string) error
	PollDecision(opID runid.ID) (approval.Record, error)
	Withdraw(opID runid.ID) error
}

// gate registers a pending approval and blocks until the operator decides, the
// gate's own timeout elapses, or the caller's context is cancelled.  On timeout
// or cancellation it WITHDRAWS the pending record, so the operator is never
// left with an actionable request nobody is waiting on.  It returns the terminal
// decision (Approved / Rejected / Timeout).
//
// A nil ApprovalClient means approvals are unavailable → fail-closed (Rejected):
// the scholar never runs an ungated query or reads an unapproved raw URL just
// because the state service is missing.
func (s *Server) gate(ctx context.Context, tool, path string, timeout time.Duration) approval.Decision {
	if s.cfg.Approvals == nil {
		return approval.Rejected
	}
	id, err := runid.New()
	if err != nil {
		log.Printf("scholar: mint approval id: %v", err)
		return approval.Rejected // fail closed, like every other gate failure
	}
	if err := s.cfg.Approvals.RegisterPending(id, tool, path); err != nil {
		log.Printf("scholar: register approval: %v", err)
		return approval.Rejected
	}
	log.Printf("scholar: %s op=%s awaiting operator decision at /approvals (timeout %s)", tool, id, timeout)
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			log.Printf("scholar: %s op=%s withdrawn (timeout or caller gone)", tool, id)
			s.withdraw(id) // caller gone, or our timeout — pull the pending record
			return approval.Timeout
		}
		rec, err := s.cfg.Approvals.PollDecision(id)
		if err != nil {
			log.Printf("scholar: poll approval %s: %v", id, err)
			if !s.pause(ctx, deadline, 2*time.Second) {
				s.withdraw(id)
				return approval.Timeout
			}
			continue
		}
		if rec.Decision.Resolved() {
			log.Printf("scholar: %s op=%s decided: %s", tool, id, rec.Decision)
			return rec.Decision
		}
		if !s.pause(ctx, deadline, 300*time.Millisecond) {
			s.withdraw(id)
			return approval.Timeout
		}
	}
}

// pause waits up to d (clamped to the deadline), returning true to keep polling
// or false if the context is done or the deadline has passed.
func (s *Server) pause(ctx context.Context, deadline time.Time, d time.Duration) bool {
	if rem := time.Until(deadline); d > rem {
		d = rem
	}
	if d <= 0 {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (s *Server) withdraw(id runid.ID) {
	if err := s.cfg.Approvals.Withdraw(id); err != nil {
		log.Printf("scholar: withdraw approval %s: %v", id, err)
	}
}
