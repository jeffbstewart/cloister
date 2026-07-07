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

// Package mcpserver exposes the manifest's action menu as MCP tools over
// streamable HTTP: one tool per action, plus
// get_log and harness_info.  With no valid manifest it degrades to serving
// harness_info only — never a partial or guessed menu.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/digest"
	"github.com/jeffbstewart/cloister/internal/manifest"
	"github.com/jeffbstewart/cloister/internal/runid"
	"github.com/jeffbstewart/cloister/internal/runner"
)

const (
	defaultLogPageLines = 200
	maxLogPageLines     = 1000
	digestTailLines     = 30
)

// Auditor records one audit line per action call.  Both a local
// *audit.Log and the state-service client satisfy it, so the builder can
// send audit records over the network instead of owning /state.
type Auditor interface {
	Append(audit.Record) error
}

// LogFetcher reads a run's full log back from durable storage — get_log's
// fallback once the local spool has pruned the file.  The state-service
// client satisfies it.
type LogFetcher interface {
	FetchLog(runid.ID) ([]byte, error)
}

// Config wires the server to its collaborators and mount points.
type Config struct {
	Version      string
	ToolchainID  string
	Workspace    string // actions run here; the manifest lives at its root
	ManifestPath string
	LogsDir      string // local spool for digests + get_log's fast path
	Runner       *runner.Runner
	Audit        Auditor
	LogFetcher   LogFetcher // optional: get_log fallback to durable storage
}

// Server owns the MCP tool surface and the HTTP handler around it.
type Server struct {
	cfg      Config
	mcp      *mcp.Server
	degraded string // startup manifest problem; "" when the full menu is served
}

// New builds the tool surface from the manifest at startup.  A missing or
// invalid manifest is not fatal: the server comes up serving only
// harness_info, reporting the precise reason.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "agent-builder", Version: cfg.Version}, nil)
	s.addHarnessInfo()

	m, err := s.loadManifest()
	if err != nil {
		s.degraded = err.Error()
		log.Printf("degraded mode (harness_info only): %v", err)
		return s
	}
	s.addGetLog()
	names := sortedKeys(m.Actions)
	for _, name := range names {
		s.addAction(name, m.Actions[name])
	}
	log.Printf("serving %d actions: %s", len(names), strings.Join(names, ", "))
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

func (s *Server) loadManifest() (*manifest.Manifest, error) {
	m, err := manifest.Load(s.cfg.ManifestPath, s.cfg.ToolchainID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no manifest at %s; no actions available", s.cfg.ManifestPath)
	}
	return m, err
}

// addAction registers one manifest action as an MCP tool.  The manifest
// snapshot is used only for the tool's schema/description; every call
// re-reads the manifest.
func (s *Server) addAction(name string, a *manifest.Action) {
	props := map[string]*jsonschema.Schema{}
	for pname, p := range a.Params {
		props[pname] = &jsonschema.Schema{
			Type:        "string",
			Description: fmt.Sprintf("%s (must fully match %s)", p.Description, p.Pattern),
		}
	}
	desc := a.Description
	if desc == "" {
		desc = "Run the " + name + " action"
	}
	s.mcp.AddTool(
		&mcp.Tool{
			Name:        name,
			Description: desc,
			InputSchema: &jsonschema.Schema{Type: "object", Properties: props},
		},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.runAction(ctx, name, req)
		},
	)
}

