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

// The comprehension tools are the librarian's inference-backed reads
// (docs/librarian.md, "Effort, cost, and the comprehension ops"): they push
// shield-filtered file content to an engine and return a distilled answer with
// a provenance footer.  Content is pushed, never pulled — the shield is
// enforced once, here, before any bytes leave the process.  These are the
// single-file ops (ask_about_file, summarize_file); directory and big-file
// map-reduce are a later sub-phase, so an oversized input REFUSES rather than
// chunks or silently truncates.

package librarian

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/infer"
	"github.com/jeffbstewart/cloister/internal/openai"
	"github.com/jeffbstewart/cloister/internal/shield"
)

// MaxComprehendBytes bounds the file content a single comprehension op hands to
// an engine.  Over the cap the op refuses and names the cap — no silent
// truncation, no chunking (map-reduce is a later sub-phase).
const MaxComprehendBytes = 128 << 10

// Inferencer is the inference seam the comprehension tools drive, mirrored on
// the Auditor pattern so tests fake it with no real HTTP.  *infer.Client
// satisfies it.
type Inferencer interface {
	Ask(ctx context.Context, effort infer.Effort, messages []openai.Message) (infer.Result, error)
}

// System prompts are short and fixed.  They are not a security surface: the
// shield already filtered the bytes before they reach the model.
const (
	askSystemPrompt = "You answer questions about a single file using ONLY the file content provided below.  " +
		"Be concise.  If the answer is not in the file, say so plainly rather than guessing."
	summarizeSystemPrompt = "You summarize a single file using ONLY the file content provided below.  " +
		"Be concise: state the file's purpose and its key elements."
)

func (s *Server) registerComprehensionTools() {
	s.mcp.AddTool(&mcp.Tool{
		Name:        "ask_about_file",
		Description: "Answer a question about one workspace file, grounded only in the file's content.  Optionally restrict to a line range (start, end) — the answer stays distilled either way, so this is how you comprehend part of a file too large for the whole-file cap.  effort 'quick' (default) or 'thorough' buys engine-side depth at the cost of latency; the answer returns with a provenance footer.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":     str("workspace-relative file path"),
				"question": str("the question to answer from the file"),
				"effort":   effortSchema(),
				"start":    integer("optional first line, 1-based, to restrict the question to a range"),
				"end":      integer("optional last line, inclusive; with start, comprehend only that line range"),
			},
			Required: []string{"path", "question"},
		},
	}, s.askAboutFile)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "summarize_file",
		Description: "Summarize one workspace file, grounded only in its content.  Optionally restrict to a line range (start, end) — this is how you summarize part of a file too large for the whole-file cap.  effort 'quick' (default) or 'thorough' buys engine-side depth at the cost of latency; the summary returns with a provenance footer.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":   str("workspace-relative file path"),
				"effort": effortSchema(),
				"start":  integer("optional first line, 1-based, to restrict the summary to a range"),
				"end":    integer("optional last line, inclusive; with start, summarize only that line range"),
			},
			Required: []string{"path"},
		},
	}, s.summarizeFile)
}

// effortSchema is the shared optional-enum schema for the effort knob.
func effortSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Description: "engine-side effort: 'quick' (default) or 'thorough'",
		Enum:        []any{string(infer.Quick), string(infer.Thorough)},
	}
}

// parseEffort defaults an empty effort to Quick and rejects anything outside
// the enum, so an unknown intent fails closed rather than guessing a default.
func parseEffort(raw string) (infer.Effort, error) {
	if raw == "" {
		return infer.Quick, nil
	}
	e := infer.Effort(raw)
	if !e.Valid() {
		return "", fmt.Errorf("effort must be %q or %q", infer.Quick, infer.Thorough)
	}
	return e, nil
}

