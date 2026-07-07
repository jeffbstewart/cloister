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

// Package scribe is the workspace editor: the sole audited writer of the
// project workspace.  It serves MCP write tools over streamable HTTP; every
// op is confined via internal/workspace (no symlinks, no escapes) and audited
// to the state service, with the resulting diff stored for review (get_diff /
// the status pages).  Ops: create_text_file, apply_diff, replace_string,
// replace_regex, write_binary_file, create_directory, move_file,
// move_directory, copy_file, delete_file, delete_directory, and the read-only
// list_workspace_roots / get_diff.  The approval-required set — build-logic
// writes, write_binary_file, and permit_non_utf8 edits — is refused outright
// when no approval channel is wired, and held PENDING human approval when
// one is.
package scribe

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/runid"
	"github.com/jeffbstewart/cloister/internal/workspace"
)

// DefaultMaxDiffBytes caps a stored diff payload; oversized payloads are
// truncated + flagged (git is the version store, not the audit).
const DefaultMaxDiffBytes = 1 << 20

// Audit decision codes for scribe ops.
const (
	decApplied  audit.Decision = "applied"
	decDryRun   audit.Decision = "dry_run"
	decNoChange audit.Decision = "no_change"
	decConfine  audit.Decision = "rejected_confinement"
	decOverflow audit.Decision = "rejected_overflow"
	decGate     audit.Decision = "rejected_gate"
	decPattern  audit.Decision = "rejected_pattern"
	decError    audit.Decision = "error"
	// Approval lifecycle.
	decPending  audit.Decision = "pending_approval"
	decRejected audit.Decision = "rejected"
	decTimeout  audit.Decision = "rejected_timeout"
)

// Auditor records one audit line per mutation.  *sink.Client satisfies it,
// so the scribe sends audit over the same sink the builder uses.
type Auditor interface {
	Append(audit.Record) error
}

// DiffStore holds per-op diff payloads for review, keyed by opId.
// *sink.Client satisfies it.
type DiffStore interface {
	PutDiff(id runid.ID, payload []byte) error
	FetchDiff(id runid.ID) ([]byte, error)
}

// ApprovalClient is the scribe's outbound view of the approval authority.
// *sink.Client satisfies it.  nil → gated writes are refused outright
// instead of held pending.
type ApprovalClient interface {
	RegisterPending(id runid.ID, tool, path string) error
	PollDecision(id runid.ID) (approval.Record, error)
}

// Config wires the scribe to its workspace, audit sink, and diff store.
type Config struct {
	Version   string
	Root      *workspace.Root // the sole writable tree
	Audit     Auditor         // nil disables auditing (tests may set a fake)
	Diffs     DiffStore       // nil disables diff payload storage (still audits)
	Approvals ApprovalClient  // nil → gated writes refused; set → held pending
	StageDir  string          // durable staging for pending changes (required with Approvals)
}

// Server owns the scribe's MCP tool surface.
type Server struct {
	cfg  Config
	mcp  *mcp.Server
	root *workspace.Root
}

// New builds the scribe tool surface.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, root: cfg.Root}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "scribe", Version: cfg.Version}, nil)
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
func boolean(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: desc}
}
func integer(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: desc}
}