func (s *Server) runAction(ctx context.Context, name string, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// opID is this action-call's event id, used for pre-run rejections (a run
	// substitutes its own RunID).  Every audit line stays addressable.
	opID, err := runid.New()
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	params, err := stringParams(req.Params.Arguments)
	if err != nil {
		s.audit(audit.New(opID, name, audit.DecisionRejectedParam, 0))
		return errResult(fmt.Sprintf("bad arguments: %v", err)), nil
	}
	cmd := &audit.CommandDetail{Params: params}
	emit := func(id runid.ID, d audit.Decision, dur time.Duration) {
		rec := audit.New(id, name, d, dur)
		rec.Detail = cmd
		s.audit(rec)
	}

	// Fresh read on every call: manifest edits take effect without a
	// container restart.
	m, err := s.loadManifest()
	if err != nil {
		emit(opID, audit.DecisionRejectedNoManifest, 0)
		return errResult(err.Error()), nil
	}
	a, ok := m.Actions[name]
	if !ok {
		emit(opID, audit.DecisionRejectedNoManifest, 0)
		return errResult(fmt.Sprintf("action %q is no longer in the manifest", name)), nil
	}

	extra, err := a.Args(params)
	if err != nil {
		emit(opID, audit.DecisionRejectedParam, 0)
		return errResult(fmt.Sprintf("rejected: %v", err)), nil
	}
	argv := append(slices.Clone(a.Run), extra...)
	// Record what actually runs: busy and run records both carry it,
	// so a tampered manifest's argv shows up in the audit and status pages.
	cmd.Argv = argv

	res, err := s.cfg.Runner.Run(ctx, runner.Request{
		Action:  name,
		Argv:    argv,
		Dir:     s.cfg.Workspace,
		Timeout: a.Timeout.Duration(),
		Env:     cacheEnv(m),
	})
	var busy *runner.ErrBusy
	if errors.As(err, &busy) {
		emit(opID, audit.DecisionRejectedBusy, 0)
		return jsonResult(map[string]any{
			"status":      "busy",
			"activeRunId": busy.ActiveRunID,
			"hint":        "one action runs at a time; watch it via get_log(activeRunId) or retry when it finishes",
		}), nil
	}
	if err != nil {
		return errResult(fmt.Sprintf("internal: %v", err)), nil
	}

	cmd.ExitCode = &res.ExitCode
	cmd.LogPath = res.LogPath
	cmd.LogBytes = res.LogBytes
	rec := audit.New(res.RunID, name, audit.DecisionRun, res.Duration)
	rec.Status = string(res.Status)
	rec.Detail = cmd
	s.audit(rec)

	return jsonResult(s.buildDigest(a, res)), nil
}

func (s *Server) buildDigest(a *manifest.Action, res *runner.Result) digest.Digest {
	parse, _ := digest.Get(a.ParserName())
	var f digest.Findings
	if data, err := os.ReadFile(res.LogPath); err == nil {
		f = parse(data)
	}
	tail := res.Tail
	if len(tail) > digestTailLines {
		tail = tail[len(tail)-digestTailLines:]
	}
	return digest.Digest{
		RunID:         res.RunID,
		Status:        string(res.Status),
		ExitCode:      res.ExitCode,
		Duration:      audit.Duration(res.Duration),
		Error:         res.Err,
		FailedTasks:   f.FailedTasks,
		CompileErrors: f.CompileErrors,
		FailedTests:   f.FailedTests,
		LogTotalLines: res.LogTotalLines,
		LogTail:       tail,
		Hint:          f.Hint(res.RunID),
	}
}

// cacheEnv resolves the manifest's cache env vars for the child process:
// the container's own value passes through (compose sets it), falling back
// to the declared mount path so the manifest is self-sufficient.
func cacheEnv(m *manifest.Manifest) map[string]string {
	env := map[string]string{}
	for _, c := range m.Caches {
		if c.Env == "" {
			continue
		}
		v := os.Getenv(c.Env)
		if v == "" {
			v = c.Path
		}
		env[c.Env] = v
	}
	return env
}

func (s *Server) addGetLog() {
	s.mcp.AddTool(
		&mcp.Tool{
			Name:        "get_log",
			Description: "Page through the full persisted log of a prior run",
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"runId":    {Type: "string", Description: "runId returned by a prior action call"},
					"fromLine": {Type: "integer", Description: "1-based first line to return (default 1)"},
					"maxLines": {Type: "integer", Description: "lines to return (default 200, max 1000)"},
				},
				Required: []string{"runId"},
			},
		},
		s.getLog,
	)
}

