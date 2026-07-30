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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/archive/archivetest"
)

// The tests run real git against throwaway repositories: a bare origin,
// a seed clone that populates it, and the workspace clone the Archive
// drives — the fake-remote rig from the execution plan.  Everything is
// under t.TempDir; no network, no host config (the rig scrubs the
// environment the same way the runner does).

// botIdent is the endpoint-table identity the Archive commits as.
var botIdent = Identity{Name: "cloister-bot", Email: "bot@cloister.test"}

// stepClock is the injected clock: a fixed start, a fixed step per
// read, so recorded times are deterministic.
type stepClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

// clockEpoch is the rig clock's start; every Now() lands after it.
var clockEpoch = time.Unix(1_753_000_000, 0).UTC()

// requireGit skips the test when no git binary is available.
func requireGit(t *testing.T) {
	t.Helper()
	archivetest.RequireGit(t)
}

// rig is one origin + workspace pair with an open Archive.
type rig struct {
	t      *testing.T
	tmp    string
	origin string // the bare "remote"
	dir    string // the workspace the Archive drives
	other  string // a second clone, for simulating upstream motion; lazy
	clock  *stepClock
	a      *Archive
}

// newRig builds the fake remote (origin.git seeded with one README
// commit on main) and clones the workspace.
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		t:     t,
		tmp:   t.TempDir(),
		clock: &stepClock{t: clockEpoch, step: time.Second},
	}
	r.origin, r.dir = archivetest.Seed(t, r.tmp)

	a, err := New(r.dir, botIdent, WithClock(r.clock.Now))
	if err != nil {
		t.Fatalf("New(%s): %v", r.dir, err)
	}
	t.Cleanup(func() { a.Close() })
	r.a = a
	return r
}

// git runs raw git for rig setup and independent verification — NOT the
// hardened runner, so the tests never assume the code under test.
func (r *rig) git(dir string, args ...string) string {
	r.t.Helper()
	return archivetest.GitRun(r.t, dir, args...)
}

// write puts content at a workspace-relative path.
func (r *rig) write(rel, content string) {
	r.t.Helper()
	writeFile(r.t, filepath.Join(r.dir, filepath.FromSlash(rel)), content)
}

// read returns a workspace file's content.
func (r *rig) read(rel string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel)))
	if err != nil {
		r.t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// startWork parses the name and begins a line of work, fatally on error.
func (r *rig) startWork(name string) BranchName {
	r.t.Helper()
	b, err := ParseBranchName(name)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.a.StartWork(context.Background(), b); err != nil {
		r.t.Fatalf("start_work(%s): %v", name, err)
	}
	return b
}

// checkpoint records the tree, fatally on error.
func (r *rig) checkpoint(msg string, paths ...string) CheckpointID {
	r.t.Helper()
	id, err := r.a.Checkpoint(context.Background(), msg, paths)
	if err != nil {
		r.t.Fatalf("checkpoint(%q): %v", msg, err)
	}
	return id
}

// publish simulates the (phase 3) publish verb: push the branch and set
// its upstream, as the remote verb will.
func (r *rig) publish(branch string) {
	r.t.Helper()
	r.git(r.dir, "push", "-u", "origin", branch)
}

// upstreamCommit advances a branch on the origin from a second clone —
// motion the workspace has not seen until it fetches.
func (r *rig) upstreamCommit(branch, rel, content, msg string) {
	r.t.Helper()
	if r.other == "" {
		r.other = filepath.Join(r.tmp, "other")
		r.git("", "clone", r.origin, r.other)
	}
	r.git(r.other, "fetch", "origin")
	r.git(r.other, "switch", "--force-create", branch, "origin/"+branch)
	writeFile(r.t, filepath.Join(r.other, filepath.FromSlash(rel)), content)
	r.git(r.other, "add", "-A")
	r.git(r.other, "commit", "-m", msg)
	r.git(r.other, "push", "origin", branch)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewDetectsDefaultBranch(t *testing.T) {
	r := newRig(t)
	if got := r.a.DefaultBranch(); got != "main" {
		t.Errorf("DefaultBranch() = %q, want main", got)
	}
}

func TestNewRefusesSubdirectory(t *testing.T) {
	r := newRig(t)
	sub := filepath.Join(r.dir, "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(sub, botIdent); err == nil {
		t.Error("New on a subdirectory of the worktree should refuse; the archivist operates only on its mount root")
	}
}

// TestCurrentBranchRefusesHostileHead: .git/HEAD is agent-writable, a
// ref named "-x" is check-ref-format-valid, and the current branch name
// is interpolated into later argv — so a dashy name read from HEAD must
// come back as an error, not as a value.
func TestCurrentBranchRefusesHostileHead(t *testing.T) {
	r := newRig(t)
	writeFile(t, filepath.Join(r.dir, ".git", "HEAD"), "ref: refs/heads/-x\n")
	if _, err := r.a.CurrentState(context.Background()); err == nil {
		t.Error("a HEAD naming refs/heads/-x should read as an error, not a branch")
	}
}

func TestNewRefusesNonRepo(t *testing.T) {
	requireGit(t)
	if _, err := New(t.TempDir(), botIdent); err == nil {
		t.Error("New on a plain directory should refuse")
	}
}

func TestNewRequiresIdentity(t *testing.T) {
	requireGit(t)
	if _, err := New(t.TempDir(), Identity{Name: "x"}); err == nil {
		t.Error("New without an email should refuse")
	}
	if _, err := New(t.TempDir(), Identity{Email: "x@y"}); err == nil {
		t.Error("New without a name should refuse")
	}
}
