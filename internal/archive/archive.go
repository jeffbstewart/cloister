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

// Package archive is the archivist's engine: the hardened git runner and
// the local (working-tree) verb set of docs/archivist.md — phase 1 of the
// M1 execution sequencing.  The verbs speak the VCS-agnostic contract
// (checkpoint, restore, set_aside, ...) and this package realizes them in
// git; nothing here opens a network connection, serves MCP, or touches
// compose — those arrive in later phases.
//
// Two contract rules shape every realization below:
//
//   - No staging: the index is an implementation detail.  Checkpoints
//     read the working tree; selective recording is a paths parameter.
//   - Published history is append-only: once a branch has a published
//     counterpart, restore and sync switch from history rewrite
//     (reset --hard, rebase) to forward motion (content restore, merge),
//     because rewinding would need the force-push the archivist refuses.
package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Identity is the bot identity a checkpoint is recorded as.  It is pinned
// per invocation from the archivist's endpoint table — never read from
// git config — so nothing that can write `.git/config` (the agent,
// post-M3) can spoof the author of an archivist checkpoint.
type Identity struct {
	Name  string
	Email string
}

// Archive drives one provisioned working tree through hardened git
// invocations.  Construct with New against an existing checkout; the
// grange lifecycle (provision/dispose, a later phase) is what brings a
// checkout into being.  An Archive holds an empty hooks directory on
// disk; Close releases it.
type Archive struct {
	run   *runner
	def   string // the default branch name, e.g. "main"
	ident Identity
}

// config collects the New options.
type config struct {
	gitPath string
	def     string
	now     func() time.Time
}

// Option customizes New.
type Option func(*config)

// WithClock injects the clock behind checkpoint timestamps; tests pin it.
// nil is ignored (the real clock stays).
func WithClock(now func() time.Time) Option {
	return func(c *config) {
		if now != nil {
			c.now = now
		}
	}
}

// WithGitPath overrides the git binary looked up on PATH.
func WithGitPath(path string) Option {
	return func(c *config) { c.gitPath = path }
}

// WithDefaultBranch overrides default-branch detection for a checkout
// whose origin/HEAD is absent or wrong.
func WithDefaultBranch(name string) Option {
	return func(c *config) { c.def = name }
}

// New opens the working tree rooted exactly at dir.  It refuses a dir
// that is not itself the worktree root — an archivist must never operate
// up-tree of its mount — and detects the default branch from the
// clone's origin/HEAD unless WithDefaultBranch overrides it.
func New(dir string, ident Identity, opts ...Option) (*Archive, error) {
	if ident.Name == "" || ident.Email == "" {
		return nil, fmt.Errorf("archive: identity requires both name and email")
	}
	cfg := config{gitPath: "git", now: time.Now}
	for _, o := range opts {
		o(&cfg)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("archive: resolving %q: %w", dir, err)
	}
	// The hooks path handed to every invocation: an empty directory we
	// own, so repository-supplied hooks can never execute.  A missing
	// directory would also neutralize hooks, but an existing empty one
	// cannot be racily created underneath us by another party.
	hooks, err := os.MkdirTemp("", "cloister-no-hooks-")
	if err != nil {
		return nil, fmt.Errorf("archive: creating the empty hooks dir: %w", err)
	}
	a := &Archive{
		run:   &runner{git: cfg.gitPath, dir: abs, hooks: hooks, now: cfg.now},
		def:   cfg.def,
		ident: ident,
	}
	ctx := context.Background()
	top, err := a.run.out(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		os.RemoveAll(hooks)
		return nil, fmt.Errorf("archive: %s is not a git working tree: %w", abs, err)
	}
	if !sameDir(abs, top) {
		os.RemoveAll(hooks)
		return nil, fmt.Errorf("archive: %s is inside the working tree rooted at %s; the archivist operates only on its own mount root", abs, top)
	}
	if a.def == "" {
		head, err := a.run.out(ctx, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			os.RemoveAll(hooks)
			return nil, fmt.Errorf("archive: cannot detect the default branch (no origin/HEAD in %s); a grange clone always has one — pass WithDefaultBranch only for hand-built checkouts: %w", abs, err)
		}
		a.def = strings.TrimPrefix(head, "origin/")
	}
	return a, nil
}

// Close releases the empty hooks directory.  The Archive is unusable
// afterward.
func (a *Archive) Close() error {
	return os.RemoveAll(a.run.hooks)
}

// DefaultBranch reports the branch the checkout regards as default —
// the base for start_work and the merge source for sync_from_upstream.
func (a *Archive) DefaultBranch() string { return a.def }

// sameDir reports whether two paths name the same directory once
// platform quirks (case, separators, symlinks) are normalized away.
func sameDir(x, y string) bool {
	xi, err := os.Stat(x)
	if err != nil {
		return false
	}
	yi, err := os.Stat(y)
	if err != nil {
		return false
	}
	return os.SameFile(xi, yi)
}

// currentBranch names the checked-out branch, or "" when HEAD is
// detached (a state no verb creates; surfaced so callers can say so).
func (a *Archive) currentBranch(ctx context.Context) (string, error) {
	out, code, err := a.run.exit(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", nil // detached HEAD
	}
	return out, nil
}

// upstreamOf resolves branch's published counterpart ("" when the branch
// has never been published).  Publication state is what flips restore
// and sync from history rewrite to forward motion.
func (a *Archive) upstreamOf(ctx context.Context, branch string) (string, error) {
	out, code, err := a.run.exit(ctx, "rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{upstream}")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", nil
	}
	return out, nil
}

// isAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant — the fast-forward test behind the append-only rule.
func (a *Archive) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, code, err := a.run.exit(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return false, err
	}
	return code == 0, nil
}