func (s *Server) getLog(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		RunID    string `json:"runId"`
		FromLine int    `json:"fromLine"`
		MaxLines int    `json:"maxLines"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("bad arguments: %v", err)), nil
		}
	}
	// runid.Parse is the trust boundary for this agent-supplied value: its
	// strict alphabet is what makes the path join below traversal-proof.
	id, err := runid.Parse(args.RunID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Fast path: the local spool.  Fallback: durable storage, for runs the
	// spool has already pruned.
	data, err := os.ReadFile(filepath.Join(s.cfg.LogsDir, id.String()+".log"))
	if err != nil && s.cfg.LogFetcher != nil {
		data, err = s.cfg.LogFetcher.FetchLog(id)
	}
	if err != nil {
		return errResult(fmt.Sprintf("no log for run %q", id)), nil
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // drop the empty tail after a final newline
	}
	total := len(lines)
	from := args.FromLine
	if from < 1 {
		from = 1
	}
	max := args.MaxLines
	if max <= 0 {
		max = defaultLogPageLines
	}
	if max > maxLogPageLines {
		max = maxLogPageLines
	}
	if from > total {
		return textResult(fmt.Sprintf("%s: %d lines total; fromLine %d is past the end", args.RunID, total, from)), nil
	}
	end := from - 1 + max
	if end > total {
		end = total
	}
	header := fmt.Sprintf("%s: lines %d-%d of %d\n", args.RunID, from, end, total)
	return textResult(header + strings.Join(lines[from-1:end], "\n")), nil
}

func (s *Server) addHarnessInfo() {
	s.mcp.AddTool(
		&mcp.Tool{
			Name:        "harness_info",
			Description: "Report manifest status, toolchain id, available actions, queue state, and recent runs",
			InputSchema: &jsonschema.Schema{Type: "object"},
		},
		s.harnessInfo,
	)
}

func (s *Server) harnessInfo(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info := map[string]any{
		"serverVersion": s.cfg.Version,
		"toolchain":     s.cfg.ToolchainID,
		"manifestPath":  s.cfg.ManifestPath,
	}
	if s.cfg.Runner != nil {
		busy, active, recent := s.cfg.Runner.State()
		info["busy"] = busy
		if !active.IsZero() {
			info["activeRunId"] = active
		}
		info["recentRuns"] = recent
	}

	m, err := s.loadManifest()
	if err != nil {
		info["manifest"] = "unavailable: " + err.Error()
		info["actions"] = []any{}
		if s.degraded == "" {
			info["note"] = "the manifest was valid at startup but is broken now; action calls will be rejected until it is fixed"
		}
		return jsonResult(info), nil
	}

	info["manifest"] = "ok"
	var actions []map[string]any
	for _, name := range sortedKeys(m.Actions) {
		a := m.Actions[name]
		entry := map[string]any{
			"name":        name,
			"description": a.Description,
			"timeout":     a.Timeout.Duration().String(),
			"parser":      a.ParserName(),
		}
		if len(a.Params) > 0 {
			ps := map[string]any{}
			for pn, p := range a.Params {
				ps[pn] = map[string]any{
					"description": p.Description,
					"flag":        p.Flag,
					"pattern":     p.Pattern,
				}
			}
			entry["params"] = ps
		}
		actions = append(actions, entry)
	}
	info["actions"] = actions
	if s.degraded != "" {
		info["note"] = "manifest is valid now but the tool menu was fixed at startup in degraded mode; restart the builder container to expose these actions"
	}
	return jsonResult(info), nil
}

func (s *Server) audit(r audit.Record) {
	if s.cfg.Audit == nil {
		return
	}
	if err := s.cfg.Audit.Append(r); err != nil {
		log.Printf("audit append failed: %v", err)
	}
}

// stringParams decodes tool arguments into the string map the manifest
// param validator expects.
func stringParams(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		sv, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("param %q must be a string", k)
		}
		out[k] = sv
	}
	return out, nil
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
