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
	"strings"
	"testing"
)

// fakeGit answers the one question classification asks the repository.
type fakeGit struct{ branches map[string]bool }

func (f *fakeGit) branchExists(name string) bool { return f.branches[name] }

func withBranches(names ...string) *fakeGit {
	b := map[string]bool{}
	for _, n := range names {
		b[n] = true
	}
	return &fakeGit{branches: b}
}

func plan_(t *testing.T, q gitQuery, argv ...string) plan {
	t.Helper()
	return classify(argv, q)
}

func TestReadsPassThrough(t *testing.T) {
	q := withBranches()
	for _, argv := range [][]string{
		{"log", "--oneline", "-20"},
		{"log", "-p", "--", "src/main.go"},
		{"show", "HEAD~3"},
		{"diff", "HEAD"},
		{"blame", "README.md"},
		{"grep", "-n", "TODO"},
		{"status", "--porcelain"},
		{"rev-parse", "HEAD"},
		{"describe", "--tags"},
		{"ls-files"},
		{"stash", "list"},
		{"branch", "--list"},
		{"reflog"},
		{"reflog", "show"},
		// Worktree-only: the next checkpoint records the tree anyway.
		{"mv", "a.txt", "b.txt"},
		{"clean", "-fd"},
		{"rm", "old.txt"},
	} {
		if p := plan_(t, q, argv...); p.verdict != pass {
			t.Errorf("git %s = verdict %v (%s), want pass", strings.Join(argv, " "), p.verdict, p.reason)
		}
	}
}

// TestToolchainReadsPassThrough pins the invocations the build tools
// actually make.  Go's -buildvcs stamping and Gradle's version plugins
// shell out to git on every build; a proxy that refused these would
// break builds rather than merely annoy the agent.
func TestToolchainReadsPassThrough(t *testing.T) {
	q := withBranches()
	for _, argv := range [][]string{
		{"status", "--porcelain"},                   // go: is the tree dirty
		{"rev-parse", "HEAD"},                       // go: the stamped revision
		{"show", "-s", "--format=%ct", "HEAD"},      // go: commit time
		{"describe", "--tags", "--always"},          // gradle version plugins
		{"rev-parse", "--show-toplevel"},            // "am I in a repo"
		{"-C", "/grange/tree", "rev-parse", "HEAD"}, // with a global option
	} {
		if p := plan_(t, q, argv...); p.verdict != pass {
			t.Errorf("toolchain call git %s = %v (%s), want pass", strings.Join(argv, " "), p.verdict, p.reason)
		}
	}
}

func TestCommitTranslatesToCheckpoint(t *testing.T) {
	q := withBranches()
	p := plan_(t, q, "commit", "-m", "add the parser")
	if p.verdict != translate || p.verb != "checkpoint" {
		t.Fatalf("commit = %v/%s, want translate to checkpoint", p.verdict, p.verb)
	}
	if p.args["message"] != "add the parser" {
		t.Errorf("message = %v", p.args["message"])
	}
	if _, ok := p.args["paths"]; ok {
		t.Errorf("a whole-tree commit should not pass paths: %v", p.args)
	}

	// The message spellings git accepts.
	for _, argv := range [][]string{
		{"commit", "-madhoc"},
		{"commit", "--message=adhoc"},
		{"commit", "--message", "adhoc"},
		{"commit", "-a", "-m", "adhoc"}, // -a is a no-op: the tree is recorded regardless
	} {
		p := plan_(t, q, argv...)
		if p.verdict != translate || p.args["message"] != "adhoc" {
			t.Errorf("git %s = %v, message %v", strings.Join(argv, " "), p.verdict, p.args["message"])
		}
	}
}

func TestCommitPathsSurvive(t *testing.T) {
	p := plan_(t, withBranches(), "commit", "-m", "partial", "--", "a.txt", "b.txt")
	if p.verdict != translate {
		t.Fatalf("verdict = %v", p.verdict)
	}
	paths, _ := p.args["paths"].([]string)
	if len(paths) != 2 || paths[0] != "a.txt" || paths[1] != "b.txt" {
		t.Errorf("paths = %v", p.args["paths"])
	}
}

// TestUnknownFlagsRefuseRatherThanDrop is the strictness rule.  A
// dropped flag turns the agent's request into a different request and
// reports success for it — the exact failure the proxy exists to stop.
func TestUnknownFlagsRefuseRatherThanDrop(t *testing.T) {
	q := withBranches("agent/x")
	for _, argv := range [][]string{
		{"commit", "-m", "msg", "--author=someone@example.com"},
		{"commit", "-m", "msg", "--date=2020-01-01"},
		{"commit", "-m", "msg", "-S"},
		{"commit", "--amend", "--no-edit"},
		{"push", "--force"},
		{"push", "--delete", "agent/x"},
		{"push", "origin", "HEAD:refs/heads/other"},
		{"checkout", "--orphan", "thing"},
		{"branch", "--set-upstream-to=origin/main"},
	} {
		p := plan_(t, q, argv...)
		if p.verdict != refuse {
			t.Errorf("git %s = %v, want refuse (a flag we cannot honour must not be dropped)", strings.Join(argv, " "), p.verdict)
		}
		if p.reason == "" {
			t.Errorf("git %s refused with no reason", strings.Join(argv, " "))
		}
	}
}

func TestCommitWithoutAMessageRefuses(t *testing.T) {
	p := plan_(t, withBranches(), "commit")
	if p.verdict != refuse || !strings.Contains(p.reason, "message") {
		t.Errorf("bare commit = %v (%s), want a refusal naming the missing message", p.verdict, p.reason)
	}
}

