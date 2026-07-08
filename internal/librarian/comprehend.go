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
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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

// MaxDirFiles and MaxDirBytes are the big-tree guard on summarize_directory
// (docs/librarian.md): a map-reduce over a huge tree is thousands of engine
// calls, so over either threshold the op refuses and asks for a narrower scope
// rather than silently launching (or silently truncating) the fan-out.
const (
	MaxDirFiles = 40
	MaxDirBytes = 2 << 20
)

// find_relevant_files caps bound the internal retrieve-then-rank loop
// (docs/librarian.md): keyword expansion never exceeds MaxKeywords terms, only
// the top MaxRerankCandidates grep hits are fed to the reranker (more are
// dropped and the result flags it truncated), the ranked answer returns at most
// MaxRelevantFiles paths, and each candidate carries one snippet capped to
// relevantSnippetChars so the rerank prompt stays small.
const (
	MaxKeywords          = 12
	MaxRerankCandidates  = 20
	MaxRelevantFiles     = 10
	relevantSnippetChars = 200
)

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
	dirReduceSystemPrompt = "You are given short summaries of the files in one directory. " +
		"Write a concise overview of what the directory contains and how its files relate. " +
		"Use only the summaries provided."
	keywordSystemPrompt = "Expand the user's question into a short list of search keywords and synonyms " +
		"likely to appear in relevant source files.  Reply with ONLY the keywords, comma-separated."
	rerankSystemPrompt = "Given the question and candidate files (each a path and a matching snippet), " +
		"list the files most relevant to answering it, most relevant first.  " +
		"Reply one per line as `<path> — <reason>`, using ONLY the given paths."
)

// errDirBudget is the sentinel ForEachResident's callback returns to stop the
// walk once the directory would exceed the big-tree guard; the caller
// distinguishes it from a real error with errors.Is and refuses.
var errDirBudget = errors.New("summarize_directory: budget exceeded")

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

	s.mcp.AddTool(&mcp.Tool{
		Name:        "summarize_directory",
		Description: "Summarize a directory by digesting each resident file (map) then synthesizing one overview (reduce), grounded only in that content.  A context-saving alternative to reading a whole tree.  Over a file-count / total-size guard it refuses and asks for a narrower subdirectory rather than launching thousands of engine calls.  effort 'quick' (default) or 'thorough' deepens the synthesis; the overview returns with an aggregate provenance footer.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path":   str("workspace-relative directory path; empty or '.' for the workspace root"),
				"effort": effortSchema(),
			},
			Required: []string{"path"},
		},
	}, s.summarizeDirectory)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "find_relevant_files",
		Description: "Locate the workspace files most relevant to a natural-language question (\"where is retry handled?\").  Runs an internal keyword-expand -> grep -> rerank loop over resident files and returns a ranked list of paths, each with a one-line reason — never the intermediate candidate lists.  Optional path (directory prefix) and glob narrow the search; effort 'quick' (default) or 'thorough' deepens the final ranking (keyword expansion is always quick).  Embedding-based semantic recall is a later phase.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"question": str("the question to locate relevant files for"),
				"path":     str("optional workspace-relative directory prefix to restrict the search; empty or '.' for the whole tree"),
				"glob":     str("optional anchored glob to restrict candidate files (e.g. '**/*.go')"),
				"effort":   effortSchema(),
			},
			Required: []string{"question"},
		},
	}, s.findRelevantFiles)
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
	return footerParts(effort, res.ServedBy, res.Elapsed, res.Tokens)
}

// footerParts renders the provenance trailer from already-aggregated values, so
// a map-reduce op can pass its summed tokens/elapsed and its combined engine set
// (single-file footer just forwards one Result's fields).
func footerParts(effort infer.Effort, servedBy string, elapsed time.Duration, tokens int) string {
	return fmt.Sprintf("\n\n— librarian · %s → %s · %.1fs · %d tok",
		effort, servedBy, elapsed.Seconds(), tokens)
}