func (s *Server) askAboutFile(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path     string `json:"path"`
		Question string `json:"question"`
		Effort   string `json:"effort"`
		Start    int    `json:"start"`
		End      int    `json:"end"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if a.Question == "" {
		return errResult("question is required"), nil
	}
	effort, err := parseEffort(a.Effort)
	if err != nil {
		return errResult(err.Error()), nil
	}
	ar, err := s.cfg.Repo.Read(a.Path)
	if err != nil {
		return s.refuse("ask_about_file", err, a.Path), nil
	}
	snippet, loc, err := scopeContent(ar, a.Start, a.End)
	if err != nil {
		return errResult(err.Error()), nil
	}
	msgs := []openai.Message{
		{Role: "system", Content: askSystemPrompt},
		{Role: "user", Content: fmt.Sprintf("File: %s\n\n%s\n\nQuestion: %s", loc, snippet, a.Question)},
	}
	res, err := s.cfg.Infer.Ask(ctx, effort, msgs)
	if err != nil {
		return errResult("inference failed: " + err.Error()), nil
	}
	return comprehendResult(res, effort), nil
}

func (s *Server) summarizeFile(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path   string `json:"path"`
		Effort string `json:"effort"`
		Start  int    `json:"start"`
		End    int    `json:"end"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	effort, err := parseEffort(a.Effort)
	if err != nil {
		return errResult(err.Error()), nil
	}
	ar, err := s.cfg.Repo.Read(a.Path)
	if err != nil {
		return s.refuse("summarize_file", err, a.Path), nil
	}
	snippet, loc, err := scopeContent(ar, a.Start, a.End)
	if err != nil {
		return errResult(err.Error()), nil
	}
	msgs := []openai.Message{
		{Role: "system", Content: summarizeSystemPrompt},
		{Role: "user", Content: fmt.Sprintf("File: %s\n\n%s", loc, snippet)},
	}
	res, err := s.cfg.Infer.Ask(ctx, effort, msgs)
	if err != nil {
		return errResult("inference failed: " + err.Error()), nil
	}
	return comprehendResult(res, effort), nil
}

// scopeContent selects the content a comprehension op will push — the whole
// file, or the requested 1-based inclusive line range — and enforces the size
// cap on that selection.  It takes a shield-cleared AIReadable, not raw bytes,
// so pushing file content into a model prompt is structurally gated: the
// off-host push cannot happen without a value the shield minted.  It returns the
// snippet and a location label for the prompt ("path" or "path (lines A-B)").
// Over the cap it refuses and asks for a narrower RANGE rather than pointing at
// the mechanical readers: those would spill the bytes into the caller's own
// context (defeating the firewall) and cannot feed back into a path-based
// comprehension op.  No silent truncation; whole-file map-reduce for big files
// is a later sub-phase.
func scopeContent(ar shield.AIReadable, start, end int) (snippet, loc string, err error) {
	content := ar.CopyBytes()
	text, loc := string(content), ar.Path()
	if start != 0 || end != 0 {
		var from, to, total int
		// Map the 1-based inclusive [start, end] request to the shared line
		// window's 0-based half-open [from, to); lineSlice clamps to the file.
		text, from, to, total = lineSlice(content, func(n int) (int, int) {
			f := 0
			if start > 1 {
				f = start - 1
			}
			t := n
			if end != 0 {
				t = end
			}
			return f, t
		})
		if from >= to {
			return "", "", fmt.Errorf("line range (start %d, end %d) selects no lines; the file has %d", start, end, total)
		}
		loc = fmt.Sprintf("%s (lines %d-%d)", ar.Path(), from+1, to)
	}
	if len(text) > MaxComprehendBytes {
		return "", "", fmt.Errorf("%s: the selected content is %d bytes, over the %d-byte comprehension cap; pass a narrower line range (start, end) — whole-file map-reduce is a later sub-phase",
			loc, len(text), MaxComprehendBytes)
	}
	return text, loc, nil
}

// comprehendResult renders the engine's answer plus the provenance footer, and
// also carries the same fields as MCP structured content for programmatic use.
// The text footer is the source of truth (docs/librarian.md).
func comprehendResult(res infer.Result, effort infer.Effort) *mcp.CallToolResult {
	r := textResult(res.Answer + footer(res, effort))
	r.StructuredContent = map[string]any{
		"answer":    res.Answer,
		"servedBy":  res.ServedBy,
		"elapsedMs": res.Elapsed.Milliseconds(),
		"tokens":    res.Tokens,
		"effort":    string(effort),
	}
	return r
}

// footer is the compact provenance trailer: model-visible on purpose so the
// quick-vs-thorough cost tradeoff shows in the response.
func footer(res infer.Result, effort infer.Effort) string {
	return fmt.Sprintf("\n\n— librarian · %s → %s · %.1fs · %d tok",
		effort, res.ServedBy, res.Elapsed.Seconds(), res.Tokens)
}
