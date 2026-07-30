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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
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

// Config wires the archivist to its engine.
type Config struct {
	Version string
	Archive *archive.Archive
}

// Server owns the archivist's MCP tool surface.
type Server struct {
	cfg Config
	mcp *mcp.Server
	// mu serializes the verbs: the MCP SDK dispatches every tool call in
	// its own goroutine, and one working tree is single-writer — an
	// unserialized checkpoint racing a restore could record a half-reset
	// tree, and even reads during a replay would testify mid-surgery.
	mu sync.Mutex
}

// New builds the archivist tool surface over an opened Archive.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "archivist", Version: cfg.Version}, nil)
	s.registerTools()
	return s
}

// Handler serves MCP at /mcp and a liveness probe at /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcp }, nil))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	return mux
}

func str(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: desc}
}
func integer(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: desc}
}
func boolean(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: desc}
}

// noExtras is JSON Schema's `additionalProperties: false`: unknown
// argument keys are rejected, never ignored.  On a surface with
// destructive verbs a misspelled optional key must be an error — a
// restore call whose only argument was misspelled would otherwise
// decode to the zero shape and act on the whole tree.  decode enforces
// the same rule server-side.
func noExtras() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

// add registers one tool with the worktree lock taken around its
// handler.
func (s *Server) add(tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error)) {
	s.mcp.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return h(ctx, req)
	})
}

func (s *Server) registerTools() {
	s.add(&mcp.Tool{
		Name: "current_state",
		Description: "Where the working tree stands: branch, publication state, ahead/behind, " +
			"dirty and untracked files, set-aside parcels.  Read this before any destructive verb.  " +
			"ahead/behind count against the last-synced remote state and can be stale until sync_from_upstream.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: noExtras()},
	}, s.currentState)

	s.add(&mcp.Tool{
		Name:        "history",
		Description: "Recorded checkpoints, newest first.  Optionally from a revision, scoped to one path, or limited (default 50, max 200).",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"ref":   str("start revision: a branch name, checkpoint id, or HEAD, optionally with ~N/^ suffixes; default HEAD"),
				"path":  str("only checkpoints touching this workspace-relative path"),
				"limit": integer("max entries (default 50, max 200)"),
			},
			AdditionalProperties: noExtras(),
		},
	}, s.history)

	s.add(&mcp.Tool{
		Name:        "show_change",
		Description: "One checkpoint with its full diff against its parent.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"id": str("checkpoint id: 4-64 lowercase hex digits (not HEAD — history lists ids)"),
			},
			Required:             []string{"id"},
			AdditionalProperties: noExtras(),
		},
	}, s.showChange)

	s.add(&mcp.Tool{
		Name:        "file_at",
		Description: "One file's content at a revision, without touching the working tree.  UTF-8 text up to 1 MiB; binary content is refused.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"ref":  str("revision: a branch name, checkpoint id, or HEAD, optionally with ~N/^ suffixes"),
				"path": str("workspace-relative file path"),
			},
			Required:             []string{"ref", "path"},
			AdditionalProperties: noExtras(),
		},
	}, s.fileAt)

	s.add(&mcp.Tool{
		Name:        "pending_changes",
		Description: "The uncommitted delta versus the last checkpoint as a unified diff, plus untracked files — the whole tree or one path.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path": str("limit to this workspace-relative path"),
			},
			AdditionalProperties: noExtras(),
		},
	}, s.pendingChanges)

	s.add(&mcp.Tool{
		Name: "start_work",
		Description: "Begin a NEW line of work off the local default branch (to update that base first, run sync_from_upstream while on the default branch).  " +
			"Uncommitted changes ride along.  The name must not already exist — switch_work returns to an existing line of work.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": str("branch name, e.g. agent/fix-thing: letters, digits, '.', '_', '/', '-', not starting with '.' or '-'"),
			},
			Required:             []string{"name"},
			AdditionalProperties: noExtras(),
		},
	}, s.startWork)

	s.add(&mcp.Tool{
		Name: "switch_work",
		Description: "Return to an EXISTING local line of work (or the default branch).  " +
			"Uncommitted changes ride along when they can be carried cleanly; otherwise the switch is refused — checkpoint or set_aside first.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": str("the existing branch to return to"),
			},
			Required:             []string{"name"},
			AdditionalProperties: noExtras(),
		},
	}, s.switchWork)

	s.add(&mcp.Tool{
		Name: "abandon_work",
		Description: "Discard a line of work: delete the local branch (switching to the default branch first when the doomed branch is checked out).  " +
			"Refuses the default branch and a dirty tree.  Local only — deleting a published counterpart arrives with the remote verbs.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": str("the doomed branch"),
			},
			Required:             []string{"name"},
			AdditionalProperties: noExtras(),
		},
	}, s.abandonWork)

	s.add(&mcp.Tool{
		Name: "checkpoint",
		Description: "Record the working tree — all of it, or just the named paths — as one checkpoint.  " +
			"There is no staging: the tree is what gets recorded.  Refused on the default branch (start_work first), " +
			"on a detached HEAD, and when nothing changed.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"message": str("what this checkpoint is: printable ASCII up to 2000 bytes; attribution trailers (Signed-off-by, Co-authored-by, ...) are refused"),
				"paths":   {Type: "array", Items: str("workspace-relative path"), Description: "record only these paths"},
			},
			Required:             []string{"message"},
			AdditionalProperties: noExtras(),
		},
	}, s.checkpoint)

	s.add(&mcp.Tool{
		Name: "restore",
		Description: "DESTRUCTIVE rollback; discarded edits are unrecoverable (parked work comes back with resume, and set_aside is the recoverable way to clear the tree).  " +
			"Shapes: path only — discard one file's local edits; checkpoint + path — one file's content from that checkpoint; " +
			"checkpoint only — the whole tree to a checkpoint on this line of work (rewinds history only while the published branch stays a fast-forward; " +
			"otherwise the content lands in the tree for the next checkpoint to record); all: true — discard every local edit.  " +
			"Exactly one shape must be chosen; untracked files are never deleted.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"checkpoint": str("checkpoint id: 4-64 lowercase hex digits (history lists ids)"),
				"path":       str("workspace-relative file path"),
				"all":        boolean("true to discard every local edit (must be the only argument)"),
			},
			AdditionalProperties: noExtras(),
		},
	}, s.restore)

	s.add(&mcp.Tool{
		Name: "set_aside",
		Description: "Park all uncommitted work — tracked edits and untracked files — so the tree matches the last checkpoint.  " +
			"Recoverable: resume brings the parcel back (unlike restore, which discards).  Refused when the tree is already clean.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: noExtras()},
	}, s.setAside)

	s.add(&mcp.Tool{
		Name:        "resume",
		Description: "Recover the most recently parked parcel (the counterpart to set_aside).  A conflict leaves markers in the tree and keeps the parcel parked; current_state shows both.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: noExtras()},
	}, s.resume)

	s.add(&mcp.Tool{
		Name: "sync_from_upstream",
		Description: "Update the local default branch from its remote and replay the current line of work on it.  " +
			"Requires a clean tree (untracked files count).  A conflicted replay is aborted — the tree is restored and the answer lists the conflicting files.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: noExtras()},
	}, s.syncFromUpstream)
}