// provenance accumulates tokens, wall-clock, and the engine set across the
// several engine calls a multi-call op makes (map-reduce, or the
// retrieve-then-rank loop), so the footer reports one aggregate and names EVERY
// engine that served — a mixed set means a fallback happened mid-op and both
// are named, never a silent substitution (the agency invariant).
type provenance struct {
	tokens  int
	elapsed time.Duration
	engines map[string]bool
}

// add folds one Result into the running totals.
func (p *provenance) add(res infer.Result) {
	p.tokens += res.Tokens
	p.elapsed += res.Elapsed
	if res.ServedBy != "" {
		if p.engines == nil {
			p.engines = map[string]bool{}
		}
		p.engines[res.ServedBy] = true
	}
}

// servedBy is the engine set as a sorted, "+"-joined string for the footer.
func (p *provenance) servedBy() string {
	names := make([]string, 0, len(p.engines))
	for n := range p.engines {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "+")
}

// summarizeDirectory summarizes a directory by map-reduce: a cheap per-file
// digest (map) followed by one synthesis (reduce).  The map always runs at
// infer.Quick — depth pays off in the reduce, not in N thorough per-file passes,
// so `thorough` improves the overview without N thorough calls — while the
// reduce runs at the caller's requested effort.
func (s *Server) summarizeDirectory(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Path   string `json:"path"`
		Effort string `json:"effort"`
	}
	if err := decode(req, &a); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	effort, err := parseEffort(a.Effort)
	if err != nil {
		return errResult(err.Error()), nil
	}

	// Validate the directory.  Root ("" or ".") needs no Stat; any other path
	// must resolve to a visible directory — a jailed or hidden path denies and
	// audits (via refuse), a non-directory is a plain error.
	dir := path.Clean(filepath.ToSlash(a.Path))
	root := dir == "." || dir == ""
	label := dir
	if root {
		label = "the workspace root"
	} else {
		entry, err := s.cfg.Repo.Stat(dir)
		if err != nil {
			return s.refuse("summarize_directory", err, dir), nil
		}
		if !entry.IsDir {
			return errResult(fmt.Sprintf("%s is not a directory", dir)), nil
		}
	}

	// Collect resident file content under the repo lock.  ForEachResident holds
	// the lock across the callback, so we MUST NOT call the engine here: we copy
	// the bytes (CopyBytes returns an owned copy, safe to retain past the
	// callback) and run every inference AFTER the walk returns.  Jailed, binary,
	// and oversized files are already absent from ForEachResident — the correct
	// silent-skip for a tree-wide op.
	type collected struct {
		path    string
		content []byte
	}
	prefix := ""
	if !root {
		prefix = dir + "/"
	}
	var files []collected
	var totalBytes int
	var budgetErr error
	walkErr := s.cfg.Repo.ForEachResident(func(ar shield.AIReadable) error {
		p := ar.Path()
		if prefix != "" && !strings.HasPrefix(p, prefix) {
			return nil
		}
		content := ar.CopyBytes()
		// Count only the bytes we would actually push (each file is capped to
		// MaxComprehendBytes below).
		size := len(content)
		if size > MaxComprehendBytes {
			size = MaxComprehendBytes
		}
		// Budget guard: refuse rather than silently truncate the fan-out.  Stop
		// the walk on the first file that would tip us over either limit.
		if len(files)+1 > MaxDirFiles {
			budgetErr = fmt.Errorf("%s has more than %d readable files (the summarize_directory limit); summarize a narrower subdirectory", label, MaxDirFiles)
			return errDirBudget
		}
		if totalBytes+size > MaxDirBytes {
			budgetErr = fmt.Errorf("%s exceeds the %d-byte summarize_directory content budget; summarize a narrower subdirectory", label, MaxDirBytes)
			return errDirBudget
		}
		files = append(files, collected{path: p, content: content})
		totalBytes += size
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, errDirBudget) {
			return errResult(budgetErr.Error()), nil
		}
		return errResult(walkErr.Error()), nil
	}
	if len(files) == 0 {
		return errResult("no readable files under " + label), nil
	}

	// Map: one cheap infer.Quick digest per file.  Sequential is fine here — the
	// budget guard bounds the count; parallelizing the map is a possible later
	// refinement.
	type fileSummary struct {
		Path    string `json:"path"`
		Summary string `json:"summary"`
	}
	var summaries []fileSummary
	var truncated int
	var prov provenance
	for _, fc := range files {
		content := fc.content
		note := ""
		if len(content) > MaxComprehendBytes {
			content = content[:MaxComprehendBytes]
			note = " (truncated)"
			truncated++
		}
		msgs := []openai.Message{
			{Role: "system", Content: summarizeSystemPrompt},
			{Role: "user", Content: fmt.Sprintf("File: %s%s\n\n%s", fc.path, note, content)},
		}
		res, err := s.cfg.Infer.Ask(ctx, infer.Quick, msgs)
		if err != nil {
			return errResult("inference failed: " + err.Error()), nil
		}
		prov.add(res)
		summaries = append(summaries, fileSummary{Path: fc.path, Summary: res.Answer})
	}

	// Reduce: one synthesis at the caller's requested effort over the per-file
	// summaries.
	var b strings.Builder
	fmt.Fprintf(&b, "Directory: %s\n\n", label)
	for _, fs := range summaries {
		fmt.Fprintf(&b, "- %s: %s\n", fs.Path, fs.Summary)
	}
	res, err := s.cfg.Infer.Ask(ctx, effort, []openai.Message{
		{Role: "system", Content: dirReduceSystemPrompt},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return errResult("inference failed: " + err.Error()), nil
	}
	prov.add(res)

	// Aggregate provenance across all N+1 calls; provenance names every engine
	// that served so a mixed-engine fallback is never silent.
	servedBy := prov.servedBy()
	r := textResult(res.Answer + footerParts(effort, servedBy, prov.elapsed, prov.tokens))
	r.StructuredContent = map[string]any{
		"overview":        res.Answer,
		"files":           summaries,
		"filesSummarized": len(summaries),
		"filesTruncated":  truncated,
		"servedBy":        servedBy,
		"elapsedMs":       prov.elapsed.Milliseconds(),
		"tokens":          prov.tokens,
		"effort":          string(effort),
	}
	return r, nil
}

