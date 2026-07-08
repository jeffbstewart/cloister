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

package librarian

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/infer"
	"github.com/jeffbstewart/cloister/internal/openai"
	"github.com/jeffbstewart/cloister/internal/repo"
)

// fakeInferencer is the inference seam under test: it records every call and
// returns a canned Result or error, with no real HTTP.  For the map-reduce
// directory op it derives a per-call Result from the request (so each map call
// gets a distinct summary and the reduce sees them) when res is left zero.
type fakeInferencer struct {
	res infer.Result
	// scripted, when non-empty, is drained one Result per call in order — so a
	// multi-call op (keyword-expand then rerank) can return a distinct answer per
	// stage.  It takes precedence over res.
	scripted []infer.Result
	err      error
	calls    int
	effort   infer.Effort // effort of the LAST call (single-file ops assert this)
	msgs     []openai.Message
	// recorded captures EVERY call in order, for the map-reduce assertions.
	recorded []recordedCall
}

type recordedCall struct {
	effort infer.Effort
	msgs   []openai.Message
}

func (f *fakeInferencer) Ask(_ context.Context, effort infer.Effort, msgs []openai.Message) (infer.Result, error) {
	f.calls++
	f.effort = effort
	f.msgs = msgs
	f.recorded = append(f.recorded, recordedCall{effort: effort, msgs: msgs})
	if f.err != nil {
		return infer.Result{}, f.err
	}
	// Scripted answers win: return the next one in order, so a test can hand call
	// 1 a keyword list and call 2 a ranking.
	if len(f.scripted) > 0 {
		res := f.scripted[0]
		f.scripted = f.scripted[1:]
		return res, nil
	}
	// A zero-value canned Result means "derive a per-call answer" — the last
	// user message truncated to a short digest — so map calls stay distinct and
	// carry a token so aggregation is testable.  A non-zero res is returned
	// verbatim (the single-file tests rely on the exact fields).
	if f.res == (infer.Result{}) {
		user := msgs[len(msgs)-1].Content
		if len(user) > 20 {
			user = user[:20]
		}
		return infer.Result{Answer: "digest:" + user, ServedBy: "think-fast", Tokens: 1}, nil
	}
	return f.res, nil
}

// comprehendFixture is the standard fixture wired with a fake Inferencer, so
// the comprehension tools register.  It mirrors newFixture's MCP plumbing.
func comprehendFixture(t *testing.T, files map[string]string, inf Inferencer) *fixture {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, files)
	rep, err := repo.New(dir, repo.Config{Budget: 1 << 20, MaxFileSize: 512 << 10})
	if err != nil {
		t.Fatal(err)
	}
	aud := &fakeAuditor{}
	srv := New(Config{Version: "test", Repo: rep, Audit: aud, Infer: inf})

	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.mcp.Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return &fixture{dir: dir, aud: aud, session: session}
}

func TestComprehendHappyPathAndFooter(t *testing.T) {
	inf := &fakeInferencer{res: infer.Result{
		Answer:   "It defines main and helper functions.",
		ServedBy: "think-fast",
		Elapsed:  1500 * time.Millisecond,
		Tokens:   4242,
	}}
	f := comprehendFixture(t, map[string]string{"src/main.go": "package main\n\nfunc main() {}\n"}, inf)

	text, isErr := f.call(t, "ask_about_file", map[string]any{
		"path": "src/main.go", "question": "what does this do?",
	})
	if isErr {
		t.Fatalf("ask_about_file errored: %s", text)
	}
	if !strings.Contains(text, "It defines main and helper functions.") {
		t.Fatalf("answer missing: %s", text)
	}
	// Footer carries servedBy, effort, and tokens.
	if !strings.Contains(text, "think-fast") || !strings.Contains(text, "quick") ||
		!strings.Contains(text, "4242 tok") || !strings.Contains(text, "1.5s") {
		t.Fatalf("footer wrong: %s", text)
	}
	if inf.calls != 1 || inf.effort != infer.Quick {
		t.Fatalf("infer calls=%d effort=%q, want 1/quick", inf.calls, inf.effort)
	}
	// The pushed prompt embeds the path, content, and question.
	user := inf.msgs[len(inf.msgs)-1].Content
	if !strings.Contains(user, "src/main.go") || !strings.Contains(user, "func main()") ||
		!strings.Contains(user, "what does this do?") {
		t.Fatalf("prompt missing path/content/question: %s", user)
	}
}

