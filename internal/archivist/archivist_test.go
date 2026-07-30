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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/archive/archivetest"
)

// The tests drive the MCP surface over in-memory transports against a
// real Archive on archivetest's seeded repositories.  The engine's
// behavior is proven by internal/archive's own suite; what this layer
// asserts is the surface — wiring, answer shapes, and refusals arriving
// as tool-level errors.

type fixture struct {
	tmp     string // the rig root; origin.git lives here
	dir     string // the workspace clone
	session *mcp.ClientSession
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, Config{Version: "test"})
}

// newFixtureWith builds the rig with the caller's Config; the Archive
// field is filled in here.
func newFixtureWith(t *testing.T, cfg Config) *fixture {
	t.Helper()
	tmp := t.TempDir()
	_, dir := archivetest.Seed(t, tmp)

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

	a, err := archive.New(dir, archive.WithIdentity(archive.Identity{Name: "cloister-bot", Email: "bot@cloister.test"}),
		archive.WithClock(clock))
	if err != nil {
		t.Fatalf("archive.New(%s): %v", dir, err)
	}
	t.Cleanup(func() { a.Close() })

	cfg.Archive = a
	srv := New(cfg)
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
	return &fixture{tmp: tmp, dir: dir, session: session}
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

// field returns a typed field of a tool's JSON answer, failing with the
// key name rather than panicking when the shape drifts.
func field[T any](t *testing.T, m map[string]any, key string) T {
	t.Helper()
	v, ok := m[key].(T)
	if !ok {
		t.Fatalf("answer field %q = %v (%T), want %T", key, m[key], m[key], v)
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
	if n := field[float64](t, st, "set_aside"); n != 0 {
		t.Errorf("set_aside = %v on a fresh clone", n)
	}
	// A clean tree answers empty arrays, never null.
	if u, ok := st["untracked"].([]any); !ok || len(u) != 0 {
		t.Errorf("untracked = %v (%T), want an empty array", st["untracked"], st["untracked"])
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
	id := field[string](t, cp, "checkpoint")
	if len(id) < 4 {
		t.Fatalf("checkpoint id = %q", id)
	}

	hist := asJSON(t, f.ok(t, "history", map[string]any{"limit": 1}))
	changes := field[[]any](t, hist, "changes")
	if len(changes) != 1 {
		t.Fatalf("history = %v, want one change", hist)
	}
	top, ok := changes[0].(map[string]any)
	if !ok {
		t.Fatalf("change entry = %v (%T), want an object", changes[0], changes[0])
	}
	if top["subject"] != "add a" || top["author"] != "cloister-bot" {
		t.Errorf("top change = %v, want the bot's 'add a'", top)
	}

	shown := asJSON(t, f.ok(t, "show_change", map[string]any{"id": id}))
	if diff := field[string](t, shown, "diff"); !strings.Contains(diff, "+new content") {
		t.Errorf("show_change diff missing the added line:\n%s", diff)
	}

	if got := f.ok(t, "file_at", map[string]any{"ref": "HEAD", "path": "a.txt"}); got != "new content\n" {
		t.Errorf("file_at = %q, want the checkpointed content", got)
	}
}

// TestRestoreShapes: every restore answer names the action taken, and
// the destructive whole-tree discard exists only behind an explicit
// all: true.
func TestRestoreShapes(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/undo"})

	f.write(t, "README.md", "ruined\n")
	res := asJSON(t, f.ok(t, "restore", map[string]any{"path": "README.md"}))
	if got := field[string](t, res, "action"); got != "file_restored" {
		t.Errorf("path restore action = %q, want file_restored", got)
	}

	f.write(t, "README.md", "ruined again\n")
	res = asJSON(t, f.ok(t, "restore", map[string]any{"all": true}))
	if got := field[string](t, res, "action"); got != "discarded_local_edits" {
		t.Errorf("all restore action = %q, want discarded_local_edits", got)
	}

	f.write(t, "a.txt", "one\n")
	cp := asJSON(t, f.ok(t, "checkpoint", map[string]any{"message": "v1"}))
	first := field[string](t, cp, "checkpoint")
	f.write(t, "a.txt", "two\n")
	f.ok(t, "checkpoint", map[string]any{"message": "v2"})
	res = asJSON(t, f.ok(t, "restore", map[string]any{"checkpoint": first}))
	if got := field[string](t, res, "action"); got != "tree_rewound" {
		t.Errorf("unpublished whole-tree restore action = %q, want tree_rewound", got)
	}
}

// TestRestoreRefusesAmbiguousShapes: no target is an error (never a
// silent whole-tree discard), and all: true tolerates no other
// argument.
func TestRestoreRefusesAmbiguousShapes(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/guarded"})
	f.write(t, "README.md", "precious edit\n")

	for name, args := range map[string]map[string]any{
		"bare":              {},
		"all plus path":     {"all": true, "path": "README.md"},
		"misspelled target": {"checkpont": "abcd"},
		"wrong key":         {"id": "abcd"},
	} {
		text, isErr := f.call(t, "restore", args)
		if !isErr {
			t.Fatalf("restore %s (%v) succeeded: %s", name, args, text)
		}
		if !strings.Contains(text, "bad arguments") {
			t.Errorf("restore %s error %q is not a bad-arguments refusal", name, text)
		}
	}
	// The refusals must have destroyed nothing.
	b, err := os.ReadFile(filepath.Join(f.dir, "README.md"))
	if err != nil || string(b) != "precious edit\n" {
		t.Fatalf("README.md = %q, %v; a refused restore modified the tree", b, err)
	}
}

func TestSwitchWork(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/first"})
	f.write(t, "a.txt", "one\n")
	f.ok(t, "checkpoint", map[string]any{"message": "on first"})
	f.ok(t, "start_work", map[string]any{"name": "agent/second"})

	text := f.ok(t, "switch_work", map[string]any{"name": "agent/first"})
	if !strings.Contains(text, "agent/first") {
		t.Errorf("switch answer %q does not name the branch", text)
	}
	st := asJSON(t, f.ok(t, "current_state", nil))
	if st["branch"] != "agent/first" {
		t.Errorf("branch = %v after switch_work, want agent/first", st["branch"])
	}

	if text, isErr := f.call(t, "switch_work", map[string]any{"name": "agent/never"}); !isErr {
		t.Errorf("switch_work to a missing branch succeeded: %s", text)
	}
}

func TestSetAsideAndResume(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/parcel"})
	f.write(t, "draft.txt", "parked\n")
	f.ok(t, "set_aside", nil)
	st := asJSON(t, f.ok(t, "current_state", nil))
	if n := field[float64](t, st, "set_aside"); n != 1 {
		t.Fatalf("set_aside count = %v after parking", n)
	}
	f.ok(t, "resume", nil)
	st = asJSON(t, f.ok(t, "current_state", nil))
	if n := field[float64](t, st, "set_aside"); n != 0 {
		t.Errorf("set_aside count = %v after resume", n)
	}
}

func TestSyncFromUpstreamOnDefault(t *testing.T) {
	f := newFixture(t)
	res := asJSON(t, f.ok(t, "sync_from_upstream", nil))
	if res["replayed"] != false || res["merged"] != false {
		t.Errorf("sync on an up-to-date default = %v, want a plain fast-forward answer", res)
	}
}

// TestSyncConflictIsStructured: a conflicted replay answers with data —
// the files, and the fact the tree was restored — not prose alone.
func TestSyncConflictIsStructured(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/conflicted"})
	f.write(t, "README.md", "my line\n")
	f.ok(t, "checkpoint", map[string]any{"message": "my README"})

	// Upstream motion from a second clone: the same file, a different
	// line.
	other := filepath.Join(f.tmp, "other")
	origin := filepath.Join(f.tmp, "origin.git")
	archivetest.GitRun(t, "", "clone", origin, other)
	if err := os.WriteFile(filepath.Join(other, "README.md"), []byte("their line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivetest.GitRun(t, other, "add", "-A")
	archivetest.GitRun(t, other, "commit", "-m", "their README")
	archivetest.GitRun(t, other, "push", "origin", "main")

	text, isErr := f.call(t, "sync_from_upstream", nil)
	if !isErr {
		t.Fatalf("conflicted sync succeeded: %s", text)
	}
	res := asJSON(t, text)
	if res["conflict"] != true || res["tree_restored"] != true {
		t.Fatalf("conflict answer = %v, want conflict and tree_restored true", res)
	}
	files := field[[]any](t, res, "files")
	if len(files) != 1 || files[0] != "README.md" {
		t.Errorf("conflict files = %v, want [README.md]", files)
	}
}

// TestAbandonWorkReportsActualBranch: abandoning a branch that is not
// checked out must not claim a switch to the default branch.
func TestAbandonWorkReportsActualBranch(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/doomed"})
	f.ok(t, "start_work", map[string]any{"name": "agent/current"})

	text := f.ok(t, "abandon_work", map[string]any{"name": "agent/doomed"})
	if !strings.Contains(text, "on agent/current") {
		t.Errorf("abandon answer %q does not report the branch actually checked out", text)
	}

	text = f.ok(t, "abandon_work", map[string]any{"name": "agent/current"})
	if !strings.Contains(text, "on main") {
		t.Errorf("abandoning the current branch should land on the default: %q", text)
	}
}

// TestFileAtRefusesBinary: non-UTF-8 content is refused, never handed
// back corrupted under a success status.
func TestFileAtRefusesBinary(t *testing.T) {
	f := newFixture(t)
	f.ok(t, "start_work", map[string]any{"name": "agent/binary"})
	if err := os.WriteFile(filepath.Join(f.dir, "blob.bin"), []byte{0xff, 0xfe, 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	f.ok(t, "checkpoint", map[string]any{"message": "add blob"})

	text, isErr := f.call(t, "file_at", map[string]any{"ref": "HEAD", "path": "blob.bin"})
	if !isErr {
		t.Fatalf("file_at on binary content succeeded: %q", text)
	}
	if !strings.Contains(text, "not UTF-8") {
		t.Errorf("binary refusal %q does not say why", text)
	}
}

// TestToolErrorsAreResults: failures come back as tool-level errors
// (IsError), never transport errors — and injection-shaped or unknown
// input dies at the parser with a "bad arguments" answer.  Engine-side
// refusals are covered by internal/archive's suite; one representative
// (checkpoint on the default branch) proves they surface correctly.
func TestToolErrorsAreResults(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"dashy branch name", "start_work", map[string]any{"name": "-x"}, "bad arguments"},
		{"uppercase checkpoint id", "show_change", map[string]any{"id": "ABCD"}, "bad arguments"},
		{"range ref refused", "history", map[string]any{"ref": "main..evil"}, "bad arguments"},
		{"unknown argument key", "checkpoint", map[string]any{"message": "ok", "sign": true}, "bad arguments"},
		{"checkpoint on default", "checkpoint", map[string]any{"message": "on main"}, "default branch"},
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