// rankedFile is one entry of the ranked result: a path the shield surfaced and
// a one-line reason it is relevant.
type rankedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// candidate is one grep hit accumulated during retrieve: a resident path, how
// many of its lines matched the keyword regexp, and the first matching line as
// a representative snippet for the rerank prompt.
type candidate struct {
	path    string
	count   int
	snippet string
}

// findRelevantFiles is the semantic locate: an internal retrieve-then-rank loop
// (docs/librarian.md) that never exposes its stages to the agent.  A cheap
// keyword-expand (always infer.Quick — like summarize_directory's map, depth
// pays off only in the final step) turns the question into a keyword regexp;
// grepResident retrieves candidates from the resident tree; and ONE rerank at
// the caller's requested effort orders them.  It returns only the ranked paths,
// so the candidate lists never burn the agent's context.  Embeddings for true
// semantic recall are deferred to Phase 6; v1 is keyword-expand + grep + rerank.
func (s *Server) findRelevantFiles(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Question string `json:"question"`
		Path     string `json:"path"`
		Glob     string `json:"glob"`
		Effort   string `json:"effort"`
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

	var prov provenance

	// 1. Keyword expansion (map).  Always infer.Quick: only the rerank honors the
	// requested effort, mirroring summarize_directory's map=quick / reduce=effort
	// split — a heavy model earns its keep ranking, not brainstorming synonyms.
	kwRes, err := s.cfg.Infer.Ask(ctx, infer.Quick, []openai.Message{
		{Role: "system", Content: keywordSystemPrompt},
		{Role: "user", Content: a.Question},
	})
	if err != nil {
		return errResult("inference failed: " + err.Error()), nil
	}
	prov.add(kwRes)
	keywords := parseKeywords(kwRes.Answer)
	if len(keywords) == 0 {
		// The model yielded nothing usable — fall back to the question's own
		// significant words rather than making a second call.
		keywords = questionKeywords(a.Question)
	}
	re := keywordRegexp(keywords)

	// 2. Grep candidates (retrieve).  A nil regexp (no usable keywords at all)
	// yields no candidates without touching the tree.
	prefix := ""
	if a.Path != "" && a.Path != "." {
		prefix = strings.TrimSuffix(filepath.ToSlash(a.Path), "/") + "/"
	}
	byPath := map[string]*candidate{}
	if re != nil {
		grepErr := s.grepResident(re, prefix, a.Glob, func(rel string, _ int, line string, _ []string) {
			c := byPath[rel]
			if c == nil {
				// First matching line is the representative snippet.
				c = &candidate{path: rel, snippet: snippet(line)}
				byPath[rel] = c
			}
			c.count++
		})
		if grepErr != nil {
			return errResult("find_relevant_files: " + grepErr.Error()), nil
		}
	}

	// Rank by match count desc, path asc for a deterministic tie-break.
	candidates := make([]*candidate, 0, len(byPath))
	for _, c := range byPath {
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].count != candidates[j].count {
			return candidates[i].count > candidates[j].count
		}
		return candidates[i].path < candidates[j].path
	})
	considered := len(candidates)
	truncated := false
	if len(candidates) > MaxRerankCandidates {
		candidates = candidates[:MaxRerankCandidates]
		truncated = true
	}

	// Zero candidates: a normal (non-error) result, and NO rerank call — there is
	// nothing to rank.
	if len(candidates) == 0 {
		return relevantResult(a.Question, nil, considered, truncated, effort, &prov,
			"No files matched the question's keywords."), nil
	}

	// 3. Rerank (at the requested effort).  Feed only the candidate snippets.
	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s\n\nCandidate files:\n", a.Question)
	for _, c := range candidates {
		fmt.Fprintf(&b, "%s: %s\n", c.path, c.snippet)
	}
	rankRes, err := s.cfg.Infer.Ask(ctx, effort, []openai.Message{
		{Role: "system", Content: rerankSystemPrompt},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return errResult("inference failed: " + err.Error()), nil
	}
	prov.add(rankRes)

	files := parseRerank(rankRes.Answer, candidates)
	if len(files) == 0 {
		// The model's ranking was unparseable — fall back to the grep-ranked
		// candidates so a garbled answer still returns useful files, never the
		// empty set.
		files = grepFallback(candidates)
	}

	return relevantResult(a.Question, files, considered, truncated, effort, &prov, ""), nil
}