func TestComprehendEffortDefaultAndRejection(t *testing.T) {
	inf := &fakeInferencer{res: infer.Result{Answer: "ok", ServedBy: "deep-think", Tokens: 1}}
	f := comprehendFixture(t, map[string]string{"a.txt": "hello\n"}, inf)

	// Explicit thorough is honored.
	if _, isErr := f.call(t, "summarize_file", map[string]any{"path": "a.txt", "effort": "thorough"}); isErr {
		t.Fatal("thorough summarize errored")
	}
	if inf.effort != infer.Thorough {
		t.Fatalf("effort = %q, want thorough", inf.effort)
	}

	// Empty effort defaults to quick.
	if _, isErr := f.call(t, "ask_about_file", map[string]any{"path": "a.txt", "question": "?"}); isErr {
		t.Fatal("default-effort ask errored")
	}
	if inf.effort != infer.Quick {
		t.Fatalf("default effort = %q, want quick", inf.effort)
	}

	// An unknown effort is rejected before any inference call.
	before := inf.calls
	text, isErr := f.call(t, "ask_about_file", map[string]any{"path": "a.txt", "question": "?", "effort": "medium"})
	if !isErr || !strings.Contains(text, "effort must be") {
		t.Fatalf("bad effort not rejected: %q err=%v", text, isErr)
	}
	if inf.calls != before {
		t.Fatal("inference called on an invalid effort")
	}
}

func TestComprehendSizeGuard(t *testing.T) {
	big := strings.Repeat("x", MaxComprehendBytes+1)
	inf := &fakeInferencer{res: infer.Result{Answer: "unused"}}
	f := comprehendFixture(t, map[string]string{"big.txt": big}, inf)

	text, isErr := f.call(t, "summarize_file", map[string]any{"path": "big.txt"})
	if !isErr {
		t.Fatalf("oversized file not refused: %s", text)
	}
	if !strings.Contains(text, "comprehension cap") || !strings.Contains(text, "line range") {
		t.Fatalf("refusal does not name the cap/alternative: %s", text)
	}
	if inf.calls != 0 {
		t.Fatal("inference called on an oversized file")
	}
}

func TestComprehendLineRange(t *testing.T) {
	inf := &fakeInferencer{res: infer.Result{Answer: "ok", ServedBy: "think-fast", Tokens: 1}}
	f := comprehendFixture(t, map[string]string{
		"multi.txt": "line one\nline two\nline three\nline four\n",
	}, inf)

	if _, isErr := f.call(t, "ask_about_file", map[string]any{
		"path": "multi.txt", "question": "?", "start": 2, "end": 3,
	}); isErr {
		t.Fatal("ranged ask errored")
	}
	user := inf.msgs[len(inf.msgs)-1].Content
	if !strings.Contains(user, "line two") || !strings.Contains(user, "line three") {
		t.Fatalf("range missing requested lines: %s", user)
	}
	if strings.Contains(user, "line one") || strings.Contains(user, "line four") {
		t.Fatalf("range leaked lines outside 2-3: %s", user)
	}
	if !strings.Contains(user, "lines 2-3") {
		t.Fatalf("prompt missing the range label: %s", user)
	}
}

// TestComprehendRangeBringsOversizedUnderCap is the gap the review raised: a
// file too large to comprehend whole is still reachable a range at a time,
// without spilling its bytes into the caller's context.
func TestComprehendRangeBringsOversizedUnderCap(t *testing.T) {
	big := strings.Repeat("x", MaxComprehendBytes+10) + "\nsmall tail line\n"
	inf := &fakeInferencer{res: infer.Result{Answer: "ok", ServedBy: "think-fast", Tokens: 1}}
	f := comprehendFixture(t, map[string]string{"big.txt": big}, inf)

	// Whole-file refuses...
	if _, isErr := f.call(t, "summarize_file", map[string]any{"path": "big.txt"}); !isErr {
		t.Fatal("oversized whole-file not refused")
	}
	// ...but the small in-range slice comprehends fine.
	if _, isErr := f.call(t, "summarize_file", map[string]any{"path": "big.txt", "start": 2, "end": 2}); isErr {
		t.Fatal("in-range summarize refused")
	}
	if inf.calls != 1 {
		t.Fatalf("infer calls = %d, want 1 (only the in-range call)", inf.calls)
	}
}

