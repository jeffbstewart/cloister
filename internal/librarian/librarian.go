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

// Package librarian is the read side of the cell (docs/librarian.md):
// mechanical read tools served over MCP from the in-memory workspace
// model (internal/repo), every operation shield-filtered, with denials —
// and only denials — audited to the state service.
//
// Permission presentation: listings synthesize POSIX-style bits as the
// shield's rendering, not the filesystem's opinion — a Stripped path
// shows its read and execute bits removed, so the agent can see that a
// file exists and that it may not read it.
package librarian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/repo"
	"github.com/jeffbstewart/cloister/internal/runid"
	"github.com/jeffbstewart/cloister/internal/shield"
)

// Result caps: every tool's output is bounded; truncation is reported,
// never silent.
const (
	MaxSearchMatches = 200
	MaxListEntries   = 500
	MaxBatchFiles    = 20
	MaxTreeDepth     = 10
)

// Auditor records read denials.  *sink.Client satisfies it.
type Auditor interface {
	Append(audit.Record) error
}

// Config wires the librarian to its model and audit sink.
type Config struct {
	Version string
	Repo    *repo.Repo
	Audit   Auditor    // nil disables denial auditing (tests may set a fake)
	Infer   Inferencer // nil disables the comprehension tools (mechanical-only boot)
}

// Server owns the librarian's MCP tool surface.
type Server struct {
	cfg Config
	mcp *mcp.Server
}

// New builds the librarian tool surface.  The mechanical tools always
// register; the inference-backed comprehension tools register only when an
// Inferencer is wired, so the librarian can boot mechanical-only before the
// inference endpoint exists.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "librarian", Version: cfg.Version}, nil)
	s.registerTools()
	if cfg.Infer != nil {
		s.registerComprehensionTools()
	}
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
		Name:        "read_file",
		Description: "Read a workspace text file. Files marked unreadable (permission bits without r) refuse; binaries and oversized files are not served.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"path": str("workspace-relative file path")},
			Required:   []string{"path"},
		},
	}, s.readFile)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "read_range",
		Description: "Read lines start..end of a text file (1-based, inclusive).",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":  str("workspace-relative file path"),
				"start": integer("first line, 1-based"),
				"end":   integer("last line, inclusive"),
			},
			Required: []string{"path", "start", "end"},
		},
	}, s.readRange)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "read_head",
		Description: "Read the first N lines of a text file.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path": str("workspace-relative file path"),
				"n":    integer("how many lines"),
			},
			Required: []string{"path", "n"},
		},
	}, s.readHead)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "read_tail",
		Description: "Read the last N lines of a text file.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path": str("workspace-relative file path"),
				"n":    integer("how many lines"),
			},
			Required: []string{"path", "n"},
		},
	}, s.readTail)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "batch_read",
		Description: fmt.Sprintf("Read up to %d text files in one call. Per-path errors are reported alongside successful contents.", MaxBatchFiles),
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"paths": {Type: "array", Items: str("workspace-relative file path"), Description: "files to read"},
			},
			Required: []string{"paths"},
		},
	}, s.batchRead)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "stat_file",
		Description: "Metadata for one path: size, mtime, line count, sha256, permission bits (r/x stripped means the content is off-limits).",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"path": str("workspace-relative path")},
			Required:   []string{"path"},
		},
	}, s.statFile)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "list_dir",
		Description: "List a directory's immediate children with permission bits, sizes, and mtimes.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"path": str("workspace-relative directory ('.' for the root)")},
		},
	}, s.listDir)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "tree",
		Description: fmt.Sprintf("Recursive listing under a directory, depth-limited (default 3, max %d), capped at %d entries.", MaxTreeDepth, MaxListEntries),
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":  str("workspace-relative directory ('.' for the root)"),
				"depth": integer("levels below the start (default 3)"),
			},
		},
	}, s.tree)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "glob",
		Description: "Paths matching an anchored glob: path.Match per segment, '**' spans directories ('*.go' is root-only — use '**/*.go' for any depth).",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"pattern": str("anchored glob pattern")},
			Required:   []string{"pattern"},
		},
	}, s.glob)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "search",
		Description: fmt.Sprintf("RE2 search over readable text files. mode: 'context' (matching lines with surrounding context, default), 'files' (paths only), 'count' (per-file match counts), 'total' (one number). Optional glob and path-prefix filters. Matches cap at %d with truncation reported.", MaxSearchMatches),
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"pattern": str("RE2 regular expression"),
				"mode":    str("'context' (default) | 'files' | 'count' | 'total'"),
				"glob":    str("only files matching this anchored glob"),
				"path":    str("only files under this directory prefix"),
				"before":  integer("context lines before each match (mode context, default 0)"),
				"after":   integer("context lines after each match (mode context, default 0)"),
			},
			Required: []string{"pattern"},
		},
	}, s.search)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "recently_modified",
		Description: "Files ordered by modification time, newest first (default 20).",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"limit": integer("how many (default 20)")},
		},
	}, s.recentlyModified)
}

