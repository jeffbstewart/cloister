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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
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

func (s *Server) registerTools() {
	s.mcp.AddTool(&mcp.Tool{
		Name: "current_state",
		Description: "Where the working tree stands: branch, publication state, ahead/behind, " +
			"dirty and untracked files, set-aside parcels.  Read this before any destructive verb.",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}, s.currentState)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "history",
		Description: "Recorded checkpoints, newest first, capped.  Optionally from a revision, scoped to one path, or limited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"ref":   str("start revision (branch, checkpoint id, or HEAD); default HEAD"),
				"path":  str("only checkpoints touching this workspace-relative path"),
				"limit": integer("max entries (default 50, capped)"),
			},
		},
	}, s.history)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "show_change",
		Description: "One checkpoint with its full diff against its parent.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"id": str("checkpoint id")},
			Required:   []string{"id"},
		},
	}, s.showChange)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "file_at",
		Description: "One file's content at a revision, without touching the working tree.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"ref":  str("revision (branch, checkpoint id, or HEAD)"),
				"path": str("workspace-relative file path"),
			},
			Required: []string{"ref", "path"},
		},
	}, s.fileAt)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "pending_changes",
		Description: "The uncommitted delta versus the last checkpoint as a unified diff, plus untracked files — the whole tree or one path.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"path": str("limit to this workspace-relative path")},
		},
	}, s.pendingChanges)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "start_work",
		Description: "Begin a new line of work off the default branch.  Uncommitted changes ride along.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"name": str("branch name, e.g. agent/fix-thing")},
			Required:   []string{"name"},
		},
	}, s.startWork)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "abandon_work",
		Description: "Discard a line of work: switch to the default branch and delete it.  Refuses the default branch and a dirty tree.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"name": str("the doomed branch")},
			Required:   []string{"name"},
		},
	}, s.abandonWork)

	s.mcp.AddTool(&mcp.Tool{
		Name: "checkpoint",
		Description: "Record the working tree — all of it, or just the named paths — as one checkpoint.  " +
			"There is no staging: the tree is what gets recorded.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"message": str("what this checkpoint is (printable ASCII; no attribution trailers)"),
				"paths":   {Type: "array", Items: str("workspace-relative path"), Description: "record only these paths"},
			},
			Required: []string{"message"},
		},
	}, s.checkpoint)

	s.mcp.AddTool(&mcp.Tool{
		Name: "restore",
		Description: "Roll back: one file's local edits (path only), one file from a checkpoint (both), " +
			"the whole tree to a checkpoint on this line of work (checkpoint only), or discard all local edits (neither).  " +
			"Untracked files are never deleted.  Whole-tree restore rewinds history only while that stays a fast-forward for the published branch.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"checkpoint": str("checkpoint id to restore from"),
				"path":       str("workspace-relative file path"),
			},
		},
	}, s.restore)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "set_aside",
		Description: "Park all uncommitted work — tracked edits and untracked files — so the tree matches the last checkpoint.  resume recovers it.",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}, s.setAside)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "resume",
		Description: "Recover the most recently parked parcel.  A conflict leaves markers in the tree and keeps the parcel parked; current_state shows both.",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}, s.resume)

	s.mcp.AddTool(&mcp.Tool{
		Name: "sync_from_upstream",
		Description: "Update the local default branch from its remote and replay the current line of work on it.  " +
			"Requires a clean tree; a conflicted replay is aborted and reported with the conflicting files.",
		InputSchema: &jsonschema.Schema{Type: "object"},
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
	return jsonResult(map[string]any{
		"branch":    st.Branch,
		"default":   st.Default,
		"published": st.Published,
		"ahead":     st.Ahead,
		"behind":    st.Behind,
		"dirty":     dirty,
		"untracked": st.Untracked,
		"set_aside": st.SetAside,
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
	out["diff"] = c.Diff
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
	return jsonResult(map[string]any{"diff": p.Diff, "untracked": p.Untracked}), nil
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
	return textResult(fmt.Sprintf("abandoned %s; on %s", name, s.cfg.Archive.DefaultBranch())), nil
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
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	var cp archive.CheckpointID
	if a.Checkpoint != "" {
		var err error
		if cp, err = archive.ParseCheckpointID(a.Checkpoint); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
	}
	res, err := s.cfg.Archive.Restore(ctx, cp, a.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonResult(map[string]any{"rewound": res.Rewound}), nil
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

func decode(req *mcp.CallToolRequest, v any) error {
	if len(req.Params.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(req.Params.Arguments, v)
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