func TestComprehendShieldDenialPassthrough(t *testing.T) {
	inf := &fakeInferencer{res: infer.Result{Answer: "unused"}}
	f := comprehendFixture(t, map[string]string{
		".aiignore":        "secrets/\n",
		"secrets/prod.env": "DB_PASSWORD=hunter2\n",
	}, inf)

	text, isErr := f.call(t, "ask_about_file", map[string]any{"path": "secrets/prod.env", "question": "?"})
	if !isErr || !strings.Contains(text, "denied") {
		t.Fatalf("shielded comprehension = %q err=%v; want denial", text, isErr)
	}
	if strings.Contains(text, "hunter2") {
		t.Fatal("denial leaked content")
	}
	if inf.calls != 0 {
		t.Fatal("inference called on a denied file")
	}
	// The denial is audited under the tool name.
	recs := f.aud.denials()
	if len(recs) != 1 || recs[0].Tool != "ask_about_file" || recs[0].Read() == nil ||
		recs[0].Read().Paths[0] != "secrets/prod.env" {
		t.Fatalf("denial audit = %+v", recs)
	}
}

func TestComprehendInferenceError(t *testing.T) {
	inf := &fakeInferencer{err: errors.New("engine unreachable")}
	f := comprehendFixture(t, map[string]string{"a.txt": "hi\n"}, inf)

	text, isErr := f.call(t, "ask_about_file", map[string]any{"path": "a.txt", "question": "?"})
	if !isErr || !strings.Contains(text, "inference failed") {
		t.Fatalf("inference error = %q err=%v; want errResult", text, isErr)
	}
	// An inference failure is a normal error, not a denial: nothing audited.
	if got := len(f.aud.denials()); got != 0 {
		t.Fatalf("inference error wrongly audited; records = %d", got)
	}
}

func TestSummarizeDirectoryMapReduce(t *testing.T) {
	inf := &fakeInferencer{} // zero res: per-call derived digests
	f := comprehendFixture(t, map[string]string{
		"pkg/a.go":   "package pkg\n\nfunc A() {}\n",
		"pkg/b.go":   "package pkg\n\nfunc B() {}\n",
		"pkg/c.go":   "package pkg\n\nfunc C() {}\n",
		"other/z.go": "package other\n",
	}, inf)

	text, isErr := f.call(t, "summarize_directory", map[string]any{"path": "pkg", "effort": "thorough"})
	if isErr {
		t.Fatalf("summarize_directory errored: %s", text)
	}
	// 3 map calls at quick, then 1 reduce at the requested thorough.
	if inf.calls != 4 {
		t.Fatalf("infer calls = %d, want 4 (3 map + 1 reduce)", inf.calls)
	}
	wantEfforts := []infer.Effort{infer.Quick, infer.Quick, infer.Quick, infer.Thorough}
	for i, want := range wantEfforts {
		if inf.recorded[i].effort != want {
			t.Fatalf("call %d effort = %q, want %q", i, inf.recorded[i].effort, want)
		}
	}
	// The reduce sees only files under pkg, never other/z.go.
	reduce := inf.recorded[3].msgs[len(inf.recorded[3].msgs)-1].Content
	if !strings.Contains(reduce, "pkg/a.go") || strings.Contains(reduce, "other/z.go") {
		t.Fatalf("reduce prompt scoped wrong: %s", reduce)
	}
	// Output carries the overview and an aggregate footer (4 calls × 1 tok).
	if !strings.Contains(text, "digest:") || !strings.Contains(text, "4 tok") ||
		!strings.Contains(text, "think-fast") {
		t.Fatalf("overview/footer wrong: %s", text)
	}
}

