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

// Package archivist is the version-control worker's MCP surface
// (docs/archivist.md): the VCS-agnostic local verbs of internal/archive
// served as tools.  The contract speaks in checkpoints and lines of
// work, never git incantations; the remote verbs (publish, propose,
// reviews) and the audit wiring arrive with the git jail — the local
// verbs here are working-tree mechanics, deliberately unaudited.
package archivist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/forge"
	"github.com/jeffbstewart/cloister/internal/mcpserve"
)

// Result caps: the consumer is a model context, not a pager, so every
// content-bearing answer is bounded and truncation is reported, never
// silent (the same rationale as history's own 200-entry cap).
const (
	// MaxFileBytes bounds file_at.  Oversize is refused rather than
	// truncated: partial file content presented as the file would be
	// corruption with a success status.
	MaxFileBytes = 1 << 20
	// MaxDiffBytes bounds the diffs in show_change and pending_changes;
	// past it the diff is cut with an explicit marker.
	MaxDiffBytes = 1 << 20
	// MaxUntracked bounds the untracked listings in current_state and
	// pending_changes (an unignored node_modules/ would otherwise
	// enumerate every file); untracked_total always carries the real
	// count.
	MaxUntracked = 500
)

// Auditor records the archivist's remote operations.  *sink.Client
// satisfies it.
type Auditor interface {
	Append(audit.Record) error
}

// Config wires the archivist to its grange (the workspace lifecycle owner,
// which hands out the live Archive and forge client) and its audit sink.
type Config struct {
	Version string
	// Grange owns the workspace: provision brings a checkout into being,
	// dispose returns it to empty, and every other verb operates on the
	// live Archive it hands out — refusing cleanly until one is
	// provisioned.
	Grange *archive.Grange
	// Audit records remote and lifecycle operations; nil disables (tests).
	// Working-tree verbs are unaudited by design.
	Audit Auditor
	// Draining is closed when the process begins a lame-duck shutdown.
	// await_review blocks for up to an hour, and a graceful shutdown
	// WAITS for handlers rather than cancelling them, so without this a
	// restart would hold the drain open until docker's SIGKILL.  nil
	// (tests) is a channel that never closes — no drain, no early
	// return.
	Draining <-chan struct{}
	// Now and Sleep are await_review's clock: the deadline arithmetic and
	// the spacing between endpoint polls.  nil means the real time.Now and
	// a context-aware sleep; tests pin both (never real sleeps).
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Server owns the archivist's MCP tool surface.
type Server struct {
	cfg Config
	mcp *mcp.Server
	// mu serializes every verb, provision and dispose included: the MCP
	// SDK dispatches each tool call in its own goroutine, and both the one
	// working tree (single-writer) and the grange's live-workspace pointer
	// are shared state.  Without it a checkpoint could race a dispose's
	// teardown (use-after-free on the Archive), two provisions could both
	// clone onto the same empty workspace, and a read during a replay
	// would testify mid-surgery.  Every handler runs under this lock
	// because add — the sole registration path — takes it.  The one
	// exception is await_review, whose wait spans minutes: it takes the
	// lock only around its target resolution (see registerAwaitReview).
	mu sync.Mutex
}

// New builds the archivist tool surface over a grange.  Every verb is
// registered; the working-tree and remote verbs refuse until a workspace
// is provisioned, so an unprovisioned instance has a full surface that
// simply says "provision first".
func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "archivist", Version: cfg.Version}, nil)
	s.registerLifecycleTools()
	s.registerTools()
	s.registerRemoteTools()
	return s
}

// Handler serves MCP at /mcp and a liveness probe at /healthz.
func (s *Server) Handler() http.Handler {
	return mcpserve.Handler(s.mcp)
}

// add registers one tool with the serialization lock taken around its
// handler.  Every verb but await_review registers through here, so
// holding the lock here is what makes "every verb is serialized" true by
// construction; await_review manages the lock itself around its target
// resolution (see registerAwaitReview for why).
func (s *Server) add(tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error)) {
	s.mcp.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return h(ctx, req)
	})
}

