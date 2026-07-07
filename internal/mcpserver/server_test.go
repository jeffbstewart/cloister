// End-to-end tests for server.go: a real MCP client session over in-memory
// transports, with actions that re-exec this test binary as the workload
// (same helper-process pattern as the runner tests).
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/runner"
)

// TestHelperProcess is the child workload, active only when re-exec'd.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	sep := slices.Index(os.Args, "--")
	if sep < 0 || sep+1 >= len(os.Args) {
		os.Exit(2)
	}
	args := os.Args[sep+1:]
	switch args[0] {
	case "exit":
		fmt.Println("helper ran with args:", strings.Join(args, " "))
		code, _ := strconv.Atoi(args[1])
		os.Exit(code)
	}
	os.Exit(2)
}

type fixture struct {
	srv          *Server
	session      *mcp.ClientSession
	manifestPath string
	auditPath    string
}

// manifestYAML declares two actions that re-exec this test binary.  The
// cache entry smuggles GO_WANT_HELPER_PROCESS=1 into the child through the
// allowlist env path (the value falls back to the cache path).
func manifestYAML() string {
	exe := os.Args[0]
	return fmt.Sprintf(`harness: 1
toolchain: tc-test
actions:
  run_ok:
    description: exit zero
    run: ['%s', '-test.run=TestHelperProcess', '--', 'exit', '0']
    timeout: 1m
    parser: generic
    params:
      filter:
        description: a filter
        flag: '--filter'
        pattern: '^[A-Za-z0-9_.*]+$'
  run_fail:
    description: exit three
    run: ['%s', '-test.run=TestHelperProcess', '--', 'exit', '3']
    timeout: 1m
    parser: generic
caches:
  - volume: helper
    env: GO_WANT_HELPER_PROCESS
    path: "1"
`, exe, exe)
}

func newFixture(t *testing.T, withManifest bool) *fixture {
	t.Helper()
	ws := t.TempDir()
	state := t.TempDir()
	logs := filepath.Join(state, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(ws, "agent-harness.yaml")
	if withManifest {
		if err := os.WriteFile(manifestPath, []byte(manifestYAML()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	auditPath := filepath.Join(state, "audit.jsonl")
	al, err := audit.Open(auditPath, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })

	srv := New(Config{
		Version:      "test",
		ToolchainID:  "tc-test",
		Workspace:    ws,
		ManifestPath: manifestPath,
		LogsDir:      logs,
		Runner: &runner.Runner{
			LogsDir:     logs,
			ToolchainID: "tc-test",
			KillGrace:   200 * time.Millisecond,
		},
		Audit: al,
	})

	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.mcp.Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	return &fixture{srv: srv, session: session, manifestPath: manifestPath, auditPath: auditPath}
}

func callTool(t *testing.T, f *fixture, name string, args map[string]any) (string, bool) {
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

type digestJSON struct {
	RunID    string `json:"runId"`
	Status   string `json:"status"`
	ExitCode int    `json:"exitCode"`
}

// TestActionLifecycle walks the milestone-1 loop: run an action with
// a param, get a digest, page the log, and find every call audited.
func TestActionLifecycle(t *testing.T) {
	f := newFixture(t, true)

	// Action with a valid param returns an ok digest.
	text, isErr := callTool(t, f, "run_ok", map[string]any{"filter": "SomeTest"})
	if isErr {
		t.Fatalf("run_ok errored: %s", text)
	}
	var d digestJSON
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		t.Fatalf("digest is not JSON: %v\n%s", err, text)
	}
	if d.Status != "ok" || d.ExitCode != 0 || d.RunID == "" {
		t.Errorf("digest = %+v, want ok/0 with a runId", d)
	}

	// Failing action reports status and exit code, not an error.
	text, isErr = callTool(t, f, "run_fail", nil)
	if isErr {
		t.Fatalf("run_fail errored: %s", text)
	}
	var d2 digestJSON
	if err := json.Unmarshal([]byte(text), &d2); err != nil {
		t.Fatal(err)
	}
	if d2.Status != "failed" || d2.ExitCode != 3 {
		t.Errorf("digest = %+v, want failed/3", d2)
	}

	// A param failing its pattern is rejected, never executed.
	text, isErr = callTool(t, f, "run_ok", map[string]any{"filter": "bad value!"})
	if !isErr || !strings.Contains(text, "does not match") {
		t.Errorf("bad param: isErr=%v text=%q", isErr, text)
	}

	// get_log pages the persisted log of a prior run.
	text, isErr = callTool(t, f, "get_log", map[string]any{"runId": d.RunID})
	if isErr || !strings.Contains(text, "helper ran with args") {
		t.Errorf("get_log: isErr=%v text=%q", isErr, text)
	}

	// get_log rejects anything that is not a canonical run id.
	if text, isErr := callTool(t, f, "get_log", map[string]any{"runId": "../../etc/passwd"}); !isErr {
		t.Errorf("hostile runId accepted: %q", text)
	}

	// Every action call landed in the audit log; get_log is not an action.
	data, err := os.ReadFile(f.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	records := strings.Count(string(data), "\n")
	if records != 3 {
		t.Errorf("audit has %d records, want 3:\n%s", records, data)
	}
	if got := strings.Count(string(data), `"decision":"run"`); got != 2 {
		t.Errorf(`%d "run" decisions, want 2`, got)
	}
	if got := strings.Count(string(data), `"decision":"rejected_param"`); got != 1 {
		t.Errorf(`%d "rejected_param" decisions, want 1`, got)
	}
	// The resolved argv is recorded on run records: the manifest's
	// run array plus the validated param appear in the audit line.
	if !strings.Contains(string(data), `"argv":`) || !strings.Contains(string(data), `"--filter","SomeTest"`) {
		t.Errorf("audit did not record the resolved argv:\n%s", data)
	}
}

func TestHarnessInfo(t *testing.T) {
	f := newFixture(t, true)
	text, isErr := callTool(t, f, "harness_info", nil)
	if isErr {
		t.Fatalf("harness_info errored: %s", text)
	}
	for _, want := range []string{`"manifest": "ok"`, "run_ok", "run_fail", "tc-test"} {
		if !strings.Contains(text, want) {
			t.Errorf("harness_info missing %q:\n%s", want, text)
		}
	}
}

// TestDegradedWithoutManifest: no manifest means harness_info is the
// only tool served, and it says why.
func TestDegradedWithoutManifest(t *testing.T) {
	f := newFixture(t, false)

	res, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if len(names) != 1 || names[0] != "harness_info" {
		t.Fatalf("degraded menu = %v, want exactly [harness_info]", names)
	}

	text, isErr := callTool(t, f, "harness_info", nil)
	if isErr || !strings.Contains(text, "no manifest") {
		t.Errorf("harness_info: isErr=%v text=%q", isErr, text)
	}
}

// TestManifestEditTakesEffectWithoutRestart: the manifest is re-read
// on every action call, so breaking it after startup rejects the next call.
func TestManifestEditTakesEffectWithoutRestart(t *testing.T) {
	f := newFixture(t, true)

	broken := "harness: 2\ntoolchain: tc-test\nactions:\n  run_ok:\n    run: [\"x\"]\n    timeout: 1m\n"
	if err := os.WriteFile(f.manifestPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	text, isErr := callTool(t, f, "run_ok", nil)
	if !isErr || !strings.Contains(text, "supports only 1") {
		t.Errorf("call against broken manifest: isErr=%v text=%q", isErr, text)
	}

	data, err := os.ReadFile(f.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"decision":"rejected_no_manifest"`) {
		t.Errorf("rejection not audited:\n%s", data)
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(t, false)
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
