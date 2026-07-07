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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/repo"
)

type fakeAuditor struct {
	mu   sync.Mutex
	recs []audit.Record
}

func (f *fakeAuditor) Append(r audit.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
	return nil
}

func (f *fakeAuditor) denials() []audit.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audit.Record(nil), f.recs...)
}

type fixture struct {
	dir     string
	aud     *fakeAuditor
	session *mcp.ClientSession
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := repo.New(dir, repo.Config{Budget: 1 << 20, MaxFileSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	aud := &fakeAuditor{}
	srv := New(Config{Version: "test", Repo: rep, Audit: aud})

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

// call invokes a tool, returning the first text content and IsError.
func (f *fixture) call(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallTool(%s): empty content", name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("CallTool(%s): non-text content", name)
	}
	return tc.Text, res.IsError
}

func std(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, map[string]string{
		".gitignore":       "build/\n",
		".aiignore":        "secrets/\n*.secret\n",
		"src/main.go":      "package main\n\nfunc main() {}\n",
		"src/util.go":      "package main\n\nfunc helper() {}\n",
		"docs/notes.txt":   "alpha\nbravo\ncharlie\ndelta\necho\n",
		"secrets/prod.env": "DB_PASSWORD=hunter2\n",
		"build/out.bin":    "artifact",
	})
}

func TestReadFileAndDenials(t *testing.T) {
	f := std(t)
	text, isErr := f.call(t, "read_file", map[string]any{"path": "src/main.go"})
	if isErr || !strings.Contains(text, "func main()") {
		t.Fatalf("read_file = %q, err=%v", text, isErr)
	}

	// Shielded read: refused AND audited with the path.
	text, isErr = f.call(t, "read_file", map[string]any{"path": "secrets/prod.env"})
	if !isErr || !strings.Contains(text, "denied") {
		t.Fatalf("shielded read = %q, err=%v; want denial", text, isErr)
	}
	if strings.Contains(text, "hunter2") {
		t.Fatal("denial leaked content")
	}
	recs := f.aud.denials()
	if len(recs) != 1 || recs[0].Decision != audit.DecisionReadDenied ||
		recs[0].Read == nil || recs[0].Read.Paths[0] != "secrets/prod.env" || recs[0].Tool != "read_file" {
		t.Fatalf("denial audit = %+v", recs)
	}

	// Hidden read: refused and audited too.
	if _, isErr := f.call(t, "read_file", map[string]any{"path": "build/out.bin"}); !isErr {
		t.Fatal("hidden read served")
	}
	if got := len(f.aud.denials()); got != 2 {
		t.Fatalf("denial records = %d, want 2", got)
	}

	// A merely missing file is an error but NOT a denial record.
	if _, isErr := f.call(t, "read_file", map[string]any{"path": "no/such.go"}); !isErr {
		t.Fatal("missing read served")
	}
	if got := len(f.aud.denials()); got != 2 {
		t.Fatalf("missing file wrongly audited; records = %d", got)
	}
}

func TestReadRangeHeadTail(t *testing.T) {
	f := std(t)
	var out struct {
		FromLine, ToLine, TotalLines int
		Content                      string
	}
	text, isErr := f.call(t, "read_range", map[string]any{"path": "docs/notes.txt", "start": 2, "end": 4})
	if isErr {
		t.Fatal(text)
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "bravo\ncharlie\ndelta" || out.TotalLines != 5 {
		t.Fatalf("read_range = %+v", out)
	}

	text, _ = f.call(t, "read_head", map[string]any{"path": "docs/notes.txt", "n": 2})
	_ = json.Unmarshal([]byte(text), &out)
	if out.Content != "alpha\nbravo" {
		t.Fatalf("read_head = %+v", out)
	}

	text, _ = f.call(t, "read_tail", map[string]any{"path": "docs/notes.txt", "n": 2})
	_ = json.Unmarshal([]byte(text), &out)
	if out.Content != "delta\necho" {
		t.Fatalf("read_tail = %+v", out)
	}

	// Range clamps past EOF rather than erroring.
	text, isErr = f.call(t, "read_range", map[string]any{"path": "docs/notes.txt", "start": 4, "end": 99})
	if isErr {
		t.Fatal(text)
	}
	_ = json.Unmarshal([]byte(text), &out)
	if out.Content != "delta\necho" || out.ToLine != 5 {
		t.Fatalf("clamped range = %+v", out)
	}
}

func TestBatchReadOneDenialRecordManyPaths(t *testing.T) {
	f := newFixture(t, map[string]string{
		".aiignore": "*.key\n",
		"a.txt":     "A",
		"one.key":   "k1",
		"two.key":   "k2",
	})
	text, isErr := f.call(t, "batch_read", map[string]any{"paths": []string{"a.txt", "one.key", "two.key", "missing.txt"}})
	if isErr {
		t.Fatal(text)
	}
	if !strings.Contains(text, `"content": "A"`) || strings.Contains(text, "k1") {
		t.Fatalf("batch content wrong: %s", text)
	}
	recs := f.aud.denials()
	if len(recs) != 1 {
		t.Fatalf("want ONE denial record for the batch, got %d", len(recs))
	}
	if r := recs[0].Read; r == nil || len(r.Paths) != 2 || r.Paths[0] != "one.key" || r.Paths[1] != "two.key" {
		t.Fatalf("batch denial paths = %+v", recs[0])
	}
}

func TestListingsPermsAndVisibility(t *testing.T) {
	f := std(t)
	text, isErr := f.call(t, "list_dir", map[string]any{"path": "."})
	if isErr {
		t.Fatal(text)
	}
	if strings.Contains(text, "build") {
		t.Error("hidden dir listed")
	}
	if !strings.Contains(text, `"secrets"`) && !strings.Contains(text, "secrets") {
		t.Error("stripped dir missing from listing")
	}
	if !strings.Contains(text, "d---------") {
		t.Error("stripped dir perms not stripped")
	}
	if strings.Contains(text, "w") && strings.Contains(text, "-rw") {
		t.Error("a write bit leaked into perms")
	}
	if !strings.Contains(text, "-r--r--r--") || !strings.Contains(text, "dr-xr-xr-x") {
		t.Errorf("visible perms wrong: %s", text)
	}

	// stat of a stripped file is served (names are visible), not audited.
	before := len(f.aud.denials())
	text, isErr = f.call(t, "stat_file", map[string]any{"path": "secrets/prod.env"})
	if isErr || !strings.Contains(text, "----------") {
		t.Fatalf("stat stripped = %q err=%v", text, isErr)
	}
	if len(f.aud.denials()) != before {
		t.Error("metadata stat wrongly audited as denial")
	}
}

func TestTreeDepthAndGlob(t *testing.T) {
	f := newFixture(t, map[string]string{
		"a/b/c/deep.txt": "x",
		"a/top.txt":      "y",
		"root.go":        "z",
	})
	text, _ := f.call(t, "tree", map[string]any{"path": "a", "depth": 1})
	if strings.Contains(text, "deep.txt") || !strings.Contains(text, "top.txt") {
		t.Fatalf("tree depth wrong: %s", text)
	}

	text, _ = f.call(t, "glob", map[string]any{"pattern": "**/*.txt"})
	if !strings.Contains(text, "a/b/c/deep.txt") || !strings.Contains(text, "a/top.txt") || strings.Contains(text, "root.go") {
		t.Fatalf("glob ** wrong: %s", text)
	}
	text, _ = f.call(t, "glob", map[string]any{"pattern": "*.go"})
	if !strings.Contains(text, "root.go") || strings.Contains(text, "deep") {
		t.Fatalf("anchored glob wrong: %s", text)
	}
}

func TestSearchModesAndShieldedContentUnfindable(t *testing.T) {
	f := std(t)

	// The shielded secret must be structurally unfindable.
	text, _ := f.call(t, "search", map[string]any{"pattern": "hunter2"})
	if !strings.Contains(text, `"totalMatches": 0`) {
		t.Fatalf("shielded content was findable: %s", text)
	}

	text, _ = f.call(t, "search", map[string]any{"pattern": "func \\w+", "mode": "files"})
	if !strings.Contains(text, "src/main.go") || !strings.Contains(text, "src/util.go") {
		t.Fatalf("files mode: %s", text)
	}

	text, _ = f.call(t, "search", map[string]any{"pattern": "func", "mode": "count"})
	if !strings.Contains(text, `"src/main.go": 1`) {
		t.Fatalf("count mode: %s", text)
	}

	text, _ = f.call(t, "search", map[string]any{"pattern": "bravo", "mode": "context", "before": 1, "after": 1})
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "charlie") {
		t.Fatalf("context lines missing: %s", text)
	}

	text, _ = f.call(t, "search", map[string]any{"pattern": "func", "glob": "**/util.go", "mode": "total"})
	if !strings.Contains(text, `"totalMatches": 1`) {
		t.Fatalf("glob filter: %s", text)
	}
}

func TestRecentlyModified(t *testing.T) {
	f := std(t)
	text, isErr := f.call(t, "recently_modified", map[string]any{"limit": 3})
	if isErr {
		t.Fatal(text)
	}
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 3 {
		t.Fatalf("limit not applied: %d entries", len(out.Entries))
	}
	for _, e := range out.Entries {
		if e["dir"] == true {
			t.Error("directory in recently_modified")
		}
	}
}