func TestSummarizeDirectoryBudgetGuard(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < MaxDirFiles+1; i++ {
		files[fmt.Sprintf("many/f%02d.txt", i)] = "x\n"
	}
	inf := &fakeInferencer{}
	f := comprehendFixture(t, files, inf)

	text, isErr := f.call(t, "summarize_directory", map[string]any{"path": "many"})
	if !isErr {
		t.Fatalf("oversized directory not refused: %s", text)
	}
	if !strings.Contains(text, "readable files") || !strings.Contains(text, "narrower") {
		t.Fatalf("refusal does not name the limit/alternative: %s", text)
	}
	if inf.calls != 0 {
		t.Fatalf("inference called despite the budget guard: calls = %d", inf.calls)
	}
}

func TestSummarizeDirectoryPerFileTruncation(t *testing.T) {
	big := strings.Repeat("x", MaxComprehendBytes+10) + "\ntail\n"
	inf := &fakeInferencer{}
	f := comprehendFixture(t, map[string]string{"d/big.txt": big}, inf)

	text, isErr := f.call(t, "summarize_directory", map[string]any{"path": "d"})
	if isErr {
		t.Fatalf("summarize_directory errored: %s", text)
	}
	// The oversized file is still summarized (map call happens) and marked
	// truncated in its map prompt.
	if inf.calls != 2 {
		t.Fatalf("infer calls = %d, want 2 (1 map + 1 reduce)", inf.calls)
	}
	mapPrompt := inf.recorded[0].msgs[len(inf.recorded[0].msgs)-1].Content
	if !strings.Contains(mapPrompt, "(truncated)") {
		t.Fatalf("map prompt not marked truncated: %.80s", mapPrompt)
	}
	// The pushed content is capped at MaxComprehendBytes, not the full file.
	if len(mapPrompt) > MaxComprehendBytes+200 {
		t.Fatalf("truncated map prompt too large: %d bytes", len(mapPrompt))
	}
}

func TestSummarizeDirectoryJailedExclusion(t *testing.T) {
	inf := &fakeInferencer{}
	f := comprehendFixture(t, map[string]string{
		".aiignore":     "*.secret\n",
		"d/keep.txt":    "keepme\n",
		"d/skip.secret": "DB_PASSWORD=hunter2\n",
	}, inf)

	text, isErr := f.call(t, "summarize_directory", map[string]any{"path": "d"})
	if isErr {
		t.Fatalf("summarize_directory errored: %s", text)
	}
	// Only keep.txt is summarized: 1 map + 1 reduce.  The jailed file never
	// reaches the engine (it is not in ForEachResident).
	if inf.calls != 2 {
		t.Fatalf("infer calls = %d, want 2 (jailed file excluded)", inf.calls)
	}
	for _, rc := range inf.recorded {
		user := rc.msgs[len(rc.msgs)-1].Content
		if strings.Contains(user, "skip.secret") || strings.Contains(user, "hunter2") {
			t.Fatalf("jailed file leaked into a prompt: %s", user)
		}
	}
}

func TestSummarizeDirectoryEmptyAndNonDir(t *testing.T) {
	inf := &fakeInferencer{}
	f := comprehendFixture(t, map[string]string{
		".aiignore":       "*.secret\n",
		"lonely/a.secret": "nothing readable here\n",
		"solo.txt":        "just a file\n",
	}, inf)

	// A directory whose only child is jailed has no resident files → errResult.
	text, isErr := f.call(t, "summarize_directory", map[string]any{"path": "lonely"})
	if !isErr || !strings.Contains(text, "no readable files") {
		t.Fatalf("empty dir = %q err=%v; want no-readable-files errResult", text, isErr)
	}
	// A non-directory path → errResult, not a summary.
	text, isErr = f.call(t, "summarize_directory", map[string]any{"path": "solo.txt"})
	if !isErr || !strings.Contains(text, "not a directory") {
		t.Fatalf("non-dir = %q err=%v; want not-a-directory errResult", text, isErr)
	}
	if inf.calls != 0 {
		t.Fatalf("inference called on an empty/non-dir target: calls = %d", inf.calls)
	}
}

func TestComprehensionToolsUnregisteredWithoutInfer(t *testing.T) {
	// No Infer: the mechanical tools register, the comprehension tools do not.
	f := std(t)
	tools, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if !names["read_file"] {
		t.Fatal("mechanical read_file missing")
	}
	if names["ask_about_file"] || names["summarize_file"] {
		t.Fatal("comprehension tools registered without an Inferencer")
	}
	if names["find_relevant_files"] {
		t.Fatal("find_relevant_files registered without an Inferencer")
	}
}