// --- tool handlers ---

func (s *Server) readFile(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	ar, err := s.cfg.Repo.Read(a.Path)
	if err != nil {
		return s.refuse("read_file", err, a.Path), nil
	}
	return textResult(ar.String()), nil
}

func (s *Server) readRange(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path  string `json:"path"`
		Start int    `json:"start"`
		End   int    `json:"end"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if a.Start < 1 || a.End < a.Start {
		return errResult("start must be >= 1 and end >= start"), nil
	}
	return s.serveLines("read_range", a.Path, func(total int) (int, int) {
		return min(a.Start-1, total), min(a.End, total)
	})
}

func (s *Server) readHead(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
		N    int    `json:"n"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if a.N < 1 {
		return errResult("n must be >= 1"), nil
	}
	return s.serveLines("read_head", a.Path, func(total int) (int, int) {
		return 0, min(a.N, total)
	})
}

func (s *Server) readTail(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
		N    int    `json:"n"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if a.N < 1 {
		return errResult("n must be >= 1"), nil
	}
	return s.serveLines("read_tail", a.Path, func(total int) (int, int) {
		return max(0, total-a.N), total
	})
}

// lineSlice splits content into lines and returns lines[from:to) joined, plus
// the resolved (clamped) bounds and the total line count.  window computes the
// desired 0-based half-open [from, to) from the total.  It is the one place the
// librarian turns a blob into a line window — shared by the mechanical range
// reads (serveLines) and the comprehension range ops (scopeContent).
func lineSlice(content []byte, window func(total int) (from, to int)) (text string, from, to, total int) {
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	total = len(lines)
	from, to = window(total)
	if from < 0 {
		from = 0
	}
	if from > total {
		from = total
	}
	if to < from {
		to = from
	}
	if to > total {
		to = total
	}
	return strings.Join(lines[from:to], "\n"), from, to, total
}

// serveLines reads a file and returns the [from, to) line window the selector
// picks from its total line count (0-based half-open).
func (s *Server) serveLines(tool, path string, window func(total int) (int, int)) (*mcp.CallToolResult, error) {
	ar, err := s.cfg.Repo.Read(path)
	if err != nil {
		return s.refuse(tool, err, path), nil
	}
	text, from, to, total := lineSlice(ar.CopyBytes(), window)
	return jsonResult(map[string]any{
		"path": path, "fromLine": from + 1, "toLine": to, "totalLines": total,
		"content": text,
	}), nil
}

func (s *Server) batchRead(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Paths []string `json:"paths"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if len(a.Paths) == 0 || len(a.Paths) > MaxBatchFiles {
		return errResult(fmt.Sprintf("paths must name 1..%d files", MaxBatchFiles)), nil
	}
	type fileOut struct {
		Path    string `json:"path"`
		Content string `json:"content,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	var out []fileOut
	var denied []string
	for _, p := range a.Paths {
		ar, err := s.cfg.Repo.Read(p)
		switch {
		case err == nil:
			out = append(out, fileOut{Path: p, Content: ar.String()})
		case errors.Is(err, repo.ErrForbidden):
			denied = append(denied, p)
			out = append(out, fileOut{Path: p, Error: err.Error()})
		default:
			out = append(out, fileOut{Path: p, Error: err.Error()})
		}
	}
	// One denial record carries every denied path of the batch.
	s.auditDenial("batch_read", denied...)
	return jsonResult(map[string]any{"files": out}), nil
}

func (s *Server) statFile(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	e, err := s.cfg.Repo.Stat(a.Path)
	if err != nil {
		return s.refuse("stat_file", err, a.Path), nil
	}
	return jsonResult(entryOut(e)), nil
}

func (s *Server) listDir(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if a.Path == "" {
		a.Path = "."
	}
	entries, err := s.cfg.Repo.List(a.Path)
	if err != nil {
		return s.refuse("list_dir", err, a.Path), nil
	}
	return jsonResult(map[string]any{"path": a.Path, "entries": entriesOut(entries, MaxListEntries)}), nil
}

func (s *Server) tree(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if a.Path == "" {
		a.Path = "."
	}
	if a.Depth <= 0 {
		a.Depth = 3
	}
	if a.Depth > MaxTreeDepth {
		a.Depth = MaxTreeDepth
	}
	prefix := ""
	baseDepth := 0
	if a.Path != "." {
		if _, err := s.cfg.Repo.Stat(a.Path); err != nil {
			return s.refuse("tree", err, a.Path), nil
		}
		prefix = a.Path + "/"
		baseDepth = strings.Count(a.Path, "/") + 1
	}
	var picked []repo.Entry
	for _, e := range s.cfg.Repo.All() {
		if prefix != "" && !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		if strings.Count(e.Path, "/")-baseDepth >= a.Depth {
			continue
		}
		picked = append(picked, e)
	}
	return jsonResult(map[string]any{"path": a.Path, "depth": a.Depth, "entries": entriesOut(picked, MaxListEntries)}), nil
}

func (s *Server) glob(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Pattern string `json:"pattern"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if a.Pattern == "" {
		return errResult("pattern is required"), nil
	}
	var picked []repo.Entry
	for _, e := range s.cfg.Repo.All() {
		if !e.IsDir && shield.Glob(a.Pattern, e.Path) {
			picked = append(picked, e)
		}
	}
	return jsonResult(map[string]any{"pattern": a.Pattern, "entries": entriesOut(picked, MaxListEntries)}), nil
}

// grepResident is the shared scan skeleton behind both search and
// find_relevant_files: it walks the resident tree, applies the prefix and glob
// filters, splits each file into lines, and calls hit for every line that re
// matches.  prefix is a "dir/" path prefix ("" = the whole tree); glob is an
// anchored shield.Glob ("" = every file).  hit receives the file's
// workspace-relative path, the 1-based line number, the matching line, and the
// file's full line slice (so a caller can pull surrounding context without
// re-splitting).  The shield is enforced by ForEachResident — jailed, binary,
// and oversized files never reach the callback.
func (s *Server) grepResident(re *regexp.Regexp, prefix, glob string, hit func(rel string, lineNum int, line string, lines []string)) error {
	return s.cfg.Repo.ForEachResident(func(ar shield.AIReadable) error {
		rel := ar.Path()
		if prefix != "" && !strings.HasPrefix(rel, prefix) {
			return nil
		}
		if glob != "" && !shield.Glob(glob, rel) {
			return nil
		}
		lines := strings.Split(ar.String(), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				hit(rel, i+1, line, lines)
			}
		}
		return nil
	})
}