func (s *Server) registerTools() {
	s.mcp.AddTool(&mcp.Tool{
		Name:        "create_text_file",
		Description: "Create a NEW UTF-8 text file with the given content. Fails if the file already exists — use apply_diff or replace_* to edit an existing file. Parent directories are created as needed.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":    str("workspace-relative path of the new file"),
				"content": str("full UTF-8 file content"),
				"dryRun":  boolean("if true, validate and report but do not write"),
			},
			Required: []string{"path", "content"},
		},
	}, s.createTextFile)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "apply_diff",
		Description: "Apply a unified-style diff to an EXISTING file. Hunks are located by their surrounding CONTEXT, not line numbers (the @@ numbers are ignored), so include enough context to make each change unambiguous. One file per call; UTF-8 in and out. Use create_text_file for new files.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":   str("workspace-relative path of the existing file"),
				"diff":   str("the diff to apply — hunks of space (context), - (remove), + (add) lines"),
				"dryRun": boolean("if true, return the resulting diff without writing"),
			},
			Required: []string{"path", "diff"},
		},
	}, s.applyDiff)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "replace_string",
		Description: "Replace a literal substring in an existing file. scope is 'first' (default) or 'all'; expectedCount asserts how many times `find` occurs (guards runaway edits). UTF-8 in and out.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":          str("workspace-relative file path"),
				"find":          str("literal substring to find (must be non-empty)"),
				"replace":       str("replacement text"),
				"scope":         str("'first' (default) or 'all'"),
				"expectedCount": integer("assert `find` occurs exactly this many times, else fail"),
				"dryRun":        boolean("if true, return the diff without writing"),
			},
			Required: []string{"path", "find", "replace"},
		},
	}, s.replaceString)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "replace_regex",
		Description: "Replace matches of an RE2 regular expression in an existing file, with $1 capture-group references in the replacement. scope is 'first' (default) or 'all'; expectedCount asserts the match count. UTF-8 in and out.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":          str("workspace-relative file path"),
				"pattern":       str("RE2 regular expression"),
				"replacement":   str("replacement (supports $1, ${name} capture refs)"),
				"scope":         str("'first' (default) or 'all'"),
				"expectedCount": integer("assert this many matches, else fail"),
				"dryRun":        boolean("if true, return the diff without writing"),
			},
			Required: []string{"path", "pattern", "replacement"},
		},
	}, s.replaceRegex)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "write_binary_file",
		Description: "Replace a whole file with opaque (base64-encoded) bytes. ALWAYS requires human approval — binary content can't be reviewed as a diff. Use the text ops for source; this is for binary assets.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":        str("workspace-relative file path"),
				"bytesBase64": str("file content, base64-encoded"),
				"overwrite":   boolean("if true, replace an existing file"),
			},
			Required: []string{"path", "bytesBase64"},
		},
	}, s.writeBinaryFile)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "create_directory",
		Description: "Create a directory (and any missing parents) within the workspace.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"path": str("workspace-relative directory path")},
			Required:   []string{"path"},
		},
	}, s.createDirectory)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "move_file",
		Description: "Rename or move a file within the workspace. Both ends are confined.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"from":      str("source file path"),
				"to":        str("destination path"),
				"overwrite": boolean("if true, replace an existing destination file"),
			},
			Required: []string{"from", "to"},
		},
	}, s.moveFile)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "move_directory",
		Description: "Rename or move a directory subtree within the workspace. The destination must not already exist.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"from": str("source directory path"),
				"to":   str("destination path"),
			},
			Required: []string{"from", "to"},
		},
	}, s.moveDirectory)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "copy_file",
		Description: "Copy a file within the workspace, preserving its contents (so the agent need not read+rewrite it). Both ends are confined; the destination must not exist unless overwrite is set.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"from":      str("source file path"),
				"to":        str("destination path"),
				"overwrite": boolean("if true, replace an existing destination file"),
			},
			Required: []string{"from", "to"},
		},
	}, s.copyFile)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "delete_file",
		Description: "Delete a file (not a directory) within the workspace. Recoverable via git.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"path": str("workspace-relative file path")},
			Required:   []string{"path"},
		},
	}, s.deleteFile)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "delete_directory",
		Description: "Delete a directory within the workspace. With recursive=true, deletes the whole subtree; otherwise the directory must be empty. Recoverable via git.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":      str("workspace-relative directory path"),
				"recursive": boolean("if true, delete the directory and all its contents"),
			},
			Required: []string{"path"},
		},
	}, s.deleteDirectory)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "list_workspace_roots",
		Description: "Report the writable workspace root(s). Read-only.",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}, s.listWorkspaceRoots)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "get_diff",
		Description: "Fetch the stored diff for a prior mutation by its opId — the diff the model submitted and the diff actually applied. Read-only.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"opId": str("the opId returned by a prior mutation")},
			Required:   []string{"opId"},
		},
	}, s.getDiff)
}

// --- op handlers ---