// relevantResult renders the ranked list (or an empty-set message) plus the
// aggregate provenance footer, and mirrors those fields as structured content.
func relevantResult(question string, files []rankedFile, considered int, truncated bool, effort infer.Effort, prov *provenance, emptyMsg string) *mcp.CallToolResult {
	servedBy := prov.servedBy()
	var body string
	if len(files) == 0 {
		body = emptyMsg
	} else {
		var b strings.Builder
		for _, f := range files {
			if f.Reason != "" {
				fmt.Fprintf(&b, "%s — %s\n", f.Path, f.Reason)
			} else {
				fmt.Fprintf(&b, "%s\n", f.Path)
			}
		}
		body = strings.TrimRight(b.String(), "\n")
	}
	r := textResult(body + footerParts(effort, servedBy, prov.elapsed, prov.tokens))
	r.StructuredContent = map[string]any{
		"question":             question,
		"files":                files,
		"candidatesConsidered": considered,
		"truncated":            truncated,
		"servedBy":             servedBy,
		"elapsedMs":            prov.elapsed.Milliseconds(),
		"tokens":               prov.tokens,
		"effort":               string(effort),
	}
	return r
}

// snippet trims a matching line and caps it to relevantSnippetChars, on a rune
// boundary so a multi-byte character is never split.
func snippet(line string) string {
	s := strings.TrimSpace(line)
	if len(s) <= relevantSnippetChars {
		return s
	}
	return strings.ToValidUTF8(s[:relevantSnippetChars], "")
}

