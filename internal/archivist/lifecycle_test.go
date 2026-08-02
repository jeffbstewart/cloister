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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/archive/archivetest"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forgelint"
)

const provisionURL = "https://github.com/op/repo"

// fakeGate is a controllable provision gate: err is what Verify returns,
// namespace what the repo's forge-lint config declares (R8).
type fakeGate struct {
	err       error
	namespace string
}

func (g *fakeGate) Verify(context.Context, endpoint.Endpoint, string, string) (string, error) {
	return g.namespace, g.err
}

func archivistTable(t *testing.T) *endpoint.Table {
	t.Helper()
	cred := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(cred, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `endpoints:
  - name: github.com
    canonical: https://github.com/
    wire: https://github.com/
    forge: github
    api: https://api.github.com/
    apiRelay: github-api-relay:443
    credentialFile: ` + filepath.ToSlash(cred) + `
    bot:
      name: cloister-bot
      email: bot@cloister.test
`
	p := filepath.Join(t.TempDir(), "endpoints.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	tab, err := endpoint.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

// seedForgeLintOrigin builds a bare origin holding a README and a
// forge-lint config (the file the real gate reads from staging).
func seedForgeLintOrigin(t *testing.T, origin, tmp string) {
	t.Helper()
	seed := filepath.Join(tmp, "seed")
	archivetest.GitRun(t, "", "init", "--bare", "-b", "main", origin)
	archivetest.GitRun(t, "", "init", "-b", "main", seed)
	if err := os.MkdirAll(filepath.Join(seed, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"README.md":               "hello\n",
		".github/forge-lint.yaml": "forge: github\nrepo: op/repo\n",
	} {
		if err := os.WriteFile(filepath.Join(seed, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archivetest.GitRun(t, seed, "add", "-A")
	archivetest.GitRun(t, seed, "commit", "-m", "seed")
	archivetest.GitRun(t, seed, "push", origin, "main:main")
}

// newProvisionFixture wires the MCP surface over a real Grange with the
// clone injected (the endpoint table would refuse a path remote), so the
// provision/dispose verbs run end to end without a network.
func newProvisionFixture(t *testing.T) (*fixture, *fakeGate, *fakeAuditor) {
	t.Helper()
	archivetest.RequireGit(t)
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	seedForgeLintOrigin(t, origin, tmp)

	root := filepath.Join(tmp, "grange")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	at := time.Unix(1_753_000_000, 0).UTC()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(time.Second)
		return at
	}
	gate := &fakeGate{}
	aud := &fakeAuditor{}
	cloner := func(_ context.Context, _ endpoint.Endpoint, _, dst string) error {
		archivetest.GitRun(t, "", "clone", origin, dst)
		archivetest.GitRun(t, dst, "remote", "set-url", "origin", provisionURL)
		return nil
	}
	g, err := archive.NewGrange(archive.GrangeConfig{
		Root: root, Table: archivistTable(t), Gate: gate, Now: clock, Cloner: cloner,
	})
	if err != nil {
		t.Fatalf("NewGrange: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	f := &fixture{tmp: tmp, dir: filepath.Join(root, "tree"),
		session: dial(t, New(Config{Version: "test", Grange: g, Audit: aud}))}
	return f, gate, aud
}

func TestProvisionDisposeVerbs(t *testing.T) {
	f, _, aud := newProvisionFixture(t)

	// Before provisioning, working-tree verbs refuse — but current_state
	// ANSWERS: reporting where things stand is its whole job, so the
	// pre-provision state is a successful answer naming the next step.
	st := asJSON(t, f.ok(t, "current_state", nil))
	if st["provisioned"] != false {
		t.Fatalf("current_state before provision = %v, want provisioned:false", st)
	}
	if next := field[string](t, st, "next"); !strings.Contains(next, "provision") {
		t.Errorf("next = %q, want it to name provision", next)
	}
	// Every OTHER verb still refuses, clearly.
	if text, isErr := f.call(t, "pending_changes", nil); !isErr || !strings.Contains(text, "provision first") {
		t.Fatalf("pending_changes before provision = %q (err=%v), want a provision-first refusal", text, isErr)
	}

	res := asJSON(t, f.ok(t, "provision", map[string]any{"repo": provisionURL, "branch": "agent/feature"}))
	if res["repo"] != "op/repo" || res["branch"] != "agent/feature" {
		t.Errorf("provision answer = %v", res)
	}
	st = asJSON(t, f.ok(t, "current_state", nil))
	if st["branch"] != "agent/feature" || st["provisioned"] != true {
		t.Errorf("post-provision state = %v, want provisioned on agent/feature", st)
	}
	recs := aud.records()
	if len(recs) != 1 || recs[0].Tool != "provision" || recs[0].Decision != audit.DecisionProvisioned {
		t.Fatalf("audit = %+v, want one provisioned record", recs)
	}
	if d := recs[0].Lifecycle(); d == nil || d.Repo != "op/repo" || d.Branch != "agent/feature" {
		t.Errorf("lifecycle detail = %+v", recs[0].Detail)
	}

	f.ok(t, "dispose", nil)
	if st = asJSON(t, f.ok(t, "current_state", nil)); st["provisioned"] != false {
		t.Errorf("current_state after dispose = %v, want provisioned:false — the workspace is empty", st)
	}
	if recs := aud.records(); len(recs) != 2 || recs[1].Tool != "dispose" || recs[1].Decision != audit.DecisionDisposed {
		t.Errorf("dispose audit = %+v", aud.records())
	}
}

func TestProvisionGateRefusalIsAudited(t *testing.T) {
	f, gate, aud := newProvisionFixture(t)
	gate.err = &GateRefusedError{
		Repo:     "op/repo",
		Blocking: []forgelint.Verdict{{Req: "R2", Status: forgelint.Violation, Detail: "stale approvals survive"}},
	}
	text, isErr := f.call(t, "provision", map[string]any{"repo": provisionURL})
	if !isErr || !strings.Contains(text, "R2") {
		t.Fatalf("provision through a refusing gate = %q (err=%v), want an R2 refusal", text, isErr)
	}
	recs := aud.records()
	if len(recs) != 1 || recs[0].Decision != audit.DecisionLifecycleRefused {
		t.Fatalf("audit = %+v, want one lifecycle_refused record", recs)
	}
	if d := recs[0].Lifecycle(); d == nil || d.Requirement != "R2" {
		t.Errorf("refusal detail = %+v, want Requirement R2", recs[0].Detail)
	}
	// The refused provision left the workspace empty.
	if st := asJSON(t, f.ok(t, "current_state", nil)); st["provisioned"] != false {
		t.Errorf("a refused provision left a usable workspace: %v", st)
	}
}

func TestProvisionRefusesUnknownHost(t *testing.T) {
	f, _, aud := newProvisionFixture(t)
	text, isErr := f.call(t, "provision", map[string]any{"repo": "https://evil.example/op/repo"})
	if !isErr || !strings.Contains(text, "allowlist") {
		t.Fatalf("provision of an off-table host = %q (err=%v), want an allowlist refusal", text, isErr)
	}
	if recs := aud.records(); len(recs) != 1 || recs[0].Decision != audit.DecisionLifecycleRefused {
		t.Errorf("audit = %+v, want a lifecycle_refused record", aud.records())
	}
}

// TestVerbsSerializeUnderConcurrency hammers the surface — lifecycle
// transitions interleaved with working-tree verbs — from many goroutines
// at once.  The single serialization lock must keep the live-workspace
// pointer and the tree consistent: nothing panics, every call returns a
// clean answer or a clean refusal, and the server is still usable after.
// Run under -race, an unsynchronized g.arc access is a failure.
func TestVerbsSerializeUnderConcurrency(t *testing.T) {
	f, _, _ := newProvisionFixture(t)
	ctx := context.Background()
	call := func(name string, args map[string]any) {
		// Errors are fine (a verb may hit an empty workspace mid-storm); a
		// panic or a race is not.
		f.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(4)
		go func() {
			defer wg.Done()
			call("provision", map[string]any{"repo": provisionURL, "branch": "agent/race"})
		}()
		go func() { defer wg.Done(); call("current_state", nil) }()
		go func() { defer wg.Done(); call("history", map[string]any{"limit": 1}) }()
		go func() { defer wg.Done(); call("dispose", map[string]any{"force": true}) }()
	}
	wg.Wait()

	// The surface still works: dispose to a known-empty state, then a clean
	// provision succeeds.
	f.session.CallTool(ctx, &mcp.CallToolParams{Name: "dispose", Arguments: map[string]any{"force": true}})
	f.ok(t, "provision", map[string]any{"repo": provisionURL, "branch": "agent/after"})
	st := asJSON(t, f.ok(t, "current_state", nil))
	if st["branch"] != "agent/after" {
		t.Errorf("post-storm branch = %v, want agent/after", st["branch"])
	}
}