func (s *Server) search(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Mode    string `json:"mode"`
		Glob    string `json:"glob"`
		Path    string `json:"path"`
		Before  int    `json:"before"`
		After   int    `json:"after"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	re, err := regexp.Compile(a.Pattern) // RE2: linear time, no ReDoS
	if err != nil {
		return errResult("bad regex: " + err.Error()), nil
	}
	if a.Mode == "" {
		a.Mode = "context"
	}
	prefix := ""
	if a.Path != "" && a.Path != "." {
		prefix = strings.TrimSuffix(a.Path, "/") + "/"
	}

	type match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	var matches []match
	counts := map[string]int{}
	total := 0
	truncated := false

	scanErr := s.grepResident(re, prefix, a.Glob, func(rel string, lineNum int, _ string, lines []string) {
		total++
		counts[rel]++
		if a.Mode == "context" {
			if len(matches) >= MaxSearchMatches {
				truncated = true
				return
			}
			i := lineNum - 1
			from := max(0, i-a.Before)
			to := min(len(lines), i+a.After+1)
			for j := from; j < to; j++ {
				matches = append(matches, match{Path: rel, Line: j + 1, Text: lines[j]})
			}
		}
	})
	if scanErr != nil {
		return errResult("search: " + scanErr.Error()), nil
	}

	out := map[string]any{"pattern": a.Pattern, "mode": a.Mode, "totalMatches": total, "truncated": truncated}
	switch a.Mode {
	case "context":
		out["matches"] = matches
	case "files":
		out["files"] = sortedKeys(counts)
	case "count":
		out["counts"] = counts
	case "total":
		// totalMatches already present
	default:
		return errResult("mode must be context | files | count | total"), nil
	}
	return jsonResult(out), nil
}

func (s *Server) recentlyModified(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Limit int `json:"limit"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	var files []repo.Entry
	for _, e := range s.cfg.Repo.All() {
		if !e.IsDir {
			files = append(files, e)
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].ModTime.After(files[j].ModTime) })
	if len(files) > a.Limit {
		files = files[:a.Limit]
	}
	return jsonResult(map[string]any{"entries": entriesOut(files, a.Limit)}), nil
}