func (s *Server) createTextFile(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		DryRun  bool   `json:"dryRun"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("create_text_file")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{Path: a.Path}

	p, res := s.resolveConfined(rec, a.Path)
	if res != nil {
		return res, nil
	}
	if _, statErr := os.Lstat(p.String()); statErr == nil {
		return s.rejected(rec, decError, fmt.Errorf("file already exists; use apply_diff/replace to edit")), nil
	}
	if a.DryRun {
		rec.Decision = decDryRun
		s.audit(rec)
		return jsonResult(map[string]any{"opId": rec.RunID, "path": a.Path, "wouldCreate": true, "bytes": len(a.Content)}), nil
	}
	resultDiff := workspace.Unified("/dev/null", "b/"+a.Path, nil, []byte(a.Content), workspace.DefaultContext)
	added, _ := diffStat(resultDiff)
	rec.Mutation().BytesAfter = int64(len(a.Content))
	rec.Mutation().FilesTouched = 1
	rec.Mutation().LinesAdded = added
	rec.Mutation().SHA256After = sha256hex([]byte(a.Content))
	payload, truncated := diffPayload("", resultDiff)
	rec.Mutation().DiffTruncated = truncated
	if isBuildLogic(s.rel(p)) {
		// Hold the create pending human approval.
		return s.awaitApproval(rec, stagedOp{
			OpID: rec.RunID, Tool: rec.Tool, Path: s.rel(p),
			Content: []byte(a.Content), Perm: 0o644, Payload: payload,
		}, s.progressNotifier(ctx, req)), nil
	}
	if err := os.MkdirAll(filepath.Dir(p.String()), 0o755); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	if err := workspace.WriteAtomic(p, []byte(a.Content), 0o644); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	if s.putDiff(rec.RunID, payload) {
		rec.Mutation().HasDiff = true
	}
	rec.Decision = decApplied
	s.audit(rec)
	return jsonResult(map[string]any{"opId": rec.RunID, "path": a.Path, "bytes": len(a.Content), "linesAdded": added}), nil
}

func (s *Server) applyDiff(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path          string `json:"path"`
		Diff          string `json:"diff"`
		PermitNonUTF8 bool   `json:"permit_non_utf8"`
		DryRun        bool   `json:"dryRun"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("apply_diff")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{Path: a.Path}
	p, res := s.resolveConfined(rec, a.Path)
	if res != nil {
		return res, nil
	}
	if a.PermitNonUTF8 {
		return s.applyDiffRepair(rec, p, a.Diff, a.DryRun, s.progressNotifier(ctx, req)), nil
	}
	old, fi, rerr := s.readForEdit(p)
	if rerr != nil {
		return s.rejected(rec, rerr.decision, rerr.err), nil
	}
	newContent, err := workspace.ApplyDiff(old, a.Diff)
	if errors.Is(err, workspace.ErrAlreadyApplied) {
		rec.Decision = decNoChange
		s.audit(rec)
		return jsonResult(map[string]any{"opId": rec.RunID, "path": a.Path, "status": "already_applied", "changed": false}), nil
	}
	if err != nil {
		return s.rejectDiff(rec, decError, err, a.Diff), nil
	}
	if !workspace.ValidUTF8(newContent) {
		return s.rejected(rec, decError, fmt.Errorf("result would not be valid UTF-8")), nil
	}
	return s.finishEdit(rec, p, old, newContent, a.Diff, fi.Mode().Perm(), a.DryRun, s.progressNotifier(ctx, req))
}