func TestPushTranslatesToPublish(t *testing.T) {
	q := withBranches("agent/x")
	for _, argv := range [][]string{
		{"push"},
		{"push", "origin"},
		{"push", "-u", "origin", "agent/x"},
	} {
		p := plan_(t, q, argv...)
		if p.verdict != translate || p.verb != "publish" {
			t.Errorf("git %s = %v/%s, want translate to publish", strings.Join(argv, " "), p.verdict, p.verb)
		}
	}
}

// TestCheckoutIsBranchOrPathDependingOnTheRepository: `git checkout X`
// is genuinely ambiguous, and only the repository can say which it is.
func TestCheckoutIsBranchOrPathDependingOnTheRepository(t *testing.T) {
	q := withBranches("agent/existing", "main")

	if p := plan_(t, q, "checkout", "agent/existing"); p.verdict != translate || p.verb != "switch_work" {
		t.Errorf("checkout of a real branch = %v/%s, want switch_work", p.verdict, p.verb)
	}
	if p := plan_(t, q, "switch", "main"); p.verdict != translate || p.verb != "switch_work" {
		t.Errorf("switch to a real branch = %v/%s", p.verdict, p.verb)
	}
	if p := plan_(t, q, "checkout", "-b", "agent/fresh"); p.verdict != translate || p.verb != "start_work" {
		t.Errorf("checkout -b = %v/%s, want start_work", p.verdict, p.verb)
	} else if p.args["name"] != "agent/fresh" {
		t.Errorf("name = %v", p.args["name"])
	}
	if p := plan_(t, q, "checkout", "--", "README.md"); p.verdict != translate || p.verb != "restore" {
		t.Errorf("checkout -- path = %v/%s, want restore", p.verdict, p.verb)
	}
	// Neither a branch nor an explicit path: refuse rather than guess.
	if p := plan_(t, q, "checkout", "nonesuch"); p.verdict != refuse {
		t.Errorf("checkout of an unknown ref = %v, want refuse", p.verdict)
	}
}

func TestStashAndRestore(t *testing.T) {
	q := withBranches()
	for _, tc := range []struct {
		argv []string
		verb string
	}{
		{[]string{"stash"}, "set_aside"},
		{[]string{"stash", "push"}, "set_aside"},
		{[]string{"stash", "pop"}, "resume"},
		{[]string{"restore", "README.md"}, "restore"},
		{[]string{"branch", "-D", "agent/dead"}, "abandon_work"},
		{[]string{"pull"}, "sync_from_upstream"},
		{[]string{"merge", "origin/main"}, "sync_from_upstream"},
	} {
		p := plan_(t, q, tc.argv...)
		if p.verdict != translate || p.verb != tc.verb {
			t.Errorf("git %s = %v/%s, want %s", strings.Join(tc.argv, " "), p.verdict, p.verb, tc.verb)
		}
	}
}

// TestRefusalsNameTheAlternative: a refusal that only says no teaches
// nothing.  Each one has to leave the reader knowing what to do next.
func TestRefusalsNameTheAlternative(t *testing.T) {
	q := withBranches()
	for _, tc := range []struct {
		argv []string
		want string // a word the reason must contain
	}{
		{[]string{"add", "."}, "checkpoint"},
		{[]string{"rebase", "-i", "HEAD~3"}, "append-only"},
		{[]string{"reset", "--hard", "HEAD~1"}, "restore"},
		{[]string{"revert", "HEAD"}, "restore"},
		{[]string{"fetch"}, "sync_from_upstream"},
		{[]string{"remote", "add", "other", "https://example.com"}, "endpoint"},
		{[]string{"tag", "v1.0"}, "release"},
		{[]string{"rm", "--cached", "a.txt"}, "staging"},
		{[]string{"clone", "https://example.com/x"}, "provisioned"},
		{[]string{"config", "user.email", "x@y.z"}, "provision"},
	} {
		p := plan_(t, q, tc.argv...)
		if p.verdict != refuse {
			t.Fatalf("git %s = %v, want refuse", strings.Join(tc.argv, " "), p.verdict)
		}
		if !strings.Contains(p.reason, tc.want) {
			t.Errorf("git %s refusal %q does not mention %q", strings.Join(tc.argv, " "), p.reason, tc.want)
		}
	}
}

// TestUnknownCommandRefuses: an unrecognized command is a loud error we
// see in the log immediately, not a silent bypass.
func TestUnknownCommandRefuses(t *testing.T) {
	p := plan_(t, withBranches(), "cherry", "-v")
	if p.verdict != refuse || !strings.Contains(p.reason, "operator") {
		t.Errorf("unknown command = %v (%s), want a refusal pointing at the operator", p.verdict, p.reason)
	}
}

// TestGlobalOptionsDoNotSwallowTheSubcommand: `git -c k=v commit` must
// still be seen as commit.  Skipping a value-taking flag wrongly is how
// a mutating command sneaks past as something else.
func TestGlobalOptionsDoNotSwallowTheSubcommand(t *testing.T) {
	q := withBranches()
	if p := plan_(t, q, "-C", "/grange/tree", "commit", "-m", "x"); p.verdict != translate || p.verb != "checkpoint" {
		t.Errorf("-C then commit = %v/%s", p.verdict, p.verb)
	}
	if p := plan_(t, q, "-c", "user.name=x", "commit", "-m", "x"); p.verdict != translate || p.verb != "checkpoint" {
		t.Errorf("-c then commit = %v/%s", p.verdict, p.verb)
	}
	if p := plan_(t, q, "--no-pager", "log"); p.verdict != pass {
		t.Errorf("--no-pager then log = %v, want pass", p.verdict)
	}
}
