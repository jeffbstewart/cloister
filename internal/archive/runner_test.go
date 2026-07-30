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
	"testing"
	"time"
)

// TestHooksNeverExecute: a repo-local core.hooksPath naming a hook that
// would fail every commit must be overridden by the runner's own empty
// hooks directory — the archivist never executes repository-supplied
// code, even when the repository's config asks it to.
func TestHooksNeverExecute(t *testing.T) {
	r := newRig(t)
	hookDir := filepath.Join(r.tmp, "hostile-hooks")
	hook := filepath.Join(hookDir, "pre-commit")
	writeFile(t, hook, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	r.git(r.dir, "config", "core.hooksPath", hookDir)

	r.startWork("agent/hooked")
	r.write("a.txt", "content\n")
	if _, err := r.a.Checkpoint(context.Background(), "checkpoint despite hostile hook", nil); err != nil {
		t.Errorf("checkpoint should succeed with hooks neutralized, got: %v", err)
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
