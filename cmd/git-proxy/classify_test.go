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
	"regexp"
	"strings"
	"testing"
)

// allowed asserts a command runs the real git.
func allowed(t *testing.T, argv ...string) {
	t.Helper()
	t.Run(strings.Join(argv, " "), func(t *testing.T) {
		if p := classify(argv); p.verdict != pass {
			t.Errorf("got %v (%s), want pass", p.verdict, p.reason)
		}
	})
}

// denied asserts a command is refused, and that the refusal says
// something.  want, if non-empty, must appear in the reason.
func denied(t *testing.T, want string, argv ...string) {
	t.Helper()
	t.Run(strings.Join(argv, " "), func(t *testing.T) {
		p := classify(argv)
		if p.verdict != refuse {
			t.Fatalf("got %v, want refuse", p.verdict)
		}
		if p.reason == "" {
			t.Fatal("refused with no reason")
		}
		if want != "" && !strings.Contains(p.reason, want) {
			t.Errorf("reason %q does not mention %q", p.reason, want)
		}
	})
}

func TestReadsPassThrough(t *testing.T) {
	for _, argv := range [][]string{
		{"log", "--oneline", "-20"},
		{"log", "-p", "--", "src/main.go"},
		{"show", "HEAD~3"},
		{"diff", "HEAD"},
		{"diff", "--stat", "main...HEAD"},
		{"blame", "README.md"},
		{"grep", "-n", "TODO"},
		{"status", "--porcelain"},
		{"rev-parse", "HEAD"},
		{"describe", "--tags"},
		{"ls-files"},
		{"ls-tree", "-r", "HEAD"},
		{"merge-base", "main", "HEAD"},
		{"cherry", "-v"},
		{"shortlog", "-sn"},
		{"for-each-ref", "refs/heads"},
	} {
		allowed(t, argv...)
	}
}

// TestToolchainReadsPassThrough pins the invocations the build tools
// actually make.  Go's -buildvcs stamping and Gradle's version plugins
// shell out to git on every build; a proxy that refused these would
// break builds rather than merely annoy the agent.
func TestToolchainReadsPassThrough(t *testing.T) {
	for _, argv := range [][]string{
		{"status", "--porcelain"},
		{"rev-parse", "HEAD"},
		{"rev-parse", "--show-toplevel"},
		{"describe", "--tags", "--always"},
		{"-C", "/grange/tree", "rev-parse", "HEAD"},
		// Go's buildvcs stamping, verbatim.
		{"-c", "log.showsignature=false", "log", "-1", "--format=%H:%ct"},
		// Build-scan and release tooling reach for these constantly.
		{"config", "--get", "remote.origin.url"},
		// The BARE read form, which is what Go's -buildvcs stamping
		// actually runs (cmd/go/internal/vcs.gitRemoteRepo).  Observed,
		// not transcribed: an earlier version of classifyConfig allowed
		// only the --get spelling and broke every Go build in the cell.
		{"config", "remote.origin.url"},
		{"config", "user.name"},
		{"config", "--local", "remote.origin.url"},
		{"remote", "-v"},
		{"remote", "get-url", "origin"},
		{"branch", "--show-current"},
	} {
		allowed(t, argv...)
	}
}

