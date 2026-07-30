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
	"strings"
	"testing"
)

func TestCurrentStateClean(t *testing.T) {
	r := newRig(t)
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" || st.Default != "main" {
		t.Errorf("Branch/Default = %q/%q, want main/main", st.Branch, st.Default)
	}
	if !st.Published {
		t.Error("a cloned default branch has a published counterpart")
	}
	if !st.Clean() || st.Ahead != 0 || st.Behind != 0 || st.SetAside != 0 {
		t.Errorf("fresh clone should be clean and level, got %+v", st)
	}
}

func TestCurrentStateDirtyAndUntracked(t *testing.T) {
	r := newRig(t)
	r.write("README.md", "changed\n")
	r.write("new.txt", "untracked\n")
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Dirty) != 1 || st.Dirty[0].Path != "README.md" || st.Dirty[0].Status != "modified" {
		t.Errorf("Dirty = %+v, want README.md modified", st.Dirty)
	}
	if len(st.Untracked) != 1 || st.Untracked[0] != "new.txt" {
		t.Errorf("Untracked = %v, want [new.txt]", st.Untracked)
	}
	if st.Clean() {
		t.Error("Clean() with dirty and untracked files")
	}
}

func TestCurrentStateAheadOfPublished(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/ahead")
	r.publish("agent/ahead")
	r.write("a.txt", "one\n")
	r.checkpoint("first")
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Published || st.Ahead != 1 || st.Behind != 0 {
		t.Errorf("Published/Ahead/Behind = %v/%d/%d, want true/1/0", st.Published, st.Ahead, st.Behind)
	}
}

func TestCurrentStateBehindUpstream(t *testing.T) {
	r := newRig(t)
	r.upstreamCommit("main", "b.txt", "upstream\n", "upstream motion")
	r.git(r.dir, "fetch", "origin")
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Behind != 1 || st.Ahead != 0 {
		t.Errorf("Ahead/Behind = %d/%d, want 0/1 after upstream motion", st.Ahead, st.Behind)
	}
}

func TestCurrentStateUnpublishedBranch(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/local-only")
	r.write("a.txt", "one\n")
	r.checkpoint("first")
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Published {
		t.Error("a never-published branch reports Published")
	}
	if st.Ahead != 0 || st.Behind != 0 {
		t.Errorf("Ahead/Behind = %d/%d on an unpublished branch, want 0/0", st.Ahead, st.Behind)
	}
}

func TestCurrentStateSetAsideCount(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/parked")
	r.write("a.txt", "draft\n")
	if err := r.a.SetAside(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.CurrentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.SetAside != 1 {
		t.Errorf("SetAside = %d, want 1", st.SetAside)
	}
	if !st.Clean() {
		t.Errorf("tree should be clean after set_aside, got %+v", st)
	}
}

func TestPendingChanges(t *testing.T) {
	r := newRig(t)
	r.write("README.md", "changed\n")
	r.write("new.txt", "untracked\n")
	p, err := r.a.PendingChanges(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Diff, "-hello") || !strings.Contains(p.Diff, "+changed") {
		t.Errorf("diff missing the README delta:\n%s", p.Diff)
	}
	if len(p.Untracked) != 1 || p.Untracked[0] != "new.txt" {
		t.Errorf("Untracked = %v, want [new.txt]", p.Untracked)
	}
}

func TestPendingChangesScopedToPath(t *testing.T) {
	r := newRig(t)
	r.write("README.md", "changed\n")
	r.write("keep.txt", "also changed\n")
	r.write("new.txt", "untracked\n")
	p, err := r.a.PendingChanges(context.Background(), "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Diff, "README.md") || strings.Contains(p.Diff, "keep.txt") {
		t.Errorf("path-scoped diff leaked other files:\n%s", p.Diff)
	}
	if len(p.Untracked) != 0 {
		t.Errorf("Untracked = %v, want none in scope", p.Untracked)
	}
}
