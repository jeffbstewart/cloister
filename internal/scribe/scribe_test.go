package scribe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/runid"
	"github.com/jeffbstewart/cloister/internal/workspace"
)

func mustRunID(t *testing.T) runid.ID {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatalf("runid.New() failed: %v", err)
	}
	return id
}

// fakeAuditor records audit lines and doubles as an in-memory DiffStore.
type fakeAuditor struct {
	mu    sync.Mutex
	recs  []audit.Record
	diffs map[string][]byte
}

func (f *fakeAuditor) Append(r audit.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
	return nil
}

func (f *fakeAuditor) PutDiff(id runid.ID, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diffs == nil {
		f.diffs = map[string][]byte{}
	}
	f.diffs[id.String()] = append([]byte(nil), payload...)
	return nil
}

func (f *fakeAuditor) FetchDiff(id runid.ID) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.diffs[id.String()]
	if !ok {
		return nil, fmt.Errorf("no diff for %s", id)
	}
	return p, nil
}

func (f *fakeAuditor) hasDecision(d audit.Decision) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.recs {
		if r.Decision == d {
			return true
		}
	}
	return false
}

// fakeApprovals is an in-memory ApprovalClient that returns a fixed decision for
// every op (the state-service long-poll/timeout is exercised in statesink).
type fakeApprovals struct {
	mu         sync.Mutex
	registered []runid.ID
	decision   approval.Decision
}

func (f *fakeApprovals) RegisterPending(id runid.ID, tool, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, id)
	return nil
}

func (f *fakeApprovals) PollDecision(id runid.ID) (approval.Record, error) {
	f.mu.Lock()
	d := f.decision
	f.mu.Unlock()
	if !d.Resolved() {
		time.Sleep(5 * time.Millisecond) // stand in for the server's long-poll hold
		return approval.Record{OpID: id, Decision: approval.Pending}, nil
	}
	return approval.Record{OpID: id, Decision: d}, nil
}

func (f *fakeApprovals) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registered)
}

func newApprovalFixture(t *testing.T, decision approval.Decision) (*fixture, *fakeApprovals) {
	t.Helper()
	dir := t.TempDir()
	root, err := workspace.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	aud := &fakeAuditor{}
	appr := &fakeApprovals{decision: decision}
	srv := New(Config{Version: "test", Root: root, Audit: aud, Diffs: aud, Approvals: appr, StageDir: t.TempDir()})

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
	return &fixture{srv: srv, dir: dir, aud: aud, session: session}, appr
}

func (f *fakeAuditor) last() (audit.Record, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recs) == 0 {
		return audit.Record{}, false
	}
	return f.recs[len(f.recs)-1], true
}

type fixture struct {
	srv     *Server
	dir     string
	aud     *fakeAuditor
	session *mcp.ClientSession
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	root, err := workspace.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	aud := &fakeAuditor{}
	srv := New(Config{Version: "test", Root: root, Audit: aud, Diffs: aud})

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
	return &fixture{srv: srv, dir: dir, aud: aud, session: session}
}

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
		t.Fatalf("CallTool(%s): content type %T", name, res.Content[0])
	}
	return tc.Text, res.IsError
}

func (f *fixture) onDisk(rel string) string {
	return filepath.Join(f.dir, filepath.FromSlash(rel))
}

func TestCreateTextFile(t *testing.T) {
	f := newFixture(t)
	// Nested path exercises parent-dir creation.
	text, isErr := f.call(t, "create_text_file", map[string]any{"path": "src/Foo.kt", "content": "hello"})
	if isErr {
		t.Fatalf("create_text_file errored: %s", text)
	}
	if got, _ := os.ReadFile(f.onDisk("src/Foo.kt")); string(got) != "hello" {
		t.Errorf("file content = %q, want hello", got)
	}
	rec, ok := f.aud.last()
	if !ok || rec.Mutation == nil || rec.Tool != "create_text_file" || rec.Decision != decApplied || rec.Mutation.Path != "src/Foo.kt" {
		t.Errorf("audit record = %+v, want applied create_text_file src/Foo.kt", rec)
	}
	if rec.RunID.IsZero() {
		t.Error("audit record has no opId")
	}
}