// TestReadFormsOfSplitCommands: several commands are worth having for
// their read forms alone, and each must not carry its write forms in
// with it.
func TestReadFormsOfSplitCommands(t *testing.T) {
	for _, argv := range [][]string{
		{"branch"}, {"branch", "-a"}, {"branch", "-vv"}, {"branch", "--merged"},
		{"config", "--list"}, {"config", "--get-regexp", "^remote"},
		{"remote"}, {"remote", "show"},
		{"stash", "list"}, {"stash", "show"},
		{"reflog"}, {"reflog", "show"},
		{"symbolic-ref", "--short", "HEAD"},
		{"worktree", "list"}, {"submodule", "status"}, {"notes", "list"},
	} {
		allowed(t, argv...)
	}
	for _, tc := range []struct {
		want string
		argv []string
	}{
		{"start_work", []string{"branch", "newthing"}},
		{"abandon_work", []string{"branch", "-D", "agent/x"}},
		{"switch_work", []string{"branch", "-m", "old", "new"}},
		{"provision", []string{"config", "user.email", "x@y.z"}},
		{"changes configuration", []string{"config", "--unset", "user.email"}},
		{"changes configuration", []string{"config", "--add", "remote.origin.url", "x"}},
		{"changes configuration", []string{"config", "--global", "--replace-all", "core.pager", "sh"}},
		{"endpoint table", []string{"remote", "add", "other", "https://example.com"}},
		{"set_aside", []string{"stash"}},
		{"resume", []string{"stash", "pop"}},
		{"maintenance", []string{"reflog", "expire", "--all"}},
		{"archivist", []string{"symbolic-ref", "HEAD", "refs/heads/other"}},
		{"grange", []string{"worktree", "add", "/tmp/wt"}},
		{"", []string{"submodule", "update", "--init"}},
		{"", []string{"notes", "add", "-m", "x"}},
		{"publish", []string{"bundle", "create", "out.bundle", "HEAD"}},
	} {
		denied(t, tc.want, tc.argv...)
	}
}

// TestEveryMutatingCommandIsRefused is the safety property in one
// place.  These are the commands whose translation was removed; each
// must now refuse and name the tool that replaces it.
func TestEveryMutatingCommandIsRefused(t *testing.T) {
	for _, tc := range []struct {
		want string
		argv []string
	}{
		{"checkpoint", []string{"commit", "-m", "add the parser"}},
		{"checkpoint", []string{"commit", "-am", "add the parser"}},
		{"checkpoint", []string{"commit", "--amend"}},
		{"publish", []string{"push"}},
		{"publish", []string{"push", "-u", "origin", "agent/x"}},
		{"publish", []string{"push", "--force"}},
		{"start_work", []string{"checkout", "-b", "agent/x"}},
		{"switch_work", []string{"checkout", "main"}},
		{"restore", []string{"checkout", "--", "README.md"}},
		{"restore", []string{"checkout", "main", "--", "README.md"}},
		{"start_work", []string{"switch", "-c", "agent/x"}},
		{"restore", []string{"restore", "a.txt", "b.txt"}},
		{"sync_from_upstream", []string{"pull"}},
		{"sync_from_upstream", []string{"merge", "origin/main"}},
		{"sync_from_upstream", []string{"merge", "some-feature"}},
		{"restore", []string{"reset", "--hard", "HEAD~1"}},
		{"append-only", []string{"rebase", "-i", "HEAD~3"}},
		{"checkpoint", []string{"cherry-pick", "abc123"}},
		{"restore", []string{"revert", "HEAD"}},
		{"checkpoint", []string{"add", "."}},
		{"rm", []string{"rm", "old.txt"}},
		{"mv", []string{"mv", "a.txt", "b.txt"}},
		{"rm", []string{"clean", "-fd"}},
		{"release", []string{"tag", "v1.0"}},
		{"sync_from_upstream", []string{"fetch"}},
		{"provisioned", []string{"clone", "https://example.com/x"}},
	} {
		denied(t, tc.want, tc.argv...)
	}
}

// TestNoDropSemantics: the invocations that USED to translate into
// something subtly different.  Each is now a refusal, which is the
// whole reason translation was removed — a refusal cannot silently do
// the wrong thing.
func TestNoDropSemantics(t *testing.T) {
	for _, argv := range [][]string{
		{"checkout", "main", "--", "config.go"},       // once became switch_work(main)
		{"commit", "-m", "title", "-m", "body"},       // once dropped the subject
		{"restore", "a.txt", "b.txt"},                 // once restored only a.txt
		{"checkout", "--", "a.txt", "b.txt"},          // likewise
		{"branch", "-d", "one", "two"},                // once deleted only "two"
		{"checkout", "-b", "agent/x", "main"},         // once ignored the start point
		{"push", "origin", "some-other-branch"},       // once published the current branch
		{"merge", "some-feature"},                     // once synced the default branch
		{"stash", "push", "-m", "wip", "--", "a.txt"}, // once parked the whole tree
		{"stash", "pop", "stash@{2}"},                 // once popped the newest
		{"stash", "apply"},                            // once consumed the parcel
	} {
		denied(t, "", argv...)
	}
}

