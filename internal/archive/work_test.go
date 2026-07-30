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
	"strconv"
	"strings"
	"testing"
)

func TestStartWorkBranchesOffDefault(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/fresh")
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "agent/fresh" {
		t.Errorf("Branch = %q, want agent/fresh", st.Branch)
	}
	if st.Published {
		t.Error("a fresh line of work should not be published")
	}
}

func TestStartWorkRefusesDefault(t *testing.T) {
	r := newRig(t)
	name, err := ParseBranchName("main")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.StartWork(context.Background(), name); !errors.Is(err, ErrDefaultBranch) {
		t.Errorf("start_work(main) = %v, want ErrDefaultBranch", err)
	}
}

func TestCheckpointRefusedOnDefault(t *testing.T) {
	r := newRig(t)
	r.write("a.txt", "content\n")
	if _, err := r.a.Checkpoint(context.Background(), "on main", nil); !errors.Is(err, ErrDefaultBranch) {
		t.Errorf("checkpoint on the default branch = %v, want ErrDefaultBranch", err)
	}
}

func TestCheckpointSelectivePaths(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/selective")
	r.write("one.txt", "first\n")
	r.write("two.txt", "second\n")
	id, err := r.a.Checkpoint(context.Background(), "just one", []string{"one.txt"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.a.ShowChange(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Diff, "one.txt") || strings.Contains(c.Diff, "two.txt") {
		t.Errorf("selective checkpoint recorded the wrong paths:\n%s", c.Diff)
	}
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Untracked) != 1 || st.Untracked[0] != "two.txt" {
		t.Errorf("two.txt should remain untracked, state = %+v", st)
	}
}

func TestCheckpointNothingToRecord(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/empty")
	if _, err := r.a.Checkpoint(context.Background(), "nothing", nil); !errors.Is(err, ErrNoChanges) {
		t.Errorf("empty checkpoint = %v, want ErrNoChanges", err)
	}
}

func TestAbandonWork(t *testing.T) {
	r := newRig(t)
	name := r.startWork("agent/doomed")
	r.write("a.txt", "content\n")
	r.checkpoint("work")
	if err := r.a.AbandonWork(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" {
		t.Errorf("after abandon, Branch = %q, want main", st.Branch)
	}
	if out := r.git(r.dir, "branch", "--list", "agent/doomed"); out != "" {
		t.Errorf("branch still exists: %q", out)
	}
}

func TestAbandonWorkRefusesDirtyTree(t *testing.T) {
	r := newRig(t)
	name := r.startWork("agent/dirty")
	r.write("a.txt", "uncommitted\n")
	if err := r.a.AbandonWork(context.Background(), name); !errors.Is(err, ErrDirtyTree) {
		t.Errorf("abandon with a dirty tree = %v, want ErrDirtyTree", err)
	}
}

func TestAbandonWorkRefusesDefault(t *testing.T) {
	r := newRig(t)
	name, err := ParseBranchName("main")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.AbandonWork(context.Background(), name); !errors.Is(err, ErrDefaultBranch) {
		t.Errorf("abandon_work(main) = %v, want ErrDefaultBranch", err)
	}
}

func TestRestoreOneFile(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/restore-file")
	r.write("README.md", "ruined\n")
	if _, err := r.a.Restore(context.Background(), CheckpointID{}, "README.md"); err != nil {
		t.Fatal(err)
	}
	if got := r.read("README.md"); got != "hello\n" {
		t.Errorf("README.md = %q, want the last-checkpoint content", got)
	}
}

func TestRestoreFileFromCheckpoint(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/restore-old")
	r.write("a.txt", "version one\n")
	first := r.checkpoint("v1")
	r.write("a.txt", "version two\n")
	r.checkpoint("v2")
	if _, err := r.a.Restore(context.Background(), first, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if got := r.read("a.txt"); got != "version one\n" {
		t.Errorf("a.txt = %q, want the v1 content", got)
	}
}

func TestRestoreWholeTreeUnpublishedRewinds(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/rewind")
	r.write("a.txt", "one\n")
	first := r.checkpoint("v1")
	r.write("a.txt", "two\n")
	r.checkpoint("v2")

	res, err := r.a.Restore(context.Background(), first, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rewound {
		t.Error("unpublished whole-tree restore should rewind history")
	}
	if head := r.git(r.dir, "rev-parse", "HEAD"); head != first.String() {
		t.Errorf("HEAD = %s, want %s", head, first)
	}
}

func TestRestorePublishedIsForwardMotion(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/append-only")
	r.write("a.txt", "one\n")
	first := r.checkpoint("v1")
	r.write("a.txt", "two\n")
	second := r.checkpoint("v2")
	r.publish("agent/append-only")

	res, err := r.a.Restore(context.Background(), first, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewound {
		t.Error("restore below the published tip must not rewind history")
	}
	if head := r.git(r.dir, "rev-parse", "HEAD"); head != second.String() {
		t.Errorf("HEAD moved to %s; published history must stay", head)
	}
	if got := r.read("a.txt"); got != "one\n" {
		t.Errorf("a.txt = %q, want the v1 content brought forward", got)
	}
}

func TestRestoreToPublishedTipStillRewinds(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/level")
	r.write("a.txt", "one\n")
	published := r.checkpoint("v1")
	r.publish("agent/level")
	r.write("a.txt", "two\n")
	r.checkpoint("v2, unpublished")

	res, err := r.a.Restore(context.Background(), published, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rewound {
		t.Error("rewinding an unpublished checkpoint back to the published tip is a fast-forward-safe rewind")
	}
	if head := r.git(r.dir, "rev-parse", "HEAD"); head != published.String() {
		t.Errorf("HEAD = %s, want the published tip %s", head, published)
	}
}

func TestSetAsideAndResume(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/parcel")
	r.write("README.md", "draft edit\n")
	r.write("new.txt", "untracked draft\n")
	if err := r.a.SetAside(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Clean() || st.SetAside != 1 {
		t.Fatalf("after set_aside: %+v, want clean with one parcel", st)
	}
	if err := r.a.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.read("README.md"); got != "draft edit\n" {
		t.Errorf("README.md = %q after resume, want the draft back", got)
	}
	if got := r.read("new.txt"); got != "untracked draft\n" {
		t.Errorf("new.txt = %q after resume, want the untracked file back", got)
	}
}

func TestSetAsideNothing(t *testing.T) {
	r := newRig(t)
	if err := r.a.SetAside(context.Background()); !errors.Is(err, ErrNoChanges) {
		t.Errorf("set_aside on a clean tree = %v, want ErrNoChanges", err)
	}
	if err := r.a.Resume(context.Background()); !errors.Is(err, ErrNoChanges) {
		t.Errorf("resume with nothing parked = %v, want ErrNoChanges", err)
	}
}

// mergeCount returns how many merge checkpoints the current branch holds.
func mergeCount(t *testing.T, r *rig) int {
	t.Helper()
	out := r.git(r.dir, "rev-list", "--merges", "--count", "HEAD")
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("unparseable merge count %q", out)
	}
	return n
}

func TestSyncUnpublishedRebases(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/rebased")
	r.write("a.txt", "mine\n")
	r.checkpoint("my work")
	r.upstreamCommit("main", "b.txt", "theirs\n", "upstream motion")

	res, err := r.a.SyncFromUpstream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed || res.Merged {
		t.Errorf("result = %+v, want a rebase replay", res)
	}
	if got := r.read("b.txt"); got != "theirs\n" {
		t.Errorf("upstream file missing after sync: %q", got)
	}
	if n := mergeCount(t, r); n != 0 {
		t.Errorf("unpublished sync created %d merge checkpoints, want linear history", n)
	}
}

func TestSyncPublishedMerges(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/merged")
	r.write("a.txt", "mine\n")
	r.checkpoint("my work")
	r.publish("agent/merged")
	r.upstreamCommit("main", "b.txt", "theirs\n", "upstream motion")

	res, err := r.a.SyncFromUpstream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed || !res.Merged {
		t.Errorf("result = %+v, want a merge replay on a published branch", res)
	}
	if n := mergeCount(t, r); n != 1 {
		t.Errorf("published sync should merge exactly once, got %d", n)
	}
	// The published checkpoint must remain an ancestor: append-only held.
	r.git(r.dir, "merge-base", "--is-ancestor", "origin/agent/merged", "HEAD")
}

func TestSyncConflictAbortsCleanly(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/conflicted")
	r.write("README.md", "my line\n")
	r.checkpoint("my README")
	r.upstreamCommit("main", "README.md", "their line\n", "their README")

	before := r.git(r.dir, "rev-parse", "HEAD")
	_, err := r.a.SyncFromUpstream(context.Background())
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("sync = %v, want *ConflictError", err)
	}
	if len(conflict.Files) != 1 || conflict.Files[0] != "README.md" {
		t.Errorf("conflict files = %v, want [README.md]", conflict.Files)
	}
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Clean() {
		t.Errorf("tree not restored after aborted replay: %+v", st)
	}
	if after := r.git(r.dir, "rev-parse", "HEAD"); after != before {
		t.Errorf("HEAD moved across an aborted replay: %s -> %s", before, after)
	}
}

func TestSyncRefusesDirtyTree(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/unready")
	r.write("a.txt", "uncommitted\n")
	if _, err := r.a.SyncFromUpstream(context.Background()); !errors.Is(err, ErrDirtyTree) {
		t.Errorf("sync with a dirty tree = %v, want ErrDirtyTree", err)
	}
}

func TestSyncOnDefaultFastForwards(t *testing.T) {
	r := newRig(t)
	r.upstreamCommit("main", "b.txt", "theirs\n", "upstream motion")
	res, err := r.a.SyncFromUpstream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed {
		t.Errorf("result = %+v; syncing the default branch is a fast-forward, not a replay", res)
	}
	if got := r.read("b.txt"); got != "theirs\n" {
		t.Errorf("upstream file missing after sync: %q", got)
	}
}
