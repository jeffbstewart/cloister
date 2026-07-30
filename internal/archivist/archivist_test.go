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

package archivist

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
)

// The tests drive the MCP surface over in-memory transports against a
// real Archive on throwaway repositories (a bare origin plus the
// workspace clone) — the same rig shape as internal/archive's tests,
// one layer up.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

type fixture struct {
	dir     string // the workspace clone
	session *mcp.ClientSession
}

// gitRun runs raw git for rig setup — never the code under test.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{
		"-c", "user.name=Seeder",
		"-c", "user.email=seeder@cloister.test",
		"-c", "protocol.file.allow=always",
		"-c", "commit.gpgsign=false",
	}
	if dir != "" {
		base = append([]string{"-C", dir}, base...)
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rig: git %v: %v\n%s", args, err, out)
	}
	return strings.TrimRight(string(out), "\n")
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireGit(t)
	tmp := t.TempDir()

	origin := filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")
	gitRun(t, "", "init", "--bare", "-b", "main", origin)
	gitRun(t, "", "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "commit", "-m", "seed")
	gitRun(t, seed, "push", origin, "main:main")

	dir := filepath.Join(tmp, "ws")
	gitRun(t, "", "clone", origin, dir)

	// The injected clock: fixed start, one step per read, so recorded
	// times are deterministic.
	var mu sync.Mutex
	at := time.Unix(1_753_000_000, 0).UTC()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(time.Second)
		return at
	}

	a, err := archive.New(dir, archive.Identity{Name: "cloister-bot", Email: "bot@cloister.test"},
		archive.WithClock(clock))
	if err != nil {
		t.Fatalf("archive.New(%s): %v", dir, err)
	}
	t.Cleanup(func() { a.Close() })

	srv := New(Config{Version: "test", Archive: a})
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
	return &fixture{dir: dir, session: session}
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

// ok invokes a tool and fails the test on a tool-level error.
func (f *fixture) ok(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	text, isErr := f.call(t, name, args)
	if isErr {
		t.Fatalf("%s%v returned a tool error: %s", name, args, text)
	}
	return text
}

// asJSON unmarshals a tool's JSON answer.
func asJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("unparseable tool JSON %q: %v", text, err)
	}
	return v
}

func (f *fixture) write(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentStateOnFreshClone(t *testing.T) {
	f := newFixture(t)
	st := asJSON(t, f.ok(t, "current_state", nil))
	if st["branch"] != "main" || st["default"] != "main" {
		t.Errorf("state = %v, want branch and default main", st)
	}
	if n := st["set_aside"].(float64); n != 0 {
		t.Errorf("set_aside = %v on a fresh clone", n)
	}
}

func TestCheckpointFlow(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/flow"})

	f.write(t, "a.txt", "new content\n")
	pend := asJSON(t, f.ok(t, "pending_changes", nil))
	untracked, _ := pend["untracked"].([]any)
	if len(untracked) != 1 || untracked[0] != "a.txt" {
		t.Fatalf("pending untracked = %v, want [a.txt]", pend["untracked"])
	}

	cp := asJSON(t, f.ok(t, "checkpoint", map[string]any{"message": "add a"}))
	id, _ := cp["checkpoint"].(string)
	if len(id) < 4 {
		t.Fatalf("checkpoint id = %q", id)
	}

	hist := asJSON(t, f.ok(t, "history", map[string]any{"limit": 1}))
	changes := hist["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("history = %v, want one change", hist)
	}
	top := changes[0].(map[string]any)
	if top["subject"] != "add a" || top["author"] != "cloister-bot" {
		t.Errorf("top change = %v, want the bot's 'add a'", top)
	}

	shown := asJSON(t, f.ok(t, "show_change", map[string]any{"id": id}))
	if diff, _ := shown["diff"].(string); !strings.Contains(diff, "+new content") {
		t.Errorf("show_change diff missing the added line:\n%v", shown["diff"])
	}

	if got := f.ok(t, "file_at", map[string]any{"ref": "HEAD", "path": "a.txt"}); got != "new content\n" {
		t.Errorf("file_at = %q, want the checkpointed content", got)
	}
}

func TestRestoreAndParcels(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/undo"})

	f.write(t, "README.md", "ruined\n")
	res := asJSON(t, f.ok(t, "restore", map[string]any{"path": "README.md"}))
	if res["rewound"] != false {
		t.Errorf("path restore reported %v, want rewound false", res)
	}
	b, err := os.ReadFile(filepath.Join(f.dir, "README.md"))
	if err != nil || string(b) != "hello\n" {
		t.Errorf("README.md = %q, %v; want the checkpointed content back", b, err)
	}

	f.write(t, "draft.txt", "parked\n")
	f.ok(t, "set_aside", nil)
	st := asJSON(t, f.ok(t, "current_state", nil))
	if n := st["set_aside"].(float64); n != 1 {
		t.Fatalf("set_aside count = %v after parking", n)
	}
	f.ok(t, "resume", nil)
	if b, err := os.ReadFile(filepath.Join(f.dir, "draft.txt")); err != nil || string(b) != "parked\n" {
		t.Errorf("draft.txt = %q, %v after resume; want the parcel back", b, err)
	}
}

func TestSyncFromUpstreamOnDefault(t *testing.T) {
	f := newFixture(t)
	res := asJSON(t, f.ok(t, "sync_from_upstream", nil))
	if res["replayed"] != false || res["merged"] != false {
		t.Errorf("sync on an up-to-date default = %v, want a plain fast-forward answer", res)
	}
}

func TestAbandonWork(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/doomed"})
	text := f.ok(t, "abandon_work", map[string]any{"name": "agent/doomed"})
	if !strings.Contains(text, "main") {
		t.Errorf("abandon answer %q does not name the default branch", text)
	}
	if out := gitRun(t, f.dir, "branch", "--list", "agent/doomed"); out != "" {
		t.Errorf("branch still exists: %q", out)
	}
}

// TestToolErrorsAreResults: failures come back as tool-level errors
// (IsError), never transport errors — and injection-shaped input dies
// at the parser with a "bad arguments" answer.
func TestToolErrorsAreResults(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"dashy branch name", "start_work", map[string]any{"name": "-x"}, "bad arguments"},
		{"checkpoint on default", "checkpoint", map[string]any{"message": "on main"}, "default branch"},
		{"uppercase checkpoint id", "restore", map[string]any{"checkpoint": "ABCD"}, "bad arguments"},
		{"range ref refused", "history", map[string]any{"ref": "main..evil"}, "bad arguments"},
		{"traversal path", "file_at", map[string]any{"ref": "HEAD", "path": "../outside"}, "invalid path"},
		{"resume with nothing parked", "resume", nil, "nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := f.call(t, tc.tool, tc.args)
			if !isErr {
				t.Fatalf("%s%v succeeded: %s", tc.tool, tc.args, text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("%s error %q does not mention %q", tc.tool, text, tc.want)
			}
		})
	}
}