func TestCreateTextFileRejectsExisting(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "a.txt", "content": "1"})
	text, isErr := f.call(t, "create_text_file", map[string]any{"path": "a.txt", "content": "2"})
	if !isErr || !strings.Contains(text, "already exists") {
		t.Errorf("re-create should fail: isErr=%v text=%q", isErr, text)
	}
	if got, _ := os.ReadFile(f.onDisk("a.txt")); string(got) != "1" {
		t.Errorf("file was overwritten: %q", got)
	}
}

func TestCreateTextFileDryRun(t *testing.T) {
	f := newFixture(t)
	_, isErr := f.call(t, "create_text_file", map[string]any{"path": "d.txt", "content": "x", "dryRun": true})
	if isErr {
		t.Fatal("dryRun errored")
	}
	if _, err := os.Stat(f.onDisk("d.txt")); err == nil {
		t.Error("dryRun created the file")
	}
	if rec, _ := f.aud.last(); rec.Decision != decDryRun {
		t.Errorf("dryRun decision = %q, want dry_run", rec.Decision)
	}
}

// TestConfinementRejected proves the scribe wires Resolve: an escaping path is
// rejected and nothing is written outside the workspace.
func TestConfinementRejected(t *testing.T) {
	f := newFixture(t)
	text, isErr := f.call(t, "create_text_file", map[string]any{"path": "../escape.txt", "content": "x"})
	if !isErr {
		t.Errorf("escaping path was accepted: %s", text)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.dir), "escape.txt")); err == nil {
		t.Fatal("wrote a file OUTSIDE the workspace")
	}
	if rec, _ := f.aud.last(); rec.Decision != decConfine {
		t.Errorf("decision = %q, want rejected_confinement", rec.Decision)
	}
}

func TestCreateDirectory(t *testing.T) {
	f := newFixture(t)
	if _, isErr := f.call(t, "create_directory", map[string]any{"path": "a/b/c"}); isErr {
		t.Fatal("create_directory errored")
	}
	if fi, err := os.Stat(f.onDisk("a/b/c")); err != nil || !fi.IsDir() {
		t.Errorf("directory not created: %v", err)
	}
}

func TestMoveFile(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "a.txt", "content": "move me"})
	if _, isErr := f.call(t, "move_file", map[string]any{"from": "a.txt", "to": "sub/b.txt"}); isErr {
		t.Fatal("move_file errored")
	}
	if _, err := os.Stat(f.onDisk("a.txt")); err == nil {
		t.Error("source still present after move")
	}
	if got, _ := os.ReadFile(f.onDisk("sub/b.txt")); string(got) != "move me" {
		t.Errorf("moved content = %q", got)
	}

	// Moving onto an existing file requires overwrite.
	f.call(t, "create_text_file", map[string]any{"path": "c.txt", "content": "c"})
	if _, isErr := f.call(t, "move_file", map[string]any{"from": "sub/b.txt", "to": "c.txt"}); !isErr {
		t.Error("move onto existing file without overwrite should fail")
	}
	if _, isErr := f.call(t, "move_file", map[string]any{"from": "sub/b.txt", "to": "c.txt", "overwrite": true}); isErr {
		t.Error("move with overwrite should succeed")
	}
}

func TestCopyFile(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "a.txt", "content": "dup me"})

	if _, isErr := f.call(t, "copy_file", map[string]any{"from": "a.txt", "to": "sub/b.txt"}); isErr {
		t.Fatal("copy_file errored")
	}
	// Source is preserved; destination has the same content.
	if got, _ := os.ReadFile(f.onDisk("a.txt")); string(got) != "dup me" {
		t.Errorf("source changed after copy: %q", got)
	}
	if got, _ := os.ReadFile(f.onDisk("sub/b.txt")); string(got) != "dup me" {
		t.Errorf("copy content = %q", got)
	}

	// Copying onto an existing file needs overwrite.
	if _, isErr := f.call(t, "copy_file", map[string]any{"from": "a.txt", "to": "sub/b.txt"}); !isErr {
		t.Error("copy onto existing without overwrite should fail")
	}
	if _, isErr := f.call(t, "copy_file", map[string]any{"from": "a.txt", "to": "sub/b.txt", "overwrite": true}); isErr {
		t.Error("copy with overwrite should succeed")
	}

	// Copying a directory is refused.
	f.call(t, "create_directory", map[string]any{"path": "adir"})
	if _, isErr := f.call(t, "copy_file", map[string]any{"from": "adir", "to": "bdir"}); !isErr {
		t.Error("copy_file of a directory should be refused")
	}

	// Source == destination is refused — even when aliased (./a.txt) and even
	// with overwrite, since the comparison is on the resolved paths.
	if _, isErr := f.call(t, "copy_file", map[string]any{"from": "a.txt", "to": "./a.txt", "overwrite": true}); !isErr {
		t.Error("copy_file with from == to (aliased) should be refused")
	}
}