// TestGlobalOptionsCannotSmuggleASubcommand is the highest-severity
// case found in review: subcommand parsing used to skip any leading
// flag it did not recognize, so the VALUE of a value-taking global
// option landed in the subcommand slot.  `git --namespace log commit
// -am x` classified as `log`, passed as a read, and performed a real
// commit that nothing logged.
func TestGlobalOptionsCannotSmuggleASubcommand(t *testing.T) {
	for _, argv := range [][]string{
		{"--namespace", "log", "commit", "-am", "sneak"},
		{"--git-dir", "log", "commit", "-m", "sneak"},
		{"--work-tree", "status", "commit", "-m", "sneak"},
		{"--config-env", "log", "push"},
		{"--super-prefix", "log", "commit", "-m", "x"},
		{"--exec-path=/tmp", "push"},
		{"--git-dir=/x/y", "commit", "-m", "x"},
		{"--help", "push"},  // --help was skipped; push then translated
		{"--help", "stash"}, // …and this parked the tree
		{"--help", "pull"},
	} {
		denied(t, "", argv...)
	}
	// The globals that ARE understood keep working.
	allowed(t, "-C", "/grange/tree", "status")
	allowed(t, "-C/grange/tree", "status")
	allowed(t, "--no-pager", "log")
	allowed(t, "-p", "log")
	allowed(t, "--literal-pathspecs", "grep", "x")
	// A global option with no subcommand at all is git's own usage text.
	allowed(t, "--version")
	allowed(t)
}

// TestConfigCannotSmuggleExecution: `-c core.pager=…` turns any read
// into arbitrary execution wearing git's name.  The agent already has a
// shell, so this is about CONCEALMENT — a command that runs should look
// like what it is in the transcript.
func TestConfigCannotSmuggleExecution(t *testing.T) {
	for _, argv := range [][]string{
		{"-c", "core.pager=sh -c 'curl evil'", "log"},
		{"-c", "core.editor=vim", "log"},
		{"-c", "diff.external=/bin/sh", "diff"},
		{"-c", "core.sshCommand=/bin/sh", "log"},
		{"-c", "alias.x=!sh", "log"},
		{"-c", "credential.helper=/bin/sh", "log"},
		{"-ccore.pager=sh", "log"},     // attached form
		{"-c", "CORE.PAGER=sh", "log"}, // case-insensitive, as git config is
	} {
		denied(t, "", argv...)
	}
	// Innocuous config still passes — the toolchains depend on it.
	allowed(t, "-c", "log.showsignature=false", "log")
	allowed(t, "-c", "core.quotepath=false", "status")
}

// TestReadsCannotWriteOrExecuteViaFlags: several allow-listed reads
// accept a flag that writes a file or runs a program.  The allow-list
// is the entire safety property now, so these must not ride in on it.
func TestReadsCannotWriteOrExecuteViaFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"log", "-1", "--output=.git/HEAD"},
		{"diff", "--output=OUT.txt"},
		{"show", "--output", "x"},
		{"grep", "-Osh", "TODO"},
		{"grep", "--open-files-in-pager", "TODO"},
		{"diff", "--ext-diff"},
	} {
		denied(t, "", argv...)
	}
}

// TestUnknownCommandRefuses: an unrecognized command is refused without
// being examined, which is the point — a git version that adds a
// command fails closed rather than through.
func TestUnknownCommandRefuses(t *testing.T) {
	denied(t, "read-only list", "nosuchcommand")
	denied(t, "read-only list", "maintenance", "run")
	denied(t, "read-only list", "format-patch", "-1")
	denied(t, "read-only list", "range-diff", "main...HEAD")
}