// --- helpers ---

// refuse maps a repo error to a tool refusal, auditing shield denials —
// and only shield denials (capacity refusals are errors, not secrets).
func (s *Server) refuse(tool string, err error, paths ...string) *mcp.CallToolResult {
	if errors.Is(err, repo.ErrForbidden) {
		s.auditDenial(tool, paths...)
		return errResult("denied: " + err.Error())
	}
	return errResult(err.Error())
}

// auditDenial appends one denial record naming the denied paths.
func (s *Server) auditDenial(tool string, paths ...string) {
	if s.cfg.Audit == nil || len(paths) == 0 {
		return
	}
	id, err := runid.New()
	if err != nil {
		log.Printf("librarian: mint denial op id: %v", err)
		return
	}
	rec := audit.New(id, tool, audit.DecisionReadDenied, 0)
	rec.Detail = &audit.ReadDetail{Paths: paths}
	if err := s.cfg.Audit.Append(rec); err != nil {
		log.Printf("librarian: audit append failed: %v", err)
	}
}

// entryOut renders one Entry with the synthesized permission bits.
func entryOut(e repo.Entry) map[string]any {
	out := map[string]any{
		"path":  e.Path,
		"perms": perms(e),
		"mtime": e.ModTime.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if e.IsDir {
		out["dir"] = true
		return out
	}
	out["size"] = e.Size
	if e.Resident {
		out["lines"] = e.LineCount
		out["sha256"] = e.SHA256
	}
	return out
}

func entriesOut(entries []repo.Entry, limit int) []map[string]any {
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryOut(e))
	}
	return out
}

// perms synthesizes POSIX-style bits as the shield's presentation of
// what the librarian can serve — not the filesystem's opinion.  No
// entry EVER shows a write bit: the librarian offers no writes, and a
// visible `w` would nudge the harness toward filesystem writes instead
// of the scribe's tools.  A Stripped path exists by name with read and
// execute removed.
func perms(e repo.Entry) string {
	switch {
	case e.IsDir && e.Visibility == shield.Stripped:
		return "d---------"
	case e.IsDir:
		return "dr-xr-xr-x"
	case e.Visibility == shield.Stripped:
		return "----------"
	default:
		return "-r--r--r--"
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
