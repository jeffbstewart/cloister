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
	def   BranchName // the default branch, e.g. "main"
	ident Identity
}

// originRemote is the only remote the archivist speaks to; a grange clone
// has exactly one.
const originRemote = "origin"

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
	if err := validIdentity(ident); err != nil {
		return nil, err
	}
	cfg := config{gitPath: "git", now: time.Now}
	for _, o := range opts {
		o(&cfg)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("archive: resolving %q: %w", dir, err)
	}
	// .git must be a real directory under the mount root.  A gitfile
	// ("gitdir: /elsewhere") is still followed by git even when handed to
	// --git-dir, so refusing it here is what makes the pinned --git-dir
	// mean what it says.
	gitDir := filepath.Join(abs, ".git")
	if info, err := os.Stat(gitDir); err != nil {
		return nil, fmt.Errorf("archive: %s has no .git directory: %w", abs, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("archive: %s/.git is not a directory; the archivist refuses a workspace whose repository lives elsewhere", abs)
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
		run:   &runner{git: cfg.gitPath, dir: abs, gitDir: gitDir, hooks: hooks, now: cfg.now},
		ident: ident,
	}
	ctx := context.Background()
	fail := func(format string, args ...any) (*Archive, error) {
		os.RemoveAll(hooks)
		return nil, fmt.Errorf(format, args...)
	}
	if err := a.guardConfig(ctx); err != nil {
		return fail("archive: opening %s: %w", abs, err)
	}
	top, err := a.run.out(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return fail("archive: %s is not a git working tree: %w", abs, err)
	}
	if !sameDir(abs, top) {
		return fail("archive: %s is inside the working tree rooted at %s; the archivist operates only on its own mount root", abs, top)
	}
	// The default branch is interpolated into later argv — `rebase <def>`,
	// `switch -c <name> <def>` — so it is parsed, not trusted.  It comes
	// either from the operator (WithDefaultBranch) or from origin/HEAD,
	// which is a file inside the agent-writable .git: a ref may legally be
	// named "--exec=/tmp/pwn", and `rebase --exec=...` runs it.
	raw := cfg.def
	if raw == "" {
		head, err := a.run.out(ctx, "symbolic-ref", "--short", "refs/remotes/"+originRemote+"/HEAD")
		if err != nil {
			return fail("archive: cannot detect the default branch (no origin/HEAD in %s); a grange clone always has one — pass WithDefaultBranch only for hand-built checkouts: %w", abs, err)
		}
		raw = strings.TrimPrefix(head, originRemote+"/")
	}
	def, err := ParseBranchName(raw)
	if err != nil {
		return fail("archive: the default branch of %s is not a name the archivist will hand to git: %w", abs, err)
	}
	a.def = def
	return a, nil
}

// Close releases the empty hooks directory.  The Archive is unusable
// afterward.
func (a *Archive) Close() error {
	return os.RemoveAll(a.run.hooks)
}

// DefaultBranch reports the branch the checkout regards as default —
// the base for start_work and the merge source for sync_from_upstream.
func (a *Archive) DefaultBranch() string { return a.def.String() }

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
// The answer is parsed before it is believed: .git/HEAD is agent-written
// content, a ref named "-x" is check-ref-format-valid, and the name
// returned here is interpolated into later argv.  Exit codes are read
// strictly — symbolic-ref says 1 for detached, anything else non-zero
// (128: corrupt or unreadable HEAD) is an error, not "detached".
func (a *Archive) currentBranch(ctx context.Context) (string, error) {
	out, code, err := a.run.exit(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	switch {
	case err != nil:
		return "", err
	case code == 1:
		return "", nil // detached HEAD
	case code != 0:
		return "", fmt.Errorf("archive: reading HEAD: symbolic-ref exited %d", code)
	}
	name, err := ParseBranchName(out)
	if err != nil {
		return "", fmt.Errorf("archive: HEAD names a branch the archivist will not hand to git: %w", err)
	}
	return name.String(), nil
}

// upstreamOf resolves branch's published counterpart ("" when the branch
// has never been published).  Publication state is what flips restore
// and sync from history rewrite to forward motion.
//
// Any non-zero exit reads as "unpublished" deliberately: rev-parse
// @{upstream} exits 128 both for a branch with no upstream and for a
// real error (verified on git 2.43), so there is no code to distinguish
// on, and a corrupt repository fails loudly at the next real operation.
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
// descendant — the fast-forward test behind the append-only rule.  Only
// exit 1 means "no": treating, say, 128 (unknown revision) as "no"
// would let a corrupt workspace answer a question it cannot answer.
func (a *Archive) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, code, err := a.run.exit(ctx, "merge-base", "--is-ancestor", "--end-of-options", ancestor, descendant)
	switch {
	case err != nil:
		return false, err
	case code == 0:
		return true, nil
	case code == 1:
		return false, nil
	}
	return false, fmt.Errorf("archive: merge-base --is-ancestor %s %s exited %d", ancestor, descendant, code)
}