func TestFindRelevantFilesHappyPath(t *testing.T) {
	inf := &fakeInferencer{scripted: []infer.Result{
		// 1. keyword expansion (quick).
		{Answer: "retry, backoff, attempt", ServedBy: "think-fast", Tokens: 3},
		// 2. rerank (thorough): retry.go ahead of client.go.
		{Answer: "internal/net/retry.go — the retry loop lives here\ninternal/net/client.go — calls retry",
			ServedBy: "deep-think", Tokens: 7},
	}}
	f := comprehendFixture(t, map[string]string{
		"internal/net/retry.go":  "package net\n\n// retry loop with backoff\nfunc retry() {}\n",
		"internal/net/client.go": "package net\n\nfunc call() { retry() }\n",
		"internal/net/parse.go":  "package net\n\nfunc parse() {}\n",
	}, inf)

	text, isErr := f.call(t, "find_relevant_files", map[string]any{
		"question": "where is retry handled?", "effort": "thorough",
	})
	if isErr {
		t.Fatalf("find_relevant_files errored: %s", text)
	}
	// Both expected paths present, retry.go ranked ahead of client.go.
	ri := strings.Index(text, "internal/net/retry.go")
	ci := strings.Index(text, "internal/net/client.go")
	if ri < 0 || ci < 0 || ri > ci {
		t.Fatalf("ranked order wrong (retry=%d client=%d): %s", ri, ci, text)
	}
	// parse.go matched no keyword → never a candidate.
	if strings.Contains(text, "parse.go") {
		t.Fatalf("non-matching file surfaced: %s", text)
	}
	// Effort sequence: quick (keyword-expand) then the requested thorough (rerank).
	if len(inf.recorded) != 2 {
		t.Fatalf("infer calls = %d, want 2", len(inf.recorded))
	}
	if inf.recorded[0].effort != infer.Quick || inf.recorded[1].effort != infer.Thorough {
		t.Fatalf("effort sequence = %q,%q; want quick,thorough",
			inf.recorded[0].effort, inf.recorded[1].effort)
	}
	// Footer sums tokens across both calls (3 + 7) and names both engines.
	if !strings.Contains(text, "10 tok") {
		t.Fatalf("footer token sum wrong: %s", text)
	}
	if !strings.Contains(text, "deep-think") || !strings.Contains(text, "think-fast") {
		t.Fatalf("footer engine set wrong: %s", text)
	}
}

func TestFindRelevantFilesHallucinationGuard(t *testing.T) {
	inf := &fakeInferencer{scripted: []infer.Result{
		{Answer: "widget", ServedBy: "think-fast", Tokens: 1},
		// The reranker invents a path that was never a candidate.
		{Answer: "nonexistent/ghost.go — hallucinated\nsrc/widget.go — the real match",
			ServedBy: "think-fast", Tokens: 1},
	}}
	f := comprehendFixture(t, map[string]string{
		"src/widget.go": "package src\n\nfunc widget() {}\n",
	}, inf)

	text, isErr := f.call(t, "find_relevant_files", map[string]any{"question": "widget?"})
	if isErr {
		t.Fatalf("find_relevant_files errored: %s", text)
	}
	if strings.Contains(text, "ghost.go") {
		t.Fatalf("hallucinated path not dropped: %s", text)
	}
	if !strings.Contains(text, "src/widget.go") {
		t.Fatalf("real candidate missing: %s", text)
	}
}

func TestFindRelevantFilesParseFailureFallback(t *testing.T) {
	inf := &fakeInferencer{scripted: []infer.Result{
		{Answer: "alpha", ServedBy: "think-fast", Tokens: 1},
		// Unparseable: names no candidate path at all.
		{Answer: "I could not determine which files are relevant.", ServedBy: "think-fast", Tokens: 1},
	}}
	f := comprehendFixture(t, map[string]string{
		"a/alpha.go": "package a\n// alpha alpha\n",
		"b/also.go":  "package b\n// alpha\n",
	}, inf)

	text, isErr := f.call(t, "find_relevant_files", map[string]any{"question": "alpha?"})
	if isErr {
		t.Fatalf("find_relevant_files errored: %s", text)
	}
	// Falls back to the grep-ranked candidates with the generic reason.
	if !strings.Contains(text, "a/alpha.go") || !strings.Contains(text, "keyword occurrences") {
		t.Fatalf("did not fall back to grep ranking: %s", text)
	}
}