func TestMoveDirectory(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "d/f.txt", "content": "x"})
	if _, isErr := f.call(t, "move_directory", map[string]any{"from": "d", "to": "e"}); isErr {
		t.Fatal("move_directory errored")
	}
	if _, err := os.Stat(f.onDisk("d")); err == nil {
		t.Error("source dir still present")
	}
	if _, err := os.Stat(f.onDisk("e/f.txt")); err != nil {
		t.Errorf("moved dir contents missing: %v", err)
	}
}

func TestDeleteFile(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "x.txt", "content": "x"})
	if _, isErr := f.call(t, "delete_file", map[string]any{"path": "x.txt"}); isErr {
		t.Fatal("delete_file errored")
	}
	if _, err := os.Stat(f.onDisk("x.txt")); err == nil {
		t.Error("file still present after delete")
	}
	// Deleting a directory is refused (delete_directory is not yet supported).
	f.call(t, "create_directory", map[string]any{"path": "adir"})
	if _, isErr := f.call(t, "delete_file", map[string]any{"path": "adir"}); !isErr {
		t.Error("delete_file on a directory should be refused")
	}
}

func TestListWorkspaceRoots(t *testing.T) {
	f := newFixture(t)
	text, isErr := f.call(t, "list_workspace_roots", nil)
	if isErr {
		t.Fatalf("list_workspace_roots errored: %s", text)
	}
	// Parse the JSON — a raw-substring match fails on Windows because the JSON
	// escapes backslashes in the path.
	var out struct {
		Roots []string `json:"roots"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result not JSON: %v\n%s", err, text)
	}
	if len(out.Roots) != 1 || out.Roots[0] != filepath.Clean(f.dir) {
		t.Errorf("roots = %v, want [%q]", out.Roots, filepath.Clean(f.dir))
	}
}

func TestDeleteDirectory(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "d/f.txt", "content": "x"})

	// Non-recursive delete of a non-empty directory is refused.
	if _, isErr := f.call(t, "delete_directory", map[string]any{"path": "d"}); !isErr {
		t.Error("non-recursive delete of a non-empty dir should fail")
	}
	if _, err := os.Stat(f.onDisk("d/f.txt")); err != nil {
		t.Error("failed delete removed content")
	}

	// Recursive delete removes the subtree.
	if _, isErr := f.call(t, "delete_directory", map[string]any{"path": "d", "recursive": true}); isErr {
		t.Fatal("recursive delete_directory errored")
	}
	if _, err := os.Stat(f.onDisk("d")); err == nil {
		t.Error("directory still present after recursive delete")
	}

	// The workspace root itself cannot be deleted.
	if _, isErr := f.call(t, "delete_directory", map[string]any{"path": ".", "recursive": true}); !isErr {
		t.Error("deleting the workspace root should be refused")
	}
	if _, err := os.Stat(f.dir); err != nil {
		t.Fatal("workspace root was deleted")
	}
}

func TestApplyDiffOp(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "f.txt", "content": "a\nb\nc\n"})
	diff := "@@ @@\n a\n-b\n+B\n c\n"

	// dryRun does not write.
	if _, isErr := f.call(t, "apply_diff", map[string]any{"path": "f.txt", "diff": diff, "dryRun": true}); isErr {
		t.Fatal("dryRun apply_diff errored")
	}
	if got, _ := os.ReadFile(f.onDisk("f.txt")); string(got) != "a\nb\nc\n" {
		t.Errorf("dryRun modified the file: %q", got)
	}

	// Real apply.
	if _, isErr := f.call(t, "apply_diff", map[string]any{"path": "f.txt", "diff": diff}); isErr {
		t.Fatal("apply_diff errored")
	}
	if got, _ := os.ReadFile(f.onDisk("f.txt")); string(got) != "a\nB\nc\n" {
		t.Errorf("apply_diff result = %q", got)
	}

	// Re-applying is already-applied (a retrying agent isn't sent flailing).
	text, isErr := f.call(t, "apply_diff", map[string]any{"path": "f.txt", "diff": diff})
	if isErr || !strings.Contains(text, "already_applied") {
		t.Errorf("re-apply should report already_applied: isErr=%v text=%q", isErr, text)
	}
}

func TestApplyDiffPermitNonUTF8RefusedWithoutApprovals(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "f.txt", "content": "a\n"})
	text, isErr := f.call(t, "apply_diff", map[string]any{"path": "f.txt", "diff": "@@ @@\n-a\n+b\n", "permit_non_utf8": true})
	if !isErr || !strings.Contains(text, "approval") {
		t.Errorf("permit_non_utf8 should be refused without an approval channel: isErr=%v text=%q", isErr, text)
	}
	if rec, _ := f.aud.last(); rec.Decision != decGate {
		t.Errorf("decision = %q, want rejected_gate", rec.Decision)
	}
}

func TestReplaceString(t *testing.T) {
	f := newFixture(t)

	// scope all.
	f.call(t, "create_text_file", map[string]any{"path": "a.txt", "content": "foo foo foo"})
	if _, isErr := f.call(t, "replace_string", map[string]any{"path": "a.txt", "find": "foo", "replace": "bar", "scope": "all"}); isErr {
		t.Fatal("replace_string all errored")
	}
	if got, _ := os.ReadFile(f.onDisk("a.txt")); string(got) != "bar bar bar" {
		t.Errorf("replace all = %q", got)
	}

	// expectedCount mismatch fails and leaves the file untouched.
	f.call(t, "create_text_file", map[string]any{"path": "b.txt", "content": "x x x"})
	if _, isErr := f.call(t, "replace_string", map[string]any{"path": "b.txt", "find": "x", "replace": "y", "scope": "all", "expectedCount": 2}); !isErr {
		t.Error("expectedCount mismatch should fail")
	}
	if got, _ := os.ReadFile(f.onDisk("b.txt")); string(got) != "x x x" {
		t.Errorf("failed expectedCount modified the file: %q", got)
	}

	// scope first (default) replaces only the first occurrence.
	f.call(t, "create_text_file", map[string]any{"path": "c.txt", "content": "z z z"})
	if _, isErr := f.call(t, "replace_string", map[string]any{"path": "c.txt", "find": "z", "replace": "Z"}); isErr {
		t.Fatal("replace_string first errored")
	}
	if got, _ := os.ReadFile(f.onDisk("c.txt")); string(got) != "Z z z" {
		t.Errorf("replace first = %q", got)
	}
}

func TestReplaceRegex(t *testing.T) {
	f := newFixture(t)

	// Capture-group replacement.
	f.call(t, "create_text_file", map[string]any{"path": "a.txt", "content": "hello world\n"})
	if _, isErr := f.call(t, "replace_regex", map[string]any{"path": "a.txt", "pattern": `(\w+) (\w+)`, "replacement": "$2 $1", "scope": "all"}); isErr {
		t.Fatal("replace_regex errored")
	}
	if got, _ := os.ReadFile(f.onDisk("a.txt")); string(got) != "world hello\n" {
		t.Errorf("regex swap = %q", got)
	}

	// A bad pattern is rejected as rejected_pattern.
	f.call(t, "create_text_file", map[string]any{"path": "b.txt", "content": "x"})
	if _, isErr := f.call(t, "replace_regex", map[string]any{"path": "b.txt", "pattern": "(", "replacement": "y"}); !isErr {
		t.Error("bad regex should fail")
	}
	if rec, _ := f.aud.last(); rec.Decision != decPattern {
		t.Errorf("bad regex decision = %q, want rejected_pattern", rec.Decision)
	}
}

func TestDiffPayloadAndGetDiff(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "f.txt", "content": "a\nb\nc\n"})

	res, isErr := f.call(t, "apply_diff", map[string]any{"path": "f.txt", "diff": "@@ @@\n a\n-b\n+B\n c\n"})
	if isErr {
		t.Fatalf("apply_diff errored: %s", res)
	}
	rec, _ := f.aud.last()
	if rec.Mutation == nil || !rec.Mutation.HasDiff || rec.Mutation.LinesAdded != 1 || rec.Mutation.LinesRemoved != 1 || rec.Mutation.SHA256After == "" {
		t.Errorf("audit record not enriched: %+v", rec)
	}
	if rec.Mutation == nil || rec.Mutation.BytesBefore == 0 || rec.Mutation.BytesAfter == 0 || rec.Mutation.FilesTouched != 1 {
		t.Errorf("audit summary fields missing: %+v", rec)
	}

	// get_diff returns the stored payload — both the submitted and applied diffs.
	text, isErr := f.call(t, "get_diff", map[string]any{"opId": rec.RunID.String()})
	if isErr {
		t.Fatalf("get_diff errored: %s", text)
	}
	if !strings.Contains(text, "submitted diff") || !strings.Contains(text, "applied diff") || !strings.Contains(text, "+B") {
		t.Errorf("get_diff payload unexpected:\n%s", text)
	}

	// An unknown opId is a clean error.
	if _, isErr := f.call(t, "get_diff", map[string]any{"opId": mustRunID(t).String()}); !isErr {
		t.Error("get_diff of an unknown opId should error")
	}
}

func TestDiffPayloadTruncates(t *testing.T) {
	big := strings.Repeat("+x\n", DefaultMaxDiffBytes) // well over the cap
	out, truncated := diffPayload("", big)
	if !truncated {
		t.Error("oversized payload should be flagged truncated")
	}
	if !strings.Contains(string(out), "truncated") {
		t.Error("truncated payload should carry a visible marker")
	}
	if _, tr := diffPayload("", "small"); tr {
		t.Error("small payload should not be flagged truncated")
	}
}

func TestIsBuildLogic(t *testing.T) {
	build := []string{
		"agent-harness.yaml",
		"build.gradle.kts",
		"app/build.gradle.kts",
		"settings.gradle.kts",
		"gradle.properties",
		"gradlew",
		"gradlew.bat",
		"gradle/wrapper/gradle-wrapper.properties",
		"gradle/libs.versions.toml",
		"buildSrc/src/main/kotlin/Deps.kt",
	}
	for _, p := range build {
		if !isBuildLogic(p) {
			t.Errorf("isBuildLogic(%q) = false, want true", p)
		}
	}
	ok := []string{
		"src/main/kotlin/Foo.kt",
		"README.md",
		"docs/build.md",             // not a .gradle.kts
		"mygradle/notes.txt",        // first segment isn't exactly "gradle"
		"nested/agent-harness.yaml", // only the root manifest counts
	}
	for _, p := range ok {
		if isBuildLogic(p) {
			t.Errorf("isBuildLogic(%q) = true, want false", p)
		}
	}
}

func TestGateRefusesBuildLogic(t *testing.T) {
	f := newFixture(t)

	// A build-logic file cannot be created without an approval channel.
	text, isErr := f.call(t, "create_text_file", map[string]any{"path": "build.gradle.kts", "content": "plugins {}"})
	if !isErr || !strings.Contains(text, "build logic") {
		t.Errorf("create build.gradle.kts should be gated: isErr=%v text=%q", isErr, text)
	}
	if _, err := os.Stat(f.onDisk("build.gradle.kts")); err == nil {
		t.Error("gated file was written")
	}
	if rec, _ := f.aud.last(); rec.Decision != decGate {
		t.Errorf("decision = %q, want rejected_gate", rec.Decision)
	}

	// The manifest is gated too.
	if _, isErr := f.call(t, "create_text_file", map[string]any{"path": "agent-harness.yaml", "content": "x"}); !isErr {
		t.Error("create agent-harness.yaml should be gated")
	}

	// An ordinary source edit passes and is audited applied.
	if _, isErr := f.call(t, "create_text_file", map[string]any{"path": "src/Main.kt", "content": "fun main(){}"}); isErr {
		t.Fatal("ordinary source create should pass")
	}
	if rec, _ := f.aud.last(); rec.Decision != decApplied {
		t.Errorf("ordinary edit decision = %q, want applied", rec.Decision)
	}
}

func TestGateRefusesEditMoveDeleteOfBuildLogic(t *testing.T) {
	f := newFixture(t)
	// Plant a build file directly, as if git-checked-out.
	if err := os.WriteFile(f.onDisk("settings.gradle.kts"), []byte("rootProject.name=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, isErr := f.call(t, "apply_diff", map[string]any{"path": "settings.gradle.kts", "diff": "@@ @@\n-rootProject.name=\"x\"\n+rootProject.name=\"y\"\n"}); !isErr {
		t.Error("apply_diff to a build file should be gated")
	}
	if _, isErr := f.call(t, "delete_file", map[string]any{"path": "settings.gradle.kts"}); !isErr {
		t.Error("delete of a build file should be gated")
	}
	if _, err := os.Stat(f.onDisk("settings.gradle.kts")); err != nil {
		t.Error("gated delete removed the build file")
	}

	// Moving an ordinary file ONTO a build-logic path is gated (destination).
	if err := os.WriteFile(f.onDisk("a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, isErr := f.call(t, "move_file", map[string]any{"from": "a.txt", "to": "b.gradle.kts"}); !isErr {
		t.Error("moving a file onto a build-logic path should be gated")
	}
}

func TestApprovalApproved(t *testing.T) {
	f, appr := newApprovalFixture(t, approval.Approved)
	if err := os.WriteFile(f.onDisk("build.gradle.kts"), []byte("plugins {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "@@ @@\n plugins {\n+  id(\"x\")\n }\n"

	text, isErr := f.call(t, "apply_diff", map[string]any{"path": "build.gradle.kts", "diff": diff})
	if isErr || !strings.Contains(text, "applied_after_approval") {
		t.Fatalf("approved apply_diff = %q (isErr=%v)", text, isErr)
	}
	if appr.count() != 1 {
		t.Errorf("op was not registered pending (count=%d)", appr.count())
	}
	if got, _ := os.ReadFile(f.onDisk("build.gradle.kts")); !strings.Contains(string(got), `id("x")`) {
		t.Errorf("approved change not applied: %q", got)
	}
	if !f.aud.hasDecision(decPending) || !f.aud.hasDecision(decApplied) {
		t.Error("expected both pending_approval and applied audit records")
	}
}

func TestApprovalRejected(t *testing.T) {
	f, _ := newApprovalFixture(t, approval.Rejected)
	if err := os.WriteFile(f.onDisk("build.gradle.kts"), []byte("plugins {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, isErr := f.call(t, "apply_diff", map[string]any{"path": "build.gradle.kts", "diff": "@@ @@\n plugins {\n+  id(\"x\")\n }\n"})
	if !isErr {
		t.Error("rejected apply_diff should return an error result")
	}
	if got, _ := os.ReadFile(f.onDisk("build.gradle.kts")); strings.Contains(string(got), `id("x")`) {
		t.Errorf("rejected change was applied: %q", got)
	}
	if !f.aud.hasDecision(decRejected) {
		t.Error("expected a rejected audit record")
	}
}

func TestApprovalTimeoutStops(t *testing.T) {
	f, _ := newApprovalFixture(t, approval.Timeout)
	if err := os.WriteFile(f.onDisk("build.gradle.kts"), []byte("plugins {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := f.call(t, "apply_diff", map[string]any{"path": "build.gradle.kts", "diff": "@@ @@\n plugins {\n+  id(\"x\")\n }\n"})
	if !isErr || !strings.Contains(text, "timed out") {
		t.Errorf("timeout should return a stop message: %q (isErr=%v)", text, isErr)
	}
	if !f.aud.hasDecision(decTimeout) {
		t.Error("expected a rejected_timeout audit record")
	}
}

// TestApprovalRecovery: a change staged before a crash is applied when the
// approval lands after restart (durable across a scribe restart).
func TestApprovalRecovery(t *testing.T) {
	f, _ := newApprovalFixture(t, approval.Approved)
	op := stagedOp{
		OpID: mustRunID(t), Tool: "apply_diff", Path: "build.gradle.kts",
		Content: []byte("plugins { id(\"recovered\") }\n"), Perm: 0o644, Payload: []byte("diff"),
	}
	if err := f.srv.writeStaged(op); err != nil {
		t.Fatal(err)
	}

	f.srv.Recover()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(f.onDisk("build.gradle.kts")); err == nil && strings.Contains(string(b), "recovered") {
			return // applied
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("staged change was not applied by Recover within the deadline")
}

// TestPermitNonUTF8Repair: fix the em-dash bug (a lone 0x97,
// invalid UTF-8) via a permit_non_utf8 apply_diff, held pending, approved, applied.
func TestPermitNonUTF8Repair(t *testing.T) {
	f, appr := newApprovalFixture(t, approval.Approved)
	// A file with a lone 0x97 byte (invalid UTF-8): the em-dash bug.
	if err := os.WriteFile(f.onDisk("notes.txt"), []byte("line a\x97b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without the flag, editing a non-UTF-8 file is refused.
	if _, isErr := f.call(t, "apply_diff", map[string]any{"path": "notes.txt", "diff": "@@ @@\n-x\n+y\n"}); !isErr {
		t.Error("editing a non-UTF-8 file without permit_non_utf8 should fail")
	}

	// With the flag, replace the bad byte (view rune U+0097) with a real em-dash.
	// Non-ASCII is built from runes so no literal appears in this source file.
	badView := string(rune(0x97))
	emDash := string(rune(0x2014))
	diff := "@@ @@\n-line a" + badView + "b\n+line a" + emDash + "b\n"
	text, isErr := f.call(t, "apply_diff", map[string]any{"path": "notes.txt", "diff": diff, "permit_non_utf8": true})
	if isErr || !strings.Contains(text, "applied_after_approval") {
		t.Fatalf("permit_non_utf8 repair = %q (isErr=%v)", text, isErr)
	}
	if appr.count() != 1 {
		t.Error("permit_non_utf8 edit was not held pending approval")
	}
	want := "line a" + emDash + "b\n"
	got, _ := os.ReadFile(f.onDisk("notes.txt"))
	if string(got) != want {
		t.Errorf("repaired file = %q, want valid UTF-8 %q", got, want)
	}
}

func TestWriteBinaryFileApproved(t *testing.T) {
	f, appr := newApprovalFixture(t, approval.Approved)
	content := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}

	text, isErr := f.call(t, "write_binary_file", map[string]any{
		"path": "asset.bin", "bytesBase64": base64.StdEncoding.EncodeToString(content),
	})
	if isErr || !strings.Contains(text, "applied_after_approval") {
		t.Fatalf("write_binary_file = %q (isErr=%v)", text, isErr)
	}
	if appr.count() != 1 {
		t.Error("binary write was not held pending approval")
	}
	if got, _ := os.ReadFile(f.onDisk("asset.bin")); !bytes.Equal(got, content) {
		t.Errorf("binary content = %v, want %v", got, content)
	}
}

func TestMalformedDiffIsCaptured(t *testing.T) {
	f := newFixture(t)
	f.call(t, "create_text_file", map[string]any{"path": "f.txt", "content": "a\n"})

	bad := "this is not a diff\njust some prose the model emitted\n"
	if _, isErr := f.call(t, "apply_diff", map[string]any{"path": "f.txt", "diff": bad}); !isErr {
		t.Fatal("malformed diff should be rejected")
	}
	rec, _ := f.aud.last()
	if rec.Mutation == nil || !rec.Mutation.HasDiff {
		t.Fatal("rejected diff was not captured (HasDiff false)")
	}
	// The submitted diff is retrievable, so the operator can see what was sent.
	got, isErr := f.call(t, "get_diff", map[string]any{"opId": rec.RunID.String()})
	if isErr || !strings.Contains(got, "this is not a diff") || !strings.Contains(got, "REJECTED") {
		t.Errorf("get_diff should show the submitted diff + rejection:\n%s", got)
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(f.srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
}
