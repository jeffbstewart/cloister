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

package scribe

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/runid"
	"github.com/jeffbstewart/cloister/internal/workspace"
)

// Approval-gating.  A gated write is staged on the scribe's durable volume,
// registered pending with the state service, and blocks (long-poll) until a
// human decides.  The scribe is outbound-only: it never accepts a decision,
// only pulls one.

// stagedOp is a gated mutation held on disk while it awaits a decision.  It stores
// the full resulting CONTENT (not just the diff), so approval applies exactly
// what was reviewed regardless of any concurrent change, and survives a restart.
type stagedOp struct {
	OpID    runid.ID `json:"opId"`
	Tool    string   `json:"tool"`
	Path    string   `json:"path"` // workspace-relative
	Content []byte   `json:"content"`
	Perm    uint32   `json:"perm"`
	Payload []byte   `json:"payload"` // diff payload for the approval UI / get_diff
}

func (s *Server) stagePath(id runid.ID) string {
	return filepath.Join(s.cfg.StageDir, id.String()+".json")
}

func (s *Server) writeStaged(op stagedOp) error {
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.cfg.StageDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.stagePath(op.OpID), b, 0o600)
}

func (s *Server) removeStaged(id runid.ID) { _ = os.Remove(s.stagePath(id)) }

func (s *Server) listStaged() []stagedOp {
	entries, _ := os.ReadDir(s.cfg.StageDir)
	var ops []stagedOp
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.cfg.StageDir, e.Name()))
		if err != nil {
			continue
		}
		var op stagedOp
		if json.Unmarshal(b, &op) == nil && !op.OpID.IsZero() {
			ops = append(ops, op)
		}
	}
	return ops
}

// awaitApproval stages a gated change, registers it pending, records the pending
// state, then blocks until the decision resolves and applies/discards it.  With
// no approval channel configured it falls back to a flat refusal.
func (s *Server) awaitApproval(rec audit.Record, op stagedOp, notify func(string)) *mcp.CallToolResult {
	if s.cfg.Approvals == nil {
		return s.rejected(rec, decGate, fmt.Errorf("%q is build logic; changes require human approval (not available)", op.Path))
	}
	if err := s.writeStaged(op); err != nil {
		return s.rejected(rec, decError, fmt.Errorf("stage change: %v", err))
	}
	// A stored diff is retrievable for the whole life of the op — the operator
	// reviews it to decide, so every subsequent record (pending, applied, and
	// even rejected/timed-out) truthfully carries HasDiff. rec.Mutation is shared
	// with pend and with resolveStaged's copy, so this one assignment covers them.
	if s.putDiff(op.OpID, op.Payload) {
		rec.Mutation.HasDiff = true
	}
	if err := s.cfg.Approvals.RegisterPending(op.OpID, op.Tool, op.Path); err != nil {
		s.removeStaged(op.OpID)
		return s.rejected(rec, decError, fmt.Errorf("register approval: %v", err))
	}
	pend := rec
	pend.Decision = decPending
	s.audit(pend)

	// Nudge the caller WHILE the call still blocks (MCP progress notification):
	// tell whoever is driving the session to go review it.  Only sent if the
	// client supplied a progress token; otherwise notify is nil.
	if notify != nil {
		notify(fmt.Sprintf("Awaiting approval to write %q — review and approve/reject it on the status page (opId %s). This request is waiting.", op.Path, op.OpID))
	}

	return s.resolveStaged(rec, op, s.awaitDecision(op.OpID))
}

// awaitDecision blocks until the op's decision is final, re-issuing the long-poll.
func (s *Server) awaitDecision(id runid.ID) approval.Decision {
	for {
		r, err := s.cfg.Approvals.PollDecision(id)
		if err != nil {
			log.Printf("scribe: poll approval %s: %v", id, err)
			time.Sleep(2 * time.Second)
			continue
		}
		if r.Decision.Resolved() {
			return r.Decision
		}
	}
}

// resolveStaged applies or discards a staged op per the decision, audits the
// terminal outcome, and clears the staging file.
func (s *Server) resolveStaged(rec audit.Record, op stagedOp, d approval.Decision) *mcp.CallToolResult {
	defer s.removeStaged(op.OpID)
	switch d {
	case approval.Approved:
		if err := s.applyStaged(op); err != nil {
			return s.rejected(rec, decError, fmt.Errorf("apply approved change: %v", err))
		}
		rec.Decision = decApplied
		rec.Mutation.HasDiff = true
		s.audit(rec)
		return jsonResult(map[string]any{"opId": op.OpID, "path": op.Path, "status": "applied_after_approval"})
	case approval.Timeout:
		rec.Decision = decTimeout
		rec.Status = "approval timed out"
		s.audit(rec)
		return errResult("rejected: approval timed out — STOP, do not retry")
	default: // Rejected
		rec.Decision = decRejected
		rec.Status = "rejected by a human"
		s.audit(rec)
		return errResult("rejected: a human declined this change")
	}
}

// applyStaged writes approved content into the workspace, RE-confining the path
// (never trusting the staged string) and creating parents for a create.
func (s *Server) applyStaged(op stagedOp) error {
	p, err := s.root.Resolve(op.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.String()), 0o755); err != nil {
		return err
	}
	return workspace.WriteAtomic(p, op.Content, os.FileMode(op.Perm))
}

// Recover resumes any staged ops a crash/restart left behind: poll each to its
// decision and apply/discard, so an approval granted while the scribe was down
// still lands.  Non-blocking (one goroutine per staged op).
func (s *Server) Recover() {
	if s.cfg.Approvals == nil || s.cfg.StageDir == "" {
		return
	}
	for _, op := range s.listStaged() {
		op := op
		go func() {
			rec := audit.New(op.OpID, op.Tool, "", 0)
			rec.Mutation = &audit.MutationDetail{Path: op.Path}
			s.resolveStaged(rec, op, s.awaitDecision(op.OpID))
		}()
	}
}