// TestVerbsAreNamedAsArchivistToolsNotShellCommands: a bare
// `checkpoint` in a refusal reads like a git subcommand that happens
// not to exist yet, and the reader's next move is to try spelling it
// differently.  Every mention has to say it is an MCP tool call.
func TestVerbsAreNamedAsArchivistToolsNotShellCommands(t *testing.T) {
	// Several tool names are also ordinary nouns in this domain —
	// "the archivist's history is append-only", "a checkpoint to roll
	// back to" — so a bare word-boundary match cannot tell a reference
	// from prose.  What it CAN tell, and what the original defect
	// actually looked like, is a verb offered to the reader as a thing
	// to invoke: backticked, or the object of "call"/"use".  Those must
	// carry the tool() form; the same word used as English is fine.
	verbs := []string{
		"checkpoint", "publish", "start_work", "switch_work", "abandon_work",
		"restore", "set_aside", "resume", "sync_from_upstream", "history",
		"show_change", "current_state",
	}
	res := make(map[string]*regexp.Regexp, len(verbs))
	for _, v := range verbs {
		res[v] = regexp.MustCompile("(`" + v + "`|\\b(?:call|calls|calling|use|uses|using)\\s+" + v + "\\b)")
	}

	var reasons []string
	for _, why := range refusals {
		reasons = append(reasons, why)
	}
	for _, argv := range [][]string{
		{"commit", "-m", "x"}, {"push"}, {"checkout", "main"}, {"switch", "x"},
		{"branch", "newthing"}, {"branch", "-D", "x"}, {"config", "a", "b"},
		{"remote", "add", "x", "y"}, {"stash"}, {"stash", "pop"},
		{"reflog", "expire"}, {"symbolic-ref", "HEAD", "x"}, {"worktree", "add", "x"},
		{"bundle", "create", "x"}, {"nosuchcommand"}, {"--badflag", "log"},
		{"log", "--output=x"}, {"-c", "core.pager=sh", "log"},
	} {
		if p := classify(argv); p.verdict == refuse {
			reasons = append(reasons, p.reason)
		}
	}

	for _, reason := range reasons {
		for v, re := range res {
			if m := re.FindString(reason); m != "" && !strings.Contains(m, v+"()") {
				t.Errorf("refusal offers %q as something to invoke, without the tool() form:\n  in: %s\n  saw: %s", v, reason, m)
			}
		}
	}
}

// TestEveryRefusalSaysSomething: a refusal with an empty reason is a
// dead end for the reader.  Sweep the whole static table.
func TestEveryRefusalSaysSomething(t *testing.T) {
	for cmd, why := range refusals {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s refuses with no reason", cmd)
		}
	}
}

// TestPassRequiresAKnownRead is the structural guard: nothing reaches
// pass except through the read allow-list or a split command's read
// form.  It is the invariant the whole design now rests on, so it is
// asserted directly rather than inferred from the cases above.
func TestPassRequiresAKnownRead(t *testing.T) {
	for _, argv := range [][]string{
		{"commit", "-m", "x"}, {"push"}, {"merge", "x"}, {"rebase"},
		{"--namespace", "log", "commit"}, {"-c", "core.pager=sh", "log"},
		{"log", "--output=x"}, {"branch", "x"}, {"config", "a", "b"},
		{"stash"}, {"rm", "x"}, {"mv", "a", "b"}, {"clean", "-f"},
		{"nosuchcommand"}, {"checkout", "main", "--", "f"},
	} {
		if p := classify(argv); p.verdict == pass {
			sub, _, _ := subcommand(argv)
			t.Errorf("git %s passed; resolved subcommand %q, reads[%q]=%v",
				strings.Join(argv, " "), sub, sub, reads[sub])
		}
	}
}