func (s *Server) replaceString(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path          string `json:"path"`
		Find          string `json:"find"`
		Replace       string `json:"replace"`
		Scope         string `json:"scope"`
		ExpectedCount *int   `json:"expectedCount"`
		PermitNonUTF8 bool   `json:"permit_non_utf8"`
		DryRun        bool   `json:"dryRun"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("replace_string")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{Path: a.Path}
	if a.Find == "" {
		return s.rejected(rec, decError, fmt.Errorf("find must not be empty")), nil
	}
	p, res := s.resolveConfined(rec, a.Path)
	if res != nil {
		return res, nil
	}
	if a.PermitNonUTF8 {
		return s.replaceStringRepair(rec, p, a.Find, a.Replace, a.Scope, a.ExpectedCount, a.DryRun, s.progressNotifier(ctx, req)), nil
	}
	old, fi, rerr := s.readForEdit(p)
	if rerr != nil {
		return s.rejected(rec, rerr.decision, rerr.err), nil
	}
	matches := strings.Count(string(old), a.Find)
	if matches == 0 {
		return s.rejected(rec, decError, fmt.Errorf("find string not found")), nil
	}
	if a.ExpectedCount != nil && *a.ExpectedCount != matches {
		return s.rejected(rec, decError, fmt.Errorf("expectedCount %d but `find` occurs %d times", *a.ExpectedCount, matches)), nil
	}
	var newContent []byte
	if a.Scope == "all" {
		newContent = []byte(strings.ReplaceAll(string(old), a.Find, a.Replace))
	} else {
		newContent = []byte(strings.Replace(string(old), a.Find, a.Replace, 1))
	}
	if !workspace.ValidUTF8(newContent) {
		return s.rejected(rec, decError, fmt.Errorf("result would not be valid UTF-8")), nil
	}
	return s.finishEdit(rec, p, old, newContent, "", fi.Mode().Perm(), a.DryRun, s.progressNotifier(ctx, req))
}

func (s *Server) replaceRegex(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path          string `json:"path"`
		Pattern       string `json:"pattern"`
		Replacement   string `json:"replacement"`
		Scope         string `json:"scope"`
		ExpectedCount *int   `json:"expectedCount"`
		PermitNonUTF8 bool   `json:"permit_non_utf8"`
		DryRun        bool   `json:"dryRun"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("replace_regex")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{Path: a.Path}
	re, err := regexp.Compile(a.Pattern) // RE2: linear time, no ReDoS
	if err != nil {
		return s.rejected(rec, decPattern, fmt.Errorf("bad regex: %v", err)), nil
	}
	p, res := s.resolveConfined(rec, a.Path)
	if res != nil {
		return res, nil
	}
	if a.PermitNonUTF8 {
		return s.replaceRegexRepair(rec, p, re, a.Replacement, a.Scope, a.ExpectedCount, a.DryRun, s.progressNotifier(ctx, req)), nil
	}
	old, fi, rerr := s.readForEdit(p)
	if rerr != nil {
		return s.rejected(rec, rerr.decision, rerr.err), nil
	}
	locs := re.FindAllIndex(old, -1)
	if len(locs) == 0 {
		return s.rejected(rec, decError, fmt.Errorf("pattern not found")), nil
	}
	if a.ExpectedCount != nil && *a.ExpectedCount != len(locs) {
		return s.rejected(rec, decError, fmt.Errorf("expectedCount %d but pattern matches %d times", *a.ExpectedCount, len(locs))), nil
	}
	var newContent []byte
	if a.Scope == "all" {
		newContent = re.ReplaceAll(old, []byte(a.Replacement))
	} else {
		loc := re.FindSubmatchIndex(old) // first match, with submatches for $ expansion
		expanded := re.Expand(nil, []byte(a.Replacement), old, loc)
		newContent = append(append(append([]byte{}, old[:loc[0]]...), expanded...), old[loc[1]:]...)
	}
	if !workspace.ValidUTF8(newContent) {
		return s.rejected(rec, decError, fmt.Errorf("result would not be valid UTF-8")), nil
	}
	return s.finishEdit(rec, p, old, newContent, "", fi.Mode().Perm(), a.DryRun, s.progressNotifier(ctx, req))
}

