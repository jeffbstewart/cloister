// Copyright 2026 Jeffrey B. Stewart
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
	// The gate deadline rides the context, alongside the caller's own
	// cancellation: one clock, one select.  We never need to distinguish
	// "gate timed out" from "caller vanished" — both withdraw the pending
	// record and return Timeout.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if ctx.Err() != nil {
			log.Printf("scholar: %s op=%s withdrawn (timeout or caller gone)", tool, id)
			s.withdraw(id) // pull the pending record; nobody is waiting on it
			return approval.Timeout
		}
		rec, err := s.cfg.Approvals.PollDecision(id)
		if err != nil {
			log.Printf("scholar: poll approval %s: %v", id, err)
			s.pause(ctx, 2*time.Second)
			continue
		}
		if rec.Decision.Resolved() {
			log.Printf("scholar: %s op=%s decided: %s", tool, id, rec.Decision)
			return rec.Decision
		}
		s.pause(ctx, 300*time.Millisecond)
	}
}

// pause waits up to d, returning early if ctx is done (caller cancelled or
// the gate deadline passed); the loop's top-of-body ctx check acts on it.
func (s *Server) pause(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (s *Server) withdraw(id runid.ID) {
	if err := s.cfg.Approvals.Withdraw(id); err != nil {
		log.Printf("scholar: withdraw approval %s: %v", id, err)
	}
}