func TestFindRelevantFilesNoCandidates(t *testing.T) {
	inf := &fakeInferencer{scripted: []infer.Result{
		{Answer: "zebra, quokka", ServedBy: "think-fast", Tokens: 1},
	}}
	f := comprehendFixture(t, map[string]string{
		"src/main.go": "package main\n\nfunc main() {}\n",
	}, inf)

	text, isErr := f.call(t, "find_relevant_files", map[string]any{"question": "where are the zebras?"})
	if isErr {
		t.Fatalf("no-candidates should be a normal result: %s", text)
	}
	if !strings.Contains(text, "No files matched") {
		t.Fatalf("want no-files-matched message: %s", text)
	}
	// Only the keyword call happened; the rerank did NOT.
	if inf.calls != 1 {
		t.Fatalf("infer calls = %d, want 1 (no rerank on zero candidates)", inf.calls)
	}
}

func TestFindRelevantFilesScope(t *testing.T) {
	// path prefix: lib/ is excluded from the candidate set entirely.
	pathInf := &fakeInferencer{scripted: []infer.Result{
		{Answer: "config", ServedBy: "think-fast", Tokens: 1},
		{Answer: "app/config.go — the match", ServedBy: "think-fast", Tokens: 1},
	}}
	pf := comprehendFixture(t, map[string]string{
		"app/config.go": "package app\n// config loader\n",
		"lib/config.go": "package lib\n// config helper\n",
	}, pathInf)
	text, isErr := pf.call(t, "find_relevant_files", map[string]any{"question": "config?", "path": "app"})
	if isErr {
		t.Fatalf("scoped find errored: %s", text)
	}
	if strings.Contains(text, "lib/config.go") {
		t.Fatalf("path scope leaked lib/: %s", text)
	}
	if !strings.Contains(text, "app/config.go") {
		t.Fatalf("scoped file missing: %s", text)
	}

	// glob filter: only *.go under app, so app/notes.txt is excluded.
	globInf := &fakeInferencer{scripted: []infer.Result{
		{Answer: "config", ServedBy: "think-fast", Tokens: 1},
		{Answer: "app/config.go — the match", ServedBy: "think-fast", Tokens: 1},
	}}
	gf := comprehendFixture(t, map[string]string{
		"app/config.go":  "package app\n// config loader\n",
		"app/config.txt": "config notes\n",
	}, globInf)
	text, isErr = gf.call(t, "find_relevant_files", map[string]any{"question": "config?", "glob": "app/*.go"})
	if isErr {
		t.Fatalf("glob-scoped find errored: %s", text)
	}
	if strings.Contains(text, "config.txt") {
		t.Fatalf("glob scope leaked config.txt: %s", text)
	}
	if !strings.Contains(text, "app/config.go") {
		t.Fatalf("glob-scoped file missing: %s", text)
	}
}

func TestFindRelevantFilesJailedExclusion(t *testing.T) {
	inf := &fakeInferencer{scripted: []infer.Result{
		{Answer: "password, secret", ServedBy: "think-fast", Tokens: 1},
		{Answer: "keep.txt — mentions the password policy", ServedBy: "think-fast", Tokens: 1},
	}}
	f := comprehendFixture(t, map[string]string{
		".aiignore":   "*.secret\n",
		"keep.txt":    "the password policy lives here\n",
		"leak.secret": "password=hunter2 secret\n",
	}, inf)

	text, isErr := f.call(t, "find_relevant_files", map[string]any{"question": "where is the password?"})
	if isErr {
		t.Fatalf("find_relevant_files errored: %s", text)
	}
	if strings.Contains(text, "leak.secret") || strings.Contains(text, "hunter2") {
		t.Fatalf("jailed file surfaced: %s", text)
	}
	// The jailed file's content never reached the rerank prompt either.
	for _, rc := range inf.recorded {
		user := rc.msgs[len(rc.msgs)-1].Content
		if strings.Contains(user, "leak.secret") || strings.Contains(user, "hunter2") {
			t.Fatalf("jailed file leaked into a prompt: %s", user)
		}
	}
}