// parseKeywords parses the keyword-expansion answer: comma-separated, trimmed,
// lowercased, empties dropped, deduped, capped to MaxKeywords.
func parseKeywords(answer string) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.Split(answer, ",") {
		kw := strings.ToLower(strings.TrimSpace(tok))
		if kw == "" || seen[kw] {
			continue
		}
		seen[kw] = true
		out = append(out, kw)
		if len(out) >= MaxKeywords {
			break
		}
	}
	return out
}

// wordSplit splits on runs of non-alphanumeric characters.
var wordSplit = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// questionKeywords is the fallback when the model yields no usable keywords: the
// question's own significant words (split on non-alphanumeric, tokens < 3 chars
// dropped, lowercased, deduped, capped).
func questionKeywords(question string) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range wordSplit.Split(question, -1) {
		if len(tok) < 3 {
			continue
		}
		kw := strings.ToLower(tok)
		if seen[kw] {
			continue
		}
		seen[kw] = true
		out = append(out, kw)
		if len(out) >= MaxKeywords {
			break
		}
	}
	return out
}

// keywordRegexp builds a case-insensitive alternation over the keywords, each
// QuoteMeta'd so a keyword is matched literally.  It returns nil when there are
// no keywords (the caller then surfaces no candidates).
func keywordRegexp(keywords []string) *regexp.Regexp {
	if len(keywords) == 0 {
		return nil
	}
	quoted := make([]string, len(keywords))
	for i, kw := range keywords {
		quoted[i] = regexp.QuoteMeta(kw)
	}
	// QuoteMeta output plus a fixed alternation always compiles.
	return regexp.MustCompile("(?i)(" + strings.Join(quoted, "|") + ")")
}

// parseRerank robustly parses the reranker's answer into ranked files.  Each
// non-empty line is scanned for a candidate path (longest match wins, so a
// short path that is a prefix of another does not shadow it); hallucinated paths
// — anything not in the candidate set — are dropped, and the reason is whatever
// text follows the path.  Deduped, capped to MaxRelevantFiles.
func parseRerank(answer string, candidates []*candidate) []rankedFile {
	set := map[string]bool{}
	for _, c := range candidates {
		set[c.path] = true
	}
	var out []rankedFile
	seen := map[string]bool{}
	for _, raw := range strings.Split(answer, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		p := longestPathIn(line, set)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		reason := ""
		if idx := strings.Index(line, p); idx >= 0 {
			reason = strings.TrimSpace(line[idx+len(p):])
			// Strip a leading separator (em dash, hyphen, colon, etc.).
			reason = strings.TrimLeft(reason, "—-:·|>* \t")
			reason = strings.TrimSpace(reason)
		}
		out = append(out, rankedFile{Path: p, Reason: reason})
		if len(out) >= MaxRelevantFiles {
			break
		}
	}
	return out
}

// longestPathIn returns the longest path in set that appears as a substring of
// line, or "" if none does.
func longestPathIn(line string, set map[string]bool) string {
	best := ""
	for p := range set {
		if len(p) > len(best) && strings.Contains(line, p) {
			best = p
		}
	}
	return best
}

// grepFallback is the parse-failure fallback: the top grep-ranked candidates
// with a generic reason, so a garbled rerank still returns useful files.
func grepFallback(candidates []*candidate) []rankedFile {
	out := make([]rankedFile, 0, MaxRelevantFiles)
	for _, c := range candidates {
		out = append(out, rankedFile{
			Path:   c.path,
			Reason: fmt.Sprintf("matched %d keyword occurrences", c.count),
		})
		if len(out) >= MaxRelevantFiles {
			break
		}
	}
	return out
}
