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
	"strconv"
	"strings"
	"testing"
)

// TestDisplayQuotesWhatWouldRead AsSeveralArguments: the refusal echoes
// the command back, and `git commit -m "fix the parser"` rendered as
// four bare words misleads the reader about what was rejected.
func TestDisplayQuotesWhatWouldReadAsSeveralArguments(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"commit", "-m", "fix the parser"}, "`git commit -m \"fix the parser\"`"},
		{[]string{"log", "--oneline"}, "`git log --oneline`"},
		{[]string{"commit", "-m", ""}, "`git commit -m \"\"`"},
		{[]string{"grep", "a\tb"}, "`git grep \"a\\tb\"`"},
		{[]string{}, "`git`"},
	} {
		if got := display(tc.argv); got != tc.want {
			t.Errorf("display(%q) = %s, want %s", tc.argv, got, tc.want)
		}
	}
}

// TestDisplayDoesNotSwallowNewlines: an argument containing a newline
// would otherwise break the message into what looks like two lines of
// output, one of them unattributed.
func TestDisplayDoesNotSwallowNewlines(t *testing.T) {
	got := display([]string{"commit", "-m", "one\ntwo"})
	if strings.Count(got, "\n") != 0 {
		t.Errorf("display kept a raw newline: %q", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("display(%q) = %s; the newline should survive as an escape", "one\ntwo", got)
	}
}

func TestWrapBreaksAtTheColumn(t *testing.T) {
	long := strings.Repeat("word ", 40)
	for _, line := range strings.Split(wrap(long), "\n") {
		if len(line) > 72 {
			t.Errorf("line of %d columns: %q", len(line), line)
		}
	}
	// Short text is left alone.
	if got := wrap("a short reason"); got != "a short reason" {
		t.Errorf("wrap(short) = %q", got)
	}
	// A single word longer than the column budget is emitted rather than
	// dropped — a truncated refusal would be worse than an ugly one.
	huge := strings.Repeat("x", 100)
	if !strings.Contains(wrap("see "+huge), huge) {
		t.Error("wrap dropped an over-long word")
	}
}

// TestWrapPreservesEveryWord: reflowing must not lose content.  A
// refusal is the only thing the reader gets.
func TestWrapPreservesEveryWord(t *testing.T) {
	const reason = "there is no staging area here — the archivist MCP tool checkpoint() " +
		"records the working tree as it stands, so there is nothing to stage."
	if got, want := strings.Fields(wrap(reason)), strings.Fields(reason); len(got) != len(want) {
		t.Fatalf("wrap changed the word count: %d → %d", len(want), len(got))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("word %d: got %q, want %q", i, got[i], want[i])
			}
		}
	}
}

// TestRefusesToExecItself: the proxy is installed AS `git`, so any
// resolution of the real binary that lands back on this program forks
// until the machine dies.  It is not a theoretical failure — a
// PATH-relative target did exactly that, and it took eleven thousand
// processes before anyone noticed.
func TestRefusesToExecItself(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate the test binary")
	}
	saved := realGit
	t.Cleanup(func() { realGit = saved })

	// The same file, by its own path.
	realGit = self
	if err := checkRealGit(); err == nil {
		t.Error("exec of this very binary was allowed")
	} else if !strings.Contains(err.Error(), "recurse") {
		t.Errorf("error %q does not explain the danger", err)
	}

	// The same file under a different name — a hardlink, which a path
	// comparison would miss and which is exactly how the real git is
	// reachable twice in this image.
	//
	// Not t.TempDir: a hardlink to the RUNNING binary cannot be unlinked
	// on Windows, and its cleanup failure would fail an otherwise
	// passing test.
	dir, err := os.MkdirTemp("", "gitproxy-selflink")
	if err != nil {
		t.Skip("no temp dir")
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) }) // may fail while the binary is mapped
	link := filepath.Join(dir, "not-git")
	if err := os.Link(self, link); err != nil {
		t.Skipf("hardlinks unavailable here (%v); the same-path case above still holds", err)
	}
	realGit = link
	if err := checkRealGit(); err == nil {
		t.Error("exec of a hardlink to this binary was allowed; SameFile should catch it")
	}
}

// TestRefusesARelativeRealGit: a bare name would be resolved through
// PATH, where this program is installed as `git`.
func TestRefusesARelativeRealGit(t *testing.T) {
	saved := realGit
	t.Cleanup(func() { realGit = saved })
	for _, p := range []string{"git", "./git", "bin/git"} {
		realGit = p
		err := checkRealGit()
		if err == nil {
			t.Errorf("realGit=%q was accepted; PATH resolution would find this proxy", p)
			continue
		}
		if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("realGit=%q: error %q does not name the problem", p, err)
		}
	}
}

func TestDepthCountsAndCaps(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", 0},
		{"1", 1},
		{"7", 7},
		{"garbage", maxDepth}, // unreadable counts as the cap: stop
		{"-3", maxDepth},
	} {
		t.Setenv(depthEnv, tc.env)
		if got := depth(); got != tc.want {
			t.Errorf("depth() with %s=%q = %d, want %d", depthEnv, tc.env, got, tc.want)
		}
	}
}

// TestAtMaxDepthNothingRuns: the runaway stop comes before the escape
// hatch, because passthrough is the mode that execs most eagerly.
func TestAtMaxDepthNothingRuns(t *testing.T) {
	t.Setenv(depthEnv, strconv.Itoa(maxDepth))
	saved := realGit
	t.Cleanup(func() { realGit = saved })
	realGit = filepath.Join(t.TempDir(), "never-exists")

	if code := run([]string{"log"}); code != 1 {
		t.Errorf("run at max depth = %d, want 1", code)
	}
}

func TestVerdictStringsAreWords(t *testing.T) {
	if pass.String() != "pass" || refuse.String() != "refuse" {
		t.Errorf("pass=%v refuse=%v", pass, refuse)
	}
	if got := verdict(99).String(); !strings.Contains(got, "99") {
		t.Errorf("unknown verdict = %q, want it to name the number", got)
	}
}