// addArc registers a verb that operates on the live workspace: the lock is
// taken, the provisioned Archive resolved, and a clean refusal returned
// when none is provisioned — so no verb body repeats that guard, and every
// working-tree verb uniformly says "provision first" until there is one.
func (s *Server) addArc(tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest, *archive.Archive) (*mcp.CallToolResult, error)) {
	s.add(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arc, err := s.cfg.Grange.Archive()
		if err != nil {
			return mcpserve.ErrResult(err.Error()), nil
		}
		return h(ctx, req, arc)
	})
}

// addForge registers a verb that also needs the endpoint's PR-verb client,
// resolving both under the lock; it refuses when the workspace is
// unprovisioned or its endpoint has no forge adapter.
func (s *Server) addForge(tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest, *archive.Archive, forge.Client) (*mcp.CallToolResult, error)) {
	s.addArc(tool, func(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
		fc, err := s.cfg.Grange.Forge()
		if err != nil {
			return mcpserve.ErrResult(err.Error()), nil
		}
		return h(ctx, req, arc, fc)
	})
}

func (s *Server) registerTools() {
	// NOT addArc: this is the one verb whose job is to report where things
	// stand, so "there is no workspace" is an ANSWER, not a failure.  An
	// agent orienting itself asks this first; handing it a tool error for
	// the normal pre-provision state teaches it that something broke.
	s.add(&mcp.Tool{
		Name: "current_state",
		Description: "Where things stand: whether a workspace is provisioned, and if so its branch, publication state, " +
			"ahead/behind, dirty and untracked files, and set-aside parcels.  Read this before any destructive verb, " +
			"and first in a session to orient.  ahead/behind count against the last-synced remote state and can be " +
			"stale until sync_from_upstream.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: mcpserve.NoExtras()},
	}, s.currentState)

	s.addArc(&mcp.Tool{
		Name:        "history",
		Description: "Recorded checkpoints, newest first.  Optionally from a revision, scoped to one path, or limited (default 50, max 200).",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"ref":   mcpserve.Str("start revision: a branch name, checkpoint id, or HEAD, optionally with ~N/^ suffixes; default HEAD"),
				"path":  mcpserve.Str("only checkpoints touching this workspace-relative path"),
				"limit": mcpserve.Integer("max entries (default 50, max 200)"),
			},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.history)

	s.addArc(&mcp.Tool{
		Name:        "show_change",
		Description: "One checkpoint with its full diff against its parent.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"id": mcpserve.Str("checkpoint id: 4-64 lowercase hex digits (not HEAD — history lists ids)"),
			},
			Required:             []string{"id"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.showChange)

	s.addArc(&mcp.Tool{
		Name:        "file_at",
		Description: "One file's content at a revision, without touching the working tree.  UTF-8 text up to 1 MiB; binary content is refused.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"ref":  mcpserve.Str("revision: a branch name, checkpoint id, or HEAD, optionally with ~N/^ suffixes"),
				"path": mcpserve.Str("workspace-relative file path"),
			},
			Required:             []string{"ref", "path"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.fileAt)

	s.addArc(&mcp.Tool{
		Name:        "pending_changes",
		Description: "The uncommitted delta versus the last checkpoint as a unified diff, plus untracked files — the whole tree or one path.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path": mcpserve.Str("limit to this workspace-relative path"),
			},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.pendingChanges)

	s.addArc(&mcp.Tool{
		Name: "start_work",
		Description: "Begin a NEW line of work off the local default branch (to update that base first, run sync_from_upstream while on the default branch).  " +
			"The name MUST be in the repository's agent namespace — `agent/<something>`; the forge refuses to create any other branch, " +
			"so a wrong name would only surface at publish, after the work is committed to it.  " +
			"Uncommitted changes ride along.  The name must not already exist — switch_work returns to an existing line of work.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": mcpserve.Str("branch name in the agent namespace, e.g. agent/fix-thing: letters, digits, '.', '_', '/', '-', not starting with '.' or '-'"),
			},
			Required:             []string{"name"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.startWork)

	s.addArc(&mcp.Tool{
		Name: "switch_work",
		Description: "Return to an EXISTING local line of work (or the default branch).  " +
			"Uncommitted changes ride along when they can be carried cleanly; otherwise the switch is refused — checkpoint or set_aside first.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": mcpserve.Str("the existing branch to return to"),
			},
			Required:             []string{"name"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.switchWork)

	s.addArc(&mcp.Tool{
		Name: "abandon_work",
		Description: "Discard a line of work: delete the local branch (switching to the default branch first when the doomed branch is checked out).  " +
			"Refuses the default branch and a dirty tree.  deleteRemote also removes the published counterpart; a branch never published has none, and that is not an error.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name":         mcpserve.Str("the doomed branch"),
				"deleteRemote": mcpserve.Boolean("also delete the published counterpart (an audited remote operation)"),
			},
			Required:             []string{"name"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.abandonWork)

	s.addArc(&mcp.Tool{
		Name: "checkpoint",
		Description: "Record the working tree — all of it, or just the named paths — as one checkpoint.  " +
			"There is no staging: the tree is what gets recorded.  Refused on the default branch (start_work first), " +
			"on a detached HEAD, and when nothing changed.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"message": mcpserve.Str("what this checkpoint is: printable ASCII up to 2000 bytes; attribution trailers (Signed-off-by, Co-authored-by, ...) are refused"),
				"paths":   {Type: "array", Items: mcpserve.Str("workspace-relative path"), Description: "record only these paths"},
			},
			Required:             []string{"message"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.checkpoint)

	s.addArc(&mcp.Tool{
		Name: "restore",
		Description: "DESTRUCTIVE rollback; discarded edits are unrecoverable (parked work comes back with resume, and set_aside is the recoverable way to clear the tree).  " +
			"Shapes: path only — discard one file's local edits; checkpoint + path — one file's content from that checkpoint; " +
			"checkpoint only — the whole tree to a checkpoint on this line of work (rewinds history only while the published branch stays a fast-forward; " +
			"otherwise the content lands in the tree for the next checkpoint to record); all: true — discard every local edit.  " +
			"Exactly one shape must be chosen; untracked files are never deleted.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"checkpoint": mcpserve.Str("checkpoint id: 4-64 lowercase hex digits (history lists ids)"),
				"path":       mcpserve.Str("workspace-relative file path"),
				"all":        mcpserve.Boolean("true to discard every local edit (must be the only argument)"),
			},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.restore)

	s.addArc(&mcp.Tool{
		Name: "set_aside",
		Description: "Park all uncommitted work — tracked edits and untracked files — so the tree matches the last checkpoint.  " +
			"Recoverable: resume brings the parcel back (unlike restore, which discards).  Refused when the tree is already clean.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: mcpserve.NoExtras()},
	}, s.setAside)

	s.addArc(&mcp.Tool{
		Name:        "resume",
		Description: "Recover the most recently parked parcel (the counterpart to set_aside).  A conflict leaves markers in the tree and keeps the parcel parked; current_state shows both.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: mcpserve.NoExtras()},
	}, s.resume)

	s.addArc(&mcp.Tool{
		Name: "sync_from_upstream",
		Description: "Update the local default branch from its remote and replay the current line of work on it.  " +
			"Requires a clean tree (untracked files count).  A conflicted replay is aborted — the tree is restored and the answer lists the conflicting files.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: mcpserve.NoExtras()},
	}, s.syncFromUpstream)
}

// --- tool handlers ---

func (s *Server) currentState(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	arc, err := s.cfg.Grange.Archive()
	if err != nil {
		// No workspace is a STATE, reported as a successful answer that
		// names the next step.  A CORRUPT workspace is different: it needs
		// host-side recovery, so it stays an error.
		if errors.Is(err, archive.ErrNotProvisioned) {
			return mcpserve.JSONResult(map[string]any{
				"provisioned": false,
				"next":        "provision(repo, branch?) brings a workspace into being; every other verb refuses until then",
			}), nil
		}
		return mcpserve.ErrResult(err.Error()), nil
	}
	st, err := arc.CurrentState(ctx)
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	dirty := make([]map[string]any, 0, len(st.Dirty))
	for _, f := range st.Dirty {
		d := map[string]any{"path": f.Path, "status": f.Status}
		if f.From != "" {
			d["from"] = f.From
		}
		dirty = append(dirty, d)
	}
	untracked, total := capUntracked(st.Untracked)
	return mcpserve.JSONResult(map[string]any{
		"provisioned":     true,
		"branch":          st.Branch,
		"default":         st.Default,
		"published":       st.Published,
		"ahead":           st.Ahead,
		"behind":          st.Behind,
		"dirty":           dirty,
		"untracked":       untracked,
		"untracked_total": total,
		"set_aside":       st.SetAside,
	}), nil
}

func (s *Server) history(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		Ref   string `json:"ref"`
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	q := archive.HistoryQuery{Path: a.Path, Limit: a.Limit}
	if a.Ref != "" {
		ref, err := archive.ParseRef(a.Ref)
		if err != nil {
			return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
		}
		q.Ref = ref
	}
	changes, err := arc.History(ctx, q)
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		out = append(out, changeOut(c))
	}
	return mcpserve.JSONResult(map[string]any{"changes": out}), nil
}

func (s *Server) showChange(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	id, err := archive.ParseCheckpointID(a.ID)
	if err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	c, err := arc.ShowChange(ctx, id)
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	out := changeOut(c.Change)
	diff, truncated := capDiff(c.Diff)
	out["diff"] = diff
	if truncated {
		out["diff_truncated"] = true
	}
	return mcpserve.JSONResult(out), nil
}

func (s *Server) fileAt(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		Ref  string `json:"ref"`
		Path string `json:"path"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	ref, err := archive.ParseRef(a.Ref)
	if err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	content, err := arc.FileAt(ctx, ref, a.Path)
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	// Refusals, not silent degradation: JSON transport would replace
	// invalid bytes with U+FFFD, handing back corrupted content under a
	// success status; and a truncated file presented as the file is the
	// same lie one cap over.
	if !utf8.Valid(content) {
		return mcpserve.ErrResult(fmt.Sprintf("file_at: %s at %s is not UTF-8 text (%d bytes); binary content is not served", a.Path, a.Ref, len(content))), nil
	}
	if len(content) > MaxFileBytes {
		return mcpserve.ErrResult(fmt.Sprintf("file_at: %s at %s is %d bytes, over the %d-byte cap", a.Path, a.Ref, len(content), MaxFileBytes)), nil
	}
	return mcpserve.TextResult(string(content)), nil
}

func (s *Server) pendingChanges(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	p, err := arc.PendingChanges(ctx, a.Path)
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	diff, diffTruncated := capDiff(p.Diff)
	untracked, total := capUntracked(p.Untracked)
	out := map[string]any{"diff": diff, "untracked": untracked, "untracked_total": total}
	if diffTruncated {
		out["diff_truncated"] = true
	}
	return mcpserve.JSONResult(out), nil
}

func (s *Server) startWork(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	name, err := archive.ParseBranchName(a.Name)
	if err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	if err := arc.StartWork(ctx, name); err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.TextResult(fmt.Sprintf("working on %s (off %s)", name, arc.DefaultBranch())), nil
}

func (s *Server) switchWork(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	name, err := archive.ParseBranchName(a.Name)
	if err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	if err := arc.SwitchWork(ctx, name); err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.TextResult(fmt.Sprintf("working on %s", name)), nil
}

func (s *Server) abandonWork(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		Name         string `json:"name"`
		DeleteRemote bool   `json:"deleteRemote"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	name, err := archive.ParseBranchName(a.Name)
	if err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	// The remote half runs first, while the local branch (and its
	// upstream bookkeeping) still exists, and is audited ONLY when it
	// actually touched the endpoint — a never-published branch, or a
	// refusal before any endpoint touch, leaves no phantom remote record.
	// It must not fire when the local abandon would then refuse (a dirty
	// tree, the default branch), so the local guards are checked first;
	// handlers are serialized, so nothing changes between check and act.
	if a.DeleteRemote {
		if err := arc.CanAbandon(ctx, name); err != nil {
			return mcpserve.ErrResult(err.Error()), nil
		}
		deleted, derr := arc.DeleteRemoteBranch(ctx, name)
		if deleted || derr != nil {
			s.auditRemote("abandon_remote", audit.RemoteDetail{Branch: name.String()}, remoteDecision(derr))
		}
		if derr != nil {
			return mcpserve.ErrResult(derr.Error()), nil
		}
	}
	if err := arc.AbandonWork(ctx, name); err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	// Report the branch actually checked out: the engine only switches
	// to the default branch when the doomed branch was current, and a
	// caller told "on main" while on another line of work would record
	// its next checkpoint in the wrong place.
	if st, err := arc.CurrentState(ctx); err == nil {
		return mcpserve.TextResult(fmt.Sprintf("abandoned %s; on %s", name, st.Branch)), nil
	}
	return mcpserve.TextResult(fmt.Sprintf("abandoned %s", name)), nil
}

func (s *Server) checkpoint(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	id, err := arc.Checkpoint(ctx, a.Message, a.Paths)
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.JSONResult(map[string]any{"checkpoint": id.String()}), nil
}

func (s *Server) restore(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	var a struct {
		Checkpoint string `json:"checkpoint"`
		Path       string `json:"path"`
		All        bool   `json:"all"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	// The discard-everything shape must be asked for by name.  Without
	// this, an empty call would be the most destructive one — and with
	// it, "all" alongside a narrower target is a contradiction to refuse,
	// not to guess about.
	if a.All && (a.Checkpoint != "" || a.Path != "") {
		return mcpserve.ErrResult("bad arguments: all: true discards every local edit and takes no other argument"), nil
	}
	if !a.All && a.Checkpoint == "" && a.Path == "" {
		return mcpserve.ErrResult("bad arguments: restore needs a target — a path, a checkpoint, or all: true to discard every local edit (set_aside parks work recoverably instead)"), nil
	}
	var cp archive.CheckpointID
	if a.Checkpoint != "" {
		parsed, err := archive.ParseCheckpointID(a.Checkpoint)
		if err != nil {
			return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
		}
		cp = parsed
	}
	res, err := arc.Restore(ctx, cp, a.Path)
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	// The same request realizes differently by shape and publication
	// state; name what actually happened rather than a bare bool.
	out := map[string]any{}
	switch {
	case a.All:
		out["action"] = "discarded_local_edits"
	case a.Path != "":
		out["action"] = "file_restored"
	case res.Rewound:
		out["action"] = "tree_rewound"
	default:
		out["action"] = "content_restored"
		out["note"] = "the tree now holds the checkpoint's content on unchanged history; run checkpoint to record it"
	}
	return mcpserve.JSONResult(out), nil
}

func (s *Server) setAside(ctx context.Context, _ *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	if err := arc.SetAside(ctx); err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.TextResult("set aside; the tree matches the last checkpoint"), nil
}

func (s *Server) resume(ctx context.Context, _ *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	if err := arc.Resume(ctx); err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.TextResult("resumed the most recent parcel"), nil
}

func (s *Server) syncFromUpstream(ctx context.Context, _ *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	res, err := arc.SyncFromUpstream(ctx)
	if err != nil {
		// A conflict is a structured answer, not prose: the caller needs
		// the file list as data and — critically — needs to know the
		// replay was aborted and the tree restored, so it does not
		// attempt "recovery" on a tree that needs none.  Other failures
		// (including the abort-failed, needs-manual-recovery path) stay
		// plain error text.
		var conflict *archive.ConflictError
		if errors.As(err, &conflict) {
			r := mcpserve.JSONResult(map[string]any{
				"conflict":      true,
				"files":         conflict.Files,
				"aborted":       true,
				"tree_restored": true,
			})
			r.IsError = true
			return r, nil
		}
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.JSONResult(map[string]any{"replayed": res.Replayed, "merged": res.Merged}), nil
}

// --- helpers ---

// changeOut renders one Change; the time is RFC3339 UTC, the archivist's
// testimony from the injected clock.
func changeOut(c archive.Change) map[string]any {
	return map[string]any{
		"id":      c.ID.String(),
		"time":    c.Time.UTC().Format(time.RFC3339),
		"author":  c.Author,
		"email":   c.Email,
		"subject": c.Subject,
	}
}

// capDiff bounds a diff at MaxDiffBytes with an explicit marker.
func capDiff(diff string) (string, bool) {
	if len(diff) <= MaxDiffBytes {
		return diff, false
	}
	return fmt.Sprintf("%s\n[diff truncated: showing %d of %d bytes]", diff[:MaxDiffBytes], MaxDiffBytes, len(diff)), true
}

// capUntracked bounds an untracked listing at MaxUntracked entries and
// normalizes nil to an empty slice, so a clean tree answers [] rather
// than null.
func capUntracked(untracked []string) ([]string, int) {
	total := len(untracked)
	if untracked == nil {
		return []string{}, 0
	}
	if total > MaxUntracked {
		return untracked[:MaxUntracked], total
	}
	return untracked, total
}
