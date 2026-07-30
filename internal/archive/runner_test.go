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
)

// TestHooksNeverExecute: a hook dropped into the repository's own
// .git/hooks — the default location, needing no config key at all, and
// writable by the agent post-M3 — must never run.  Every invocation
// points core.hooksPath at an empty directory the runner owns, so git
// looks for hooks somewhere the repository cannot reach.
func TestHooksNeverExecute(t *testing.T) {
	r := newRig(t)
	marker := filepath.Join(r.tmp, "hook-ran")
	hook := filepath.Join(r.dir, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\necho ran > '"+filepath.ToSlash(marker)+"'\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	r.startWork("agent/hooked")
	r.write("a.txt", "content\n")
	if _, err := r.a.Checkpoint(context.Background(), "checkpoint despite a hostile hook", nil); err != nil {
		t.Errorf("checkpoint should succeed with hooks neutralized, got: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the repository's own pre-commit hook executed")
	}
}

// TestWorkTreePinnedToTheMount: a repo-local core.worktree must not move
// the tree git reads and writes.  The config guard would refuse this
// workspace outright, so the test drives the runner directly — the guard
// is check-then-act, and this is the layer that has to hold when a
// determined agent wins that race: --work-tree on every invocation.
func TestWorkTreePinnedToTheMount(t *testing.T) {
	r := newRig(t)
	outside := filepath.Join(r.tmp, "outside")
	writeFile(t, filepath.Join(outside, "loot.txt"), "a phase-3 credential\n")
	r.git(r.dir, "config", "core.worktree", filepath.ToSlash(outside))

	ctx := context.Background()
	top, err := r.a.run.out(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if !sameDir(top, r.dir) {
		t.Errorf("core.worktree relocated the working tree to %s; want the mount root %s", top, r.dir)
	}
	status, err := r.a.run.out(ctx, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status, "loot.txt") {
		t.Errorf("git saw files outside the mount:\n%s", status)
	}
}

// TestFilterDriverNeverExecutes: `filter.<name>.clean` plus a one-line
// .gitattributes is arbitrary code execution on checkpoint's `add -A`
// (verified against git 2.54).  The driver section is named by whoever
// writes the config, so no -c override can pre-empt it — the workspace
// itself has to be refused.
func TestFilterDriverNeverExecutes(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/filtered")
	marker := filepath.Join(r.tmp, "filter-ran")
	script := filepath.Join(r.tmp, "filter.sh")
	writeFile(t, script, "#!/bin/sh\necho ran > '"+filepath.ToSlash(marker)+"'\ncat\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	r.write(".gitattributes", "secret filter=pwn\n")
	r.write("secret", "data\n")
	r.git(r.dir, "config", "filter.pwn.clean", filepath.ToSlash(script))

	if _, err := r.a.Checkpoint(context.Background(), "record with a hostile filter", nil); !errors.Is(err, ErrHostileConfig) {
		t.Errorf("checkpoint with filter.pwn.clean configured = %v, want ErrHostileConfig", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the configured clean filter executed")
	}
}

// TestParentEnvironmentScrubbed: GIT_* variables in the archivist's own
// environment (author overrides, config redirection) must never reach
// git — the runner builds its environment from an allowlist.
func TestParentEnvironmentScrubbed(t *testing.T) {
	poisoned := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, poisoned, "[user]\n\tname = Mallory\n\temail = mallory@evil.test\n")
	t.Setenv("GIT_AUTHOR_NAME", "Mallory")
	t.Setenv("GIT_AUTHOR_EMAIL", "mallory@evil.test")
	t.Setenv("GIT_COMMITTER_NAME", "Mallory")
	t.Setenv("GIT_CONFIG_GLOBAL", poisoned)

	r := newRig(t)
	r.startWork("agent/scrubbed")
	r.write("a.txt", "content\n")
	r.checkpoint("who am I")

	changes, err := r.a.History(context.Background(), HistoryQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Author != botIdent.Name || changes[0].Email != botIdent.Email {
		t.Errorf("checkpoint recorded as %s <%s>, want the pinned bot identity %s <%s>",
			changes[0].Author, changes[0].Email, botIdent.Name, botIdent.Email)
	}
}

// TestRepoConfigCannotChooseIdentity: user.name/user.email written into
// the repo-local config (agent-writable post-M3) must lose to the
// per-invocation identity pin.
func TestRepoConfigCannotChooseIdentity(t *testing.T) {
	r := newRig(t)
	r.git(r.dir, "config", "user.name", "Mallory")
	r.git(r.dir, "config", "user.email", "mallory@evil.test")

	r.startWork("agent/pinned")
	r.write("a.txt", "content\n")
	r.checkpoint("pinned identity")

	changes, err := r.a.History(context.Background(), HistoryQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Author != botIdent.Name || changes[0].Email != botIdent.Email {
		t.Errorf("checkpoint recorded as %s <%s>, want the pinned bot identity", changes[0].Author, changes[0].Email)
	}
}

// TestCheckpointTimeFromInjectedClock: recorded times are the injected
// clock's testimony, not the wall clock's.
func TestCheckpointTimeFromInjectedClock(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/clock")
	r.write("a.txt", "content\n")
	r.checkpoint("timed")

	changes, err := r.a.History(context.Background(), HistoryQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := changes[0].Time
	if !got.After(clockEpoch) || got.Sub(clockEpoch) > time.Hour {
		t.Errorf("checkpoint time %v is not from the injected clock (epoch %v)", got, clockEpoch)
	}
}