func (s *Server) writeBinaryFile(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path        string `json:"path"`
		BytesBase64 string `json:"bytesBase64"`
		Overwrite   bool   `json:"overwrite"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("write_binary_file")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{Path: a.Path}
	data, err := base64.StdEncoding.DecodeString(a.BytesBase64)
	if err != nil {
		return s.rejected(rec, decError, fmt.Errorf("invalid base64: %v", err)), nil
	}
	if int64(len(data)) > workspace.MaxBinaryFileBytes {
		return s.rejected(rec, decOverflow, fmt.Errorf("%d bytes exceeds the %d cap", len(data), workspace.MaxBinaryFileBytes)), nil
	}
	p, res := s.resolveConfined(rec, a.Path)
	if res != nil {
		return res, nil
	}
	if fi, statErr := os.Lstat(p.String()); statErr == nil {
		if fi.IsDir() {
			return s.rejected(rec, decError, fmt.Errorf("path is a directory")), nil
		}
		if !a.Overwrite {
			return s.rejected(rec, decError, fmt.Errorf("file exists; set overwrite to replace it")), nil
		}
	}
	rec.Mutation().BytesAfter = int64(len(data))
	rec.Mutation().FilesTouched = 1
	rec.Mutation().SHA256After = sha256hex(data)
	// Store hash + size, not the bytes; ALWAYS approval-gated (opaque).
	payload := []byte(fmt.Sprintf("binary write to %s\n%d bytes\nsha256 %s\n(binary content is not stored for review)", a.Path, len(data), rec.Mutation().SHA256After))
	return s.awaitApproval(rec, stagedOp{OpID: rec.RunID, Tool: rec.Tool, Path: s.rel(p), Content: data, Perm: 0o644, Payload: payload}, s.progressNotifier(ctx, req)), nil
}

// readErr carries a decision code alongside the error so the caller can audit it.
type readErr struct {
	decision audit.Decision
	err      error
}

// readForEdit reads an existing regular UTF-8 file for an in-place edit op,
// enforcing the size cap.  The returned *readErr (nil on success) carries the
// audit decision for the failure.
func (s *Server) readForEdit(p workspace.Path) ([]byte, os.FileInfo, *readErr) {
	fi, err := os.Lstat(p.String())
	if err != nil {
		return nil, nil, &readErr{decError, fmt.Errorf("file does not exist (edit ops act on existing files; use create_text_file for new ones)")}
	}
	if fi.IsDir() {
		return nil, nil, &readErr{decError, fmt.Errorf("path is a directory")}
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, &readErr{decError, fmt.Errorf("not a regular file")}
	}
	if fi.Size() > workspace.MaxTextFileBytes {
		return nil, nil, &readErr{decOverflow, fmt.Errorf("file %d bytes exceeds the %d cap", fi.Size(), workspace.MaxTextFileBytes)}
	}
	data, err := os.ReadFile(p.String())
	if err != nil {
		return nil, nil, &readErr{decError, err}
	}
	if !workspace.ValidUTF8(data) {
		return nil, nil, &readErr{decError, fmt.Errorf("file is not valid UTF-8; permit_non_utf8 is required to edit it")}
	}
	return data, fi, nil
}

// finishEdit computes the resulting diff and summary, then previews (dryRun) or
// writes atomically, storing the diff payload and auditing the outcome.  A no-op
// (new == old) is no_change, not a spurious write. inputDiff is what the model
// submitted (apply_diff) or "" (the replace ops synthesize their diff).
func (s *Server) finishEdit(rec audit.Record, p workspace.Path, old, newContent []byte, inputDiff string, perm os.FileMode, dryRun bool, notify func(string)) (*mcp.CallToolResult, error) {
	if string(newContent) == string(old) {
		rec.Decision = decNoChange
		s.audit(rec)
		return jsonResult(map[string]any{"opId": rec.RunID, "path": rec.Mutation().Path, "status": "no_change", "changed": false}), nil
	}
	resultDiff := workspace.Unified("a/"+rec.Mutation().Path, "b/"+rec.Mutation().Path, old, newContent, workspace.DefaultContext)
	added, removed := diffStat(resultDiff)
	rec.Mutation().BytesBefore = int64(len(old))
	rec.Mutation().BytesAfter = int64(len(newContent))
	rec.Mutation().FilesTouched = 1
	rec.Mutation().LinesAdded = added
	rec.Mutation().LinesRemoved = removed
	rec.Mutation().SHA256After = sha256hex(newContent)

	if dryRun {
		rec.Decision = decDryRun
		s.audit(rec)
		return jsonResult(map[string]any{"opId": rec.RunID, "path": rec.Mutation().Path, "diff": resultDiff, "linesAdded": added, "linesRemoved": removed, "dryRun": true}), nil
	}
	payload, truncated := diffPayload(inputDiff, resultDiff)
	rec.Mutation().DiffTruncated = truncated
	if isBuildLogic(s.rel(p)) {
		// Hold the reviewed change pending human approval.
		return s.awaitApproval(rec, stagedOp{
			OpID: rec.RunID, Tool: rec.Tool, Path: s.rel(p),
			Content: newContent, Perm: uint32(perm), Payload: payload,
		}, notify), nil
	}
	if err := workspace.WriteAtomic(p, newContent, perm); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	if s.putDiff(rec.RunID, payload) {
		rec.Mutation().HasDiff = true
	}
	rec.Decision = decApplied
	s.audit(rec)
	return jsonResult(map[string]any{"opId": rec.RunID, "path": rec.Mutation().Path, "diff": resultDiff, "bytes": len(newContent), "linesAdded": added, "linesRemoved": removed}), nil
}

func (s *Server) getDiff(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		OpId string `json:"opId"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	id, err := runid.Parse(a.OpId)
	if err != nil {
		return errResult("invalid opId: " + err.Error()), nil
	}
	if s.cfg.Diffs == nil {
		return errResult("diff store not configured"), nil
	}
	payload, err := s.cfg.Diffs.FetchDiff(id)
	if err != nil {
		return errResult("no diff for op " + a.OpId), nil
	}
	return textResult(string(payload)), nil
}

