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

package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/archive/archivetest"
	"github.com/jeffbstewart/cloister/internal/endpoint"
)

// grangeRepoURL is the canonical URL the test table admits; the injected
// cloner clones the local origin instead, then presents this URL as the
// workspace's origin so the (https-only) table opens it.
const grangeRepoURL = "https://github.com/op/repo"

// stubGate is an injectable ProvisionGate: err is what Verify returns, and
// it records what it was asked about so a test can assert the wiring.
type stubGate struct {
	err        error
	namespace  string // what the repo's forge-lint config declares (R8)
	sawRepo    string
	sawStaging string
}

func (s *stubGate) Verify(_ context.Context, _ endpoint.Endpoint, repo, staging string) (string, error) {
	s.sawRepo, s.sawStaging = repo, staging
	return s.namespace, s.err
}

// grangeRig is a Grange over a local bare origin.  The clone is injected
// (a path remote is exactly what the endpoint table refuses to admit as a
// real endpoint), so provision resolves the canonical URL for naming and
// the table check while the injected cloner clones the local origin.
type grangeRig struct {
	t      *testing.T
	root   string
	origin string
	gate   *stubGate
	clock  *stepClock
	g      *Grange
}

func newGrangeRig(t *testing.T) *grangeRig {
	t.Helper()
	archivetest.RequireGit(t)
	tmp := t.TempDir()
	r := &grangeRig{
		t:      t,
		root:   filepath.Join(tmp, "grange"),
		origin: filepath.Join(tmp, "origin.git"),
		gate:   &stubGate{},
		clock:  &stepClock{t: clockEpoch, step: time.Second},
	}
	seedForgeOrigin(t, r.origin, tmp)
	if err := os.MkdirAll(r.root, 0o755); err != nil {
		t.Fatal(err)
	}
	g, err := NewGrange(GrangeConfig{
		Root:   r.root,
		Table:  testTable(t),
		Gate:   r.gate,
		Now:    r.clock.Now,
		Cloner: r.cloner,
	})
	if err != nil {
		t.Fatalf("NewGrange: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	r.g = g
	return r
}

// cloner clones the local origin into dst, then rewrites origin to the
// canonical URL so the table (which never admits a path remote) opens the
// promoted workspace; the fetch already happened from the local path.
func (r *grangeRig) cloner(_ context.Context, _ endpoint.Endpoint, _, dst string) error {
	archivetest.GitRun(r.t, "", "clone", r.origin, dst)
	archivetest.GitRun(r.t, dst, "remote", "set-url", "origin", grangeRepoURL)
	return nil
}

// seedForgeOrigin builds a bare origin on main holding a README and a
// .github/forge-lint.yaml (the file the real gate would read from staging).
func seedForgeOrigin(t *testing.T, origin, tmp string) {
	t.Helper()
	seed := filepath.Join(tmp, "seed")
	archivetest.GitRun(t, "", "init", "--bare", "-b", "main", origin)
	archivetest.GitRun(t, "", "init", "-b", "main", seed)
	if err := os.MkdirAll(filepath.Join(seed, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"README.md":               "hello\n",
		".github/forge-lint.yaml": "forge: github\nrepo: op/repo\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(seed, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archivetest.GitRun(t, seed, "add", "-A")
	archivetest.GitRun(t, seed, "commit", "-m", "seed")
	archivetest.GitRun(t, seed, "push", origin, "main:main")
}

func TestHardenedCloneProducesAWorkingTree(t *testing.T) {
	archivetest.RequireGit(t)
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	seedForgeOrigin(t, origin, tmp)

	dst := filepath.Join(tmp, "clone")
	clock := &stepClock{t: clockEpoch, step: time.Second}
	if err := hardenedClone(context.Background(), "git", t.TempDir(), clock.Now, overlay{}, origin, dst); err != nil {
		t.Fatalf("hardenedClone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "README.md")); err != nil {
		t.Errorf("clone is missing the checked-out README: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dst, ".git")); err != nil || !fi.IsDir() {
		t.Errorf("clone is missing its .git directory")
	}
}

func TestGrangeProvisionAndDispose(t *testing.T) {
	r := newGrangeRig(t)
	ctx := context.Background()

	if st, _ := r.g.State(); st != StateEmpty {
		t.Fatalf("initial state = %s, want empty", st)
	}
	branch, _ := ParseBranchName("agent/feature")
	info, err := r.g.Provision(ctx, grangeRepoURL, branch)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if info.Repo != "op/repo" || info.Branch != "agent/feature" || info.Endpoint != "github.com" {
		t.Errorf("ProvisionInfo = %+v", info)
	}
	// The gate saw the staging checkout and the resolved repo.
	if r.gate.sawRepo != "op/repo" || r.gate.sawStaging != filepath.Join(r.root, "staging") {
		t.Errorf("gate saw repo=%q staging=%q", r.gate.sawRepo, r.gate.sawStaging)
	}
	if st, _ := r.g.State(); st != StateProvisioned {
		t.Fatalf("post-provision state = %s, want provisioned", st)
	}
	if _, err := os.Stat(r.g.markerPath()); err != nil {
		t.Errorf("provenance marker missing: %v", err)
	}
	if _, err := os.Stat(r.g.staging); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging survived promote: %v", err)
	}
	a, err := r.g.Archive()
	if err != nil {
		t.Fatalf("Archive after provision: %v", err)
	}
	st, err := a.CurrentState(ctx)
	if err != nil {
		t.Fatalf("CurrentState: %v", err)
	}
	if st.Branch != "agent/feature" {
		t.Errorf("provisioned on branch %q, want agent/feature", st.Branch)
	}

	if _, err := r.g.Dispose(ctx, false); err != nil {
		t.Fatalf("Dispose of a clean workspace: %v", err)
	}
	if s, _ := r.g.State(); s != StateEmpty {
		t.Errorf("post-dispose state = %s, want empty", s)
	}
	if _, err := r.g.Archive(); !errors.Is(err, ErrNotProvisioned) {
		t.Errorf("Archive after dispose = %v, want ErrNotProvisioned", err)
	}
}

// TestBranchNamespaceRefusedLocally: the forge restricts branch creation
// to the repo's agent namespace and rejects anything else at PUSH — by
// which time the agent has committed work to a branch that can never be
// published.  The namespace the gate learned at provision is enforced
// here instead, at creation, where the fix is free; and it survives a
// restart through the provenance marker.
func TestBranchNamespaceRefusedLocally(t *testing.T) {
	r := newGrangeRig(t)
	r.gate.namespace = "agent/" // what the repo's forge-lint config declares
	ctx := context.Background()

	branch, _ := ParseBranchName("agent/feature")
	if _, err := r.g.Provision(ctx, grangeRepoURL, branch); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	arc, err := r.g.Archive()
	if err != nil {
		t.Fatal(err)
	}

	bad, _ := ParseBranchName("agent-walkthrough") // the shape that bit us on abbot
	err = arc.StartWork(ctx, bad)
	if !errors.Is(err, ErrOutsideNamespace) {
		t.Fatalf("start_work(agent-walkthrough) = %v, want ErrOutsideNamespace", err)
	}
	// The refusal must teach the convention, not just deny.
	if !strings.Contains(err.Error(), "agent/") {
		t.Errorf("refusal %q does not name the required prefix", err)
	}
	good, _ := ParseBranchName("agent/walkthrough")
	if err := arc.StartWork(ctx, good); err != nil {
		t.Fatalf("start_work(agent/walkthrough) = %v, want success", err)
	}

	// The marker carries the namespace, so a restart still refuses.
	m, err := r.g.readMarker()
	if err != nil || m.Namespace != "agent/" {
		t.Fatalf("marker namespace = %q (%v), want it recorded for restart", m.Namespace, err)
	}
}

func TestGrangeProvisionRefusesNonEmpty(t *testing.T) {
	r := newGrangeRig(t)
	ctx := context.Background()
	if _, err := r.g.Provision(ctx, grangeRepoURL, BranchName{}); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if _, err := r.g.Provision(ctx, grangeRepoURL, BranchName{}); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("second Provision = %v, want ErrNotEmpty", err)
	}
}

func TestGrangeGateRefusalLeavesEmpty(t *testing.T) {
	r := newGrangeRig(t)
	r.gate.err = errors.New("R2 VIOLATION: stale approvals survive")
	ctx := context.Background()
	if _, err := r.g.Provision(ctx, grangeRepoURL, BranchName{}); err == nil {
		t.Fatal("Provision through a refusing gate: want error, got nil")
	}
	if st, _ := r.g.State(); st != StateEmpty {
		t.Errorf("state after a gate refusal = %s, want empty (staging discarded)", st)
	}
	if _, err := os.Stat(r.g.staging); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging survived a gate refusal: %v", err)
	}
}

func TestGrangeDisposeRefusesUnpublished(t *testing.T) {
	r := newGrangeRig(t)
	ctx := context.Background()
	branch, _ := ParseBranchName("agent/wip")
	if _, err := r.g.Provision(ctx, grangeRepoURL, branch); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	a, _ := r.g.Archive()
	if err := os.WriteFile(filepath.Join(r.g.tree, "new.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Checkpoint(ctx, "unpublished work", nil); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	var ue *UnpublishedError
	if _, err := r.g.Dispose(ctx, false); !errors.As(err, &ue) {
		t.Fatalf("Dispose(force=false) with unpublished work = %v, want *UnpublishedError", err)
	}
	if ue.Work.Unpushed != 1 {
		t.Errorf("unpushed count = %d, want 1", ue.Work.Unpushed)
	}
	if st, _ := r.g.State(); st != StateProvisioned {
		t.Errorf("a refused dispose changed state to %s", st)
	}
	if _, err := r.g.Dispose(ctx, true); err != nil {
		t.Fatalf("Dispose(force=true): %v", err)
	}
	if st, _ := r.g.State(); st != StateEmpty {
		t.Errorf("forced dispose left state %s, want empty", st)
	}
}

func TestGrangeRefusesToDisposeAMarkerlessTree(t *testing.T) {
	archivetest.RequireGit(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "grange")
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	// A .git with no provenance marker — a mounted host tree.
	archivetest.GitRun(t, "", "init", "-b", "main", tree)
	g, err := NewGrange(GrangeConfig{
		Root:  root,
		Table: testTable(t),
		Gate:  &stubGate{},
		Now:   (&stepClock{t: clockEpoch, step: time.Second}).Now,
	})
	if err != nil {
		t.Fatalf("NewGrange: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	if st, _ := g.State(); st != StateCorrupt {
		t.Fatalf("state = %s, want corrupt", st)
	}
	for _, force := range []bool{false, true} {
		if _, err := g.Dispose(context.Background(), force); !errors.Is(err, ErrCorruptWorkspace) {
			t.Errorf("Dispose(force=%v) of a markerless tree = %v, want ErrCorruptWorkspace", force, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tree, ".git")); err != nil {
		t.Errorf("the host tree was removed despite the refusal: %v", err)
	}
}

func TestGrangeRestartAdoptsProvisioned(t *testing.T) {
	r := newGrangeRig(t)
	ctx := context.Background()
	if _, err := r.g.Provision(ctx, grangeRepoURL, BranchName{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	r.g.Close()

	// A fresh Grange over the same root adopts the provisioned tree.
	g2, err := NewGrange(GrangeConfig{
		Root:   r.root,
		Table:  testTable(t),
		Gate:   r.gate,
		Now:    r.clock.Now,
		Cloner: r.cloner,
	})
	if err != nil {
		t.Fatalf("NewGrange (restart): %v", err)
	}
	t.Cleanup(func() { g2.Close() })
	if st, _ := g2.State(); st != StateProvisioned {
		t.Fatalf("restart state = %s, want provisioned", st)
	}
	if _, err := g2.Archive(); err != nil {
		t.Errorf("Archive after restart: %v", err)
	}
}

func TestAdoptArchiveRefusesLifecycleVerbs(t *testing.T) {
	r := newRig(t)
	g := AdoptArchive(r.a)
	if _, err := g.State(); !errors.Is(err, ErrAdopted) {
		t.Errorf("State on an adopted grange = %v, want ErrAdopted", err)
	}
	if _, err := g.Provision(context.Background(), grangeRepoURL, BranchName{}); !errors.Is(err, ErrAdopted) {
		t.Errorf("Provision on an adopted grange = %v, want ErrAdopted", err)
	}
	if _, err := g.Dispose(context.Background(), true); !errors.Is(err, ErrAdopted) {
		t.Errorf("Dispose on an adopted grange = %v, want ErrAdopted", err)
	}
	// It still serves the live Archive it wraps.
	if _, err := g.Archive(); err != nil {
		t.Errorf("Archive on an adopted grange = %v, want the wrapped Archive", err)
	}
}

func TestGrangeProvisionResumesExistingBranch(t *testing.T) {
	r := newGrangeRig(t)
	ctx := context.Background()
	// Publish an agent branch to the origin before provisioning it.
	seed := filepath.Join(t.TempDir(), "pub")
	archivetest.GitRun(t, "", "clone", r.origin, seed)
	archivetest.GitRun(t, seed, "switch", "-c", "agent/resumed")
	if err := os.WriteFile(filepath.Join(seed, "resumed.txt"), []byte("prior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivetest.GitRun(t, seed, "add", "-A")
	archivetest.GitRun(t, seed, "commit", "-m", "prior work")
	archivetest.GitRun(t, seed, "push", r.origin, "agent/resumed")

	branch, _ := ParseBranchName("agent/resumed")
	if _, err := r.g.Provision(ctx, grangeRepoURL, branch); err != nil {
		t.Fatalf("Provision (resume): %v", err)
	}
	a, _ := r.g.Archive()
	st, err := a.CurrentState(ctx)
	if err != nil {
		t.Fatalf("CurrentState: %v", err)
	}
	if st.Branch != "agent/resumed" {
		t.Errorf("resumed onto %q, want agent/resumed", st.Branch)
	}
	// The resumed line of work carries its prior file — proof it checked out
	// the existing branch rather than starting an empty one.
	if _, err := os.Stat(filepath.Join(r.g.tree, "resumed.txt")); err != nil {
		t.Errorf("resumed branch missing its prior file: %v", err)
	}
}
