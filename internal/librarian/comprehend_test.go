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
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/infer"
	"github.com/jeffbstewart/cloister/internal/openai"
	"github.com/jeffbstewart/cloister/internal/repo"
)

// fakeInferencer is the inference seam under test: it records the call and
// returns a canned Result or error, with no real HTTP.
type fakeInferencer struct {
	res    infer.Result
	err    error
	calls  int
	effort infer.Effort
	msgs   []openai.Message
}

func (f *fakeInferencer) Ask(_ context.Context, effort infer.Effort, msgs []openai.Message) (infer.Result, error) {
	f.calls++
	f.effort = effort
	f.msgs = msgs
	if f.err != nil {
		return infer.Result{}, f.err
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
	if !strings.Contains(text, "comprehension cap") || !strings.Contains(text, "read_range") {
		t.Fatalf("refusal does not name the cap/alternative: %s", text)
	}
	if inf.calls != 0 {
		t.Fatal("inference called on an oversized file")
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
}