// --- tool handlers ---

func (s *Server) currentState(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	st, err := s.cfg.Archive.CurrentState(ctx)
	if err != nil {
		return errResult(err.Error()), nil
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
	return jsonResult(map[string]any{
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

func (s *Server) history(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Ref   string `json:"ref"`
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	q := archive.HistoryQuery{Path: a.Path, Limit: a.Limit}
	if a.Ref != "" {
		ref, err := archive.ParseRef(a.Ref)
		if err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		q.Ref = ref
	}
	changes, err := s.cfg.Archive.History(ctx, q)
	if err != nil {
		return errResult(err.Error()), nil
	}
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		out = append(out, changeOut(c))
	}
	return jsonResult(map[string]any{"changes": out}), nil
}

func (s *Server) showChange(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	id, err := archive.ParseCheckpointID(a.ID)
	if err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	c, err := s.cfg.Archive.ShowChange(ctx, id)
	if err != nil {
		return errResult(err.Error()), nil
	}
	out := changeOut(c.Change)
	diff, truncated := capDiff(c.Diff)
	out["diff"] = diff
	if truncated {
		out["diff_truncated"] = true
	}
	return jsonResult(out), nil
}

func (s *Server) fileAt(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Ref  string `json:"ref"`
		Path string `json:"path"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	ref, err := archive.ParseRef(a.Ref)
	if err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	content, err := s.cfg.Archive.FileAt(ctx, ref, a.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Refusals, not silent degradation: JSON transport would replace
	// invalid bytes with U+FFFD, handing back corrupted content under a
	// success status; and a truncated file presented as the file is the
	// same lie one cap over.
	if !utf8.Valid(content) {
		return errResult(fmt.Sprintf("file_at: %s at %s is not UTF-8 text (%d bytes); binary content is not served", a.Path, a.Ref, len(content))), nil
	}
	if len(content) > MaxFileBytes {
		return errResult(fmt.Sprintf("file_at: %s at %s is %d bytes, over the %d-byte cap", a.Path, a.Ref, len(content), MaxFileBytes)), nil
	}
	return textResult(string(content)), nil
}

func (s *Server) pendingChanges(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	p, err := s.cfg.Archive.PendingChanges(ctx, a.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	diff, diffTruncated := capDiff(p.Diff)
	untracked, total := capUntracked(p.Untracked)
	out := map[string]any{"diff": diff, "untracked": untracked, "untracked_total": total}
	if diffTruncated {
		out["diff_truncated"] = true
	}
	return jsonResult(out), nil
}

func (s *Server) startWork(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	name, err := archive.ParseBranchName(a.Name)
	if err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if err := s.cfg.Archive.StartWork(ctx, name); err != nil {
		return errResult(err.Error()), nil
	}
	return textResult(fmt.Sprintf("working on %s (off %s)", name, s.cfg.Archive.DefaultBranch())), nil
}

func (s *Server) switchWork(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	name, err := archive.ParseBranchName(a.Name)
	if err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if err := s.cfg.Archive.SwitchWork(ctx, name); err != nil {
		return errResult(err.Error()), nil
	}
	return textResult(fmt.Sprintf("working on %s", name)), nil
}

func (s *Server) abandonWork(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	name, err := archive.ParseBranchName(a.Name)
	if err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if err := s.cfg.Archive.AbandonWork(ctx, name); err != nil {
		return errResult(err.Error()), nil
	}
	// Report the branch actually checked out: the engine only switches
	// to the default branch when the doomed branch was current, and a
	// caller told "on main" while on another line of work would record
	// its next checkpoint in the wrong place.
	if st, err := s.cfg.Archive.CurrentState(ctx); err == nil {
		return textResult(fmt.Sprintf("abandoned %s; on %s", name, st.Branch)), nil
	}
	return textResult(fmt.Sprintf("abandoned %s", name)), nil
}

func (s *Server) checkpoint(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	id, err := s.cfg.Archive.Checkpoint(ctx, a.Message, a.Paths)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonResult(map[string]any{"checkpoint": id.String()}), nil
}

func (s *Server) restore(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Checkpoint string `json:"checkpoint"`
		Path       string `json:"path"`
		All        bool   `json:"all"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	// The discard-everything shape must be asked for by name.  Without
	// this, an empty call would be the most destructive one — and with
	// it, "all" alongside a narrower target is a contradiction to refuse,
	// not to guess about.
	if a.All && (a.Checkpoint != "" || a.Path != "") {
		return errResult("bad arguments: all: true discards every local edit and takes no other argument"), nil
	}
	if !a.All && a.Checkpoint == "" && a.Path == "" {
		return errResult("bad arguments: restore needs a target — a path, a checkpoint, or all: true to discard every local edit (set_aside parks work recoverably instead)"), nil
	}
	var cp archive.CheckpointID
	if a.Checkpoint != "" {
		parsed, err := archive.ParseCheckpointID(a.Checkpoint)
		if err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		cp = parsed
	}
	res, err := s.cfg.Archive.Restore(ctx, cp, a.Path)
	if err != nil {
		return errResult(err.Error()), nil
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
	return jsonResult(out), nil
}

func (s *Server) setAside(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := s.cfg.Archive.SetAside(ctx); err != nil {
		return errResult(err.Error()), nil
	}
	return textResult("set aside; the tree matches the last checkpoint"), nil
}

func (s *Server) resume(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := s.cfg.Archive.Resume(ctx); err != nil {
		return errResult(err.Error()), nil
	}
	return textResult("resumed the most recent parcel"), nil
}

func (s *Server) syncFromUpstream(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := s.cfg.Archive.SyncFromUpstream(ctx)
	if err != nil {
		// A conflict is a structured answer, not prose: the caller needs
		// the file list as data and — critically — needs to know the
		// replay was aborted and the tree restored, so it does not
		// attempt "recovery" on a tree that needs none.  Other failures
		// (including the abort-failed, needs-manual-recovery path) stay
		// plain error text.
		var conflict *archive.ConflictError
		if errors.As(err, &conflict) {
			r := jsonResult(map[string]any{
				"conflict":      true,
				"files":         conflict.Files,
				"aborted":       true,
				"tree_restored": true,
			})
			r.IsError = true
			return r, nil
		}
		return errResult(err.Error()), nil
	}
	return jsonResult(map[string]any{"replayed": res.Replayed, "merged": res.Merged}), nil
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

// decode is strict, unlike the read-side workers' copies: unknown keys
// are rejected, because on this surface a misspelled optional argument
// must never quietly select a different — possibly destructive — shape.
func decode(req *mcp.CallToolRequest, v any) error {
	if len(req.Params.Arguments) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errResult(msg string) *mcp.CallToolResult {
	r := textResult(msg)
	r.IsError = true
	return r
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult(fmt.Sprintf("internal: marshal result: %v", err))
	}
	return textResult(string(b))
}
