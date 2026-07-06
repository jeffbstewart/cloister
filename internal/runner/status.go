package runner

import (
	"io"

	"github.com/jeffbstewart/cloister/internal/cellstate"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// Sink is the state service (statesink) as the runner sees it: the owner of
// durable logs, audit, and status, reached over the network.  When set, the
// builder container holds no /state mount at all, so agent-authored build
// code cannot touch the record of what it did.  Nil means
// local-only (tests, or a builder that still owns /state).
type Sink interface {
	// StartRun opens a streaming sink for one run's live log.
	StartRun(id runid.ID) io.WriteCloser
	// Reupload replaces a run's stored log wholesale — the reconciliation
	// path when live streaming dropped bytes under backpressure.
	Reupload(id runid.ID, log io.Reader) error
	// Finalize seals a run's log as immutable history.
	Finalize(id runid.ID) error
	// PutStatus publishes live queue state.
	PutStatus(st cellstate.Status) error
}

// writeStatus publishes live queue state: to the sink if one is wired, else
// to a local status.json (tests, or a builder that still owns /state).
// Best-effort by design — observability must never fail a build — and
// called outside r.mu so a slow sink never blocks queue-state readers.
func (r *Runner) writeStatus(st cellstate.Status) {
	if r.Sink != nil {
		_ = r.Sink.PutStatus(st)
		return
	}
	if r.StatusPath != "" {
		_ = cellstate.WriteFile(r.StatusPath, st)
	}
}