// putDiff stores a diff payload best-effort: a mutation is real even if the
// audit store hiccups.  Reports whether a payload was stored (→ rec.HasDiff).
func (s *Server) putDiff(id runid.ID, payload []byte) bool {
	if s.cfg.Diffs == nil {
		return false
	}
	if err := s.cfg.Diffs.PutDiff(id, payload); err != nil {
		log.Printf("scribe: store diff for %s failed: %v", id, err)
		return false
	}
	return true
}

// diffPayload assembles the review document — the submitted diff (when present)
// and the applied result — truncating + flagging if it exceeds the cap.
func diffPayload(inputDiff, resultDiff string) (payload []byte, truncated bool) {
	var b strings.Builder
	if inputDiff != "" {
		b.WriteString("=== submitted diff ===\n")
		b.WriteString(inputDiff)
		if !strings.HasSuffix(inputDiff, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("\n")
	}
	b.WriteString("=== applied diff ===\n")
	b.WriteString(resultDiff)
	out := []byte(b.String())
	if len(out) > DefaultMaxDiffBytes {
		out = out[:DefaultMaxDiffBytes]
		out = append(out, "\n[... diff truncated at cap ...]\n"...)
		truncated = true
	}
	return out, truncated
}

// diffStat counts added/removed lines in a unified diff, excluding the
// ---/+++ file headers.
func diffStat(diff string) (added, removed int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			// file header, not a content line
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Server) createDirectory(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("create_directory")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{Path: a.Path}
	p, res := s.resolveForWrite(rec, a.Path)
	if res != nil {
		return res, nil
	}
	if err := os.MkdirAll(p.String(), 0o755); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	rec.Decision = decApplied
	s.audit(rec)
	return jsonResult(map[string]any{"opId": rec.RunID, "path": a.Path}), nil
}

func (s *Server) moveFile(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("move_file")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{From: a.From, To: a.To}

	from, err := s.root.Resolve(a.From)
	if err != nil {
		return s.rejected(rec, decConfine, err), nil
	}
	to, err := s.root.Resolve(a.To)
	if err != nil {
		return s.rejected(rec, decConfine, err), nil
	}
	if res := s.gate(rec, from, to); res != nil {
		return res, nil
	}
	if from.String() == to.String() {
		return s.rejected(rec, decError, fmt.Errorf("source and destination are the same path")), nil
	}
	fi, err := os.Lstat(from.String())
	if err != nil {
		return s.rejected(rec, decError, fmt.Errorf("source does not exist")), nil
	}
	if fi.IsDir() {
		return s.rejected(rec, decError, fmt.Errorf("source is a directory; use move_directory")), nil
	}
	if ti, err := os.Lstat(to.String()); err == nil {
		if ti.IsDir() {
			return s.rejected(rec, decError, fmt.Errorf("destination is a directory")), nil
		}
		if !a.Overwrite {
			return s.rejected(rec, decError, fmt.Errorf("destination exists; set overwrite to replace it")), nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(to.String()), 0o755); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	if err := os.Rename(from.String(), to.String()); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	rec.Decision = decApplied
	s.audit(rec)
	return jsonResult(map[string]any{"opId": rec.RunID, "from": a.From, "to": a.To}), nil
}

func (s *Server) moveDirectory(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("move_directory")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{From: a.From, To: a.To}
	from, err := s.root.Resolve(a.From)
	if err != nil {
		return s.rejected(rec, decConfine, err), nil
	}
	to, err := s.root.Resolve(a.To)
	if err != nil {
		return s.rejected(rec, decConfine, err), nil
	}
	if res := s.gate(rec, from, to); res != nil {
		return res, nil
	}
	if from.String() == s.root.Dir() {
		return s.rejected(rec, decError, fmt.Errorf("refusing to move the workspace root")), nil
	}
	if from.String() == to.String() {
		return s.rejected(rec, decError, fmt.Errorf("source and destination are the same path")), nil
	}
	fi, err := os.Lstat(from.String())
	if err != nil || !fi.IsDir() {
		return s.rejected(rec, decError, fmt.Errorf("source is not a directory")), nil
	}
	if _, err := os.Lstat(to.String()); err == nil {
		return s.rejected(rec, decError, fmt.Errorf("destination already exists")), nil
	}
	if err := os.MkdirAll(filepath.Dir(to.String()), 0o755); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	if err := os.Rename(from.String(), to.String()); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	rec.Decision = decApplied
	s.audit(rec)
	return jsonResult(map[string]any{"opId": rec.RunID, "from": a.From, "to": a.To}), nil
}

func (s *Server) copyFile(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("copy_file")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{From: a.From, To: a.To}

	from, err := s.root.Resolve(a.From)
	if err != nil {
		return s.rejected(rec, decConfine, err), nil
	}
	to, err := s.root.Resolve(a.To)
	if err != nil {
		return s.rejected(rec, decConfine, err), nil
	}
	if res := s.gate(rec, to); res != nil { // gate the destination: a copy creates a build file
		return res, nil
	}
	if from.String() == to.String() {
		return s.rejected(rec, decError, fmt.Errorf("source and destination are the same path")), nil
	}
	fi, err := os.Lstat(from.String())
	if err != nil {
		return s.rejected(rec, decError, fmt.Errorf("source does not exist")), nil
	}
	if fi.IsDir() {
		return s.rejected(rec, decError, fmt.Errorf("source is a directory; copy_file copies a single file")), nil
	}
	if fi.Size() > workspace.MaxBinaryFileBytes {
		return s.rejected(rec, decOverflow, fmt.Errorf("source %d bytes exceeds the %d cap", fi.Size(), workspace.MaxBinaryFileBytes)), nil
	}
	if ti, err := os.Lstat(to.String()); err == nil {
		if ti.IsDir() {
			return s.rejected(rec, decError, fmt.Errorf("destination is a directory")), nil
		}
		if !a.Overwrite {
			return s.rejected(rec, decError, fmt.Errorf("destination exists; set overwrite to replace it")), nil
		}
	}
	data, err := os.ReadFile(from.String())
	if err != nil {
		return s.rejected(rec, decError, err), nil
	}
	if err := os.MkdirAll(filepath.Dir(to.String()), 0o755); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	if err := workspace.WriteAtomic(to, data, fi.Mode().Perm()); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	rec.Decision = decApplied
	s.audit(rec)
	return jsonResult(map[string]any{"opId": rec.RunID, "from": a.From, "to": a.To, "bytes": len(data)}), nil
}

func (s *Server) deleteFile(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("delete_file")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{Path: a.Path}
	p, res := s.resolveForWrite(rec, a.Path)
	if res != nil {
		return res, nil
	}
	fi, err := os.Lstat(p.String())
	if err != nil {
		return s.rejected(rec, decError, fmt.Errorf("file does not exist")), nil
	}
	if fi.IsDir() {
		return s.rejected(rec, decError, fmt.Errorf("path is a directory; delete_directory is not yet supported")), nil
	}
	if err := os.Remove(p.String()); err != nil {
		return s.rejected(rec, decError, err), nil
	}
	rec.Decision = decApplied
	s.audit(rec)
	return jsonResult(map[string]any{"opId": rec.RunID, "path": a.Path, "deleted": true}), nil
}

func (s *Server) deleteDirectory(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	rec, err := newToolRecord("delete_directory")
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	rec.Detail = &audit.MutationDetail{Path: a.Path}
	// The recursive flag has no MutationDetail home; it is already reported in the
	// jsonResult response (and is inherent to whether os.RemoveAll vs os.Remove ran).
	p, res := s.resolveForWrite(rec, a.Path)
	if res != nil {
		return res, nil
	}
	if p.String() == s.root.Dir() {
		return s.rejected(rec, decError, fmt.Errorf("refusing to delete the workspace root")), nil
	}
	fi, err := os.Lstat(p.String())
	if err != nil {
		return s.rejected(rec, decError, fmt.Errorf("directory does not exist")), nil
	}
	if !fi.IsDir() {
		return s.rejected(rec, decError, fmt.Errorf("path is a file; use delete_file")), nil
	}
	if a.Recursive {
		err = os.RemoveAll(p.String())
	} else {
		err = os.Remove(p.String()) // fails if the directory is non-empty
	}
	if err != nil {
		return s.rejected(rec, decError, err), nil
	}
	rec.Decision = decApplied
	s.audit(rec)
	return jsonResult(map[string]any{"opId": rec.RunID, "path": a.Path, "deleted": true, "recursive": a.Recursive}), nil
}

func (s *Server) listWorkspaceRoots(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Read-only; not audited.
	return jsonResult(map[string]any{"roots": []string{s.root.Dir()}}), nil
}

// --- helpers ---

// newToolRecord mints the op id and seeds the audit record for one tool call.
func newToolRecord(tool string) (audit.Record, error) {
	id, err := runid.New()
	if err != nil {
		return audit.Record{}, err
	}
	return audit.New(id, tool, "", 0), nil
}

func (s *Server) audit(rec audit.Record) {
	if s.cfg.Audit == nil {
		return
	}
	if err := s.cfg.Audit.Append(rec); err != nil {
		log.Printf("scribe: audit append failed: %v", err)
	}
}

// rejected stamps the decision + reason, audits, and returns an error result.
func (s *Server) rejected(rec audit.Record, decision audit.Decision, err error) *mcp.CallToolResult {
	rec.Decision = decision
	rec.Status = err.Error()
	s.audit(rec)
	return errResult("rejected: " + err.Error())
}

// rejectDiff rejects a diff the engine wouldn't apply (malformed, not-found,
// ambiguous, …) while CAPTURING the submitted diff in the diff store — a rejected
// diff is precisely when the operator wants to see what the agent sent.  The audit
// record gets a diff link (HasDiff) like an applied one.
func (s *Server) rejectDiff(rec audit.Record, decision audit.Decision, err error, submitted string) *mcp.CallToolResult {
	payload := []byte("=== submitted diff — REJECTED ===\n" + err.Error() + "\n\n" + submitted)
	if len(payload) > DefaultMaxDiffBytes {
		payload = append(payload[:DefaultMaxDiffBytes], "\n[... truncated ...]\n"...)
		rec.Mutation().DiffTruncated = true
	}
	if s.putDiff(rec.RunID, payload) {
		rec.Mutation().HasDiff = true
	}
	return s.rejected(rec, decision, err)
}

// rel returns the workspace-relative, slash-normalized form of a confined path.
func (s *Server) rel(p workspace.Path) string {
	r, err := filepath.Rel(s.root.Dir(), p.String())
	if err != nil {
		return p.String()
	}
	return filepath.ToSlash(r)
}

// gate enforces the build-logic refusal policy: a mutation
// that would create, modify, move, or delete a build-affecting path is refused
// and audited rejected_gate.  Returns a result to return immediately, or nil to
// proceed.  Pass every path the op touches.
func (s *Server) gate(rec audit.Record, ps ...workspace.Path) *mcp.CallToolResult {
	for _, p := range ps {
		if r := s.rel(p); isBuildLogic(r) {
			return s.rejected(rec, decGate, fmt.Errorf("%q is build logic; changes require human approval (no approval channel is wired)", r))
		}
	}
	return nil
}

// resolveForWrite resolves a single-path mutation target, then applies the gate.
// On confinement rejection or a gated path it audits and returns a result; on
// success it returns the confined path.  Used by the ops whose gate is a flat
// refusal (create_directory, delete_*); the content ops gate later (they need
// the computed change to stage it for approval) via resolveConfined + finishEdit.
func (s *Server) resolveForWrite(rec audit.Record, input string) (workspace.Path, *mcp.CallToolResult) {
	p, res := s.resolveConfined(rec, input)
	if res != nil {
		return workspace.Path{}, res
	}
	if res := s.gate(rec, p); res != nil {
		return workspace.Path{}, res
	}
	return p, nil
}

// resolveConfined resolves a target with confinement only (no build-logic gate).
func (s *Server) resolveConfined(rec audit.Record, input string) (workspace.Path, *mcp.CallToolResult) {
	p, err := s.root.Resolve(input)
	if err != nil {
		return workspace.Path{}, s.rejected(rec, decConfine, err)
	}
	return p, nil
}

// progressNotifier returns a function that sends an MCP progress notification to
// the caller mid-call, or nil if the client didn't supply a progress token (so
// there's nothing to key progress to).  Used to tell whoever is driving the qwen
// session that a write is now waiting on a human, without unblocking the call.
func (s *Server) progressNotifier(ctx context.Context, req *mcp.CallToolRequest) func(string) {
	token := req.Params.GetProgressToken()
	if token == nil {
		return nil
	}
	var n float64
	return func(msg string) {
		n++
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Message:       msg,
			Progress:      n,
		})
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
