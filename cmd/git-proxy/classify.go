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
	"fmt"
	"strings"

	"github.com/jeffbstewart/cloister/internal/verbs"
)

// Classification (docs/git-proxy.md).  ONE question, asked of every
// invocation: is this a git command we positively know to be read-only?
// If yes it runs unchanged.  Everything else is refused with a reason.
//
// There is deliberately no third answer.  An earlier design translated
// a "closed core" of mutating commands into archivist MCP tool calls,
// and every review lens found the same class of defect in it: git's
// argument grammar is rich enough that a translator either implements
// it faithfully or silently performs a DIFFERENT operation than the one
// asked for.  `git checkout main -- file.go` moved HEAD instead of
// restoring a file; `git commit -m title -m body` dropped the subject;
// `git merge other` synced the default branch instead; multi-path
// restores discarded every path but the first.  Each looked like
// success.  Translation was buying convenience by reintroducing exactly
// the divergence-of-belief the whole system exists to prevent, so it is
// gone.  Mutation goes through the archivist's MCP tools, which the
// agent calls itself, with the full answer rather than a rendering.

type verdict int

const (
	// refuse is the ZERO VALUE, so a plan nobody filled in fails closed.
	// The safety property here is "nothing mutating passes", and a
	// default of pass would make every future coding slip open the gate.
	refuse verdict = iota
	pass
)

// String names the verdict, so a test failure and the internal-error
// path both read as words rather than as an integer nobody can decode
// without counting the iota block.
func (v verdict) String() string {
	switch v {
	case pass:
		return "pass"
	case refuse:
		return "refuse"
	}
	return fmt.Sprintf("verdict(%d)", int(v))
}

// plan is what classify decides about one invocation.
type plan struct {
	verdict verdict
	reason  string // when refuse: the explanation, naming the alternative
}

func allow() plan             { return plan{verdict: pass} }
func deny(reason string) plan { return plan{reason: reason} }

// tool renders an archivist verb as a NAME, never as anything that
// could be typed.
//
// The first version of this wrote `checkpoint()`, and that was a real
// mistake with a real cost.  These messages arrive on stderr, in the
// shell channel — where every line an agent reads is the output of
// something it just ran.  A function-call spelling in that frame is an
// invitation, and "call checkpoint()" is an imperative whose only
// available reading is "run it".  A live session did exactly that:
// refused `git commit`, then tried `checkpoint` at the shell, then
// `file_at`, then `start_work`, burning turns until its context filled
// with failures and the model came apart.
//
// So: quoted name, no parentheses, and no verb that means "execute".
// The refusal frame (see main.go) carries one emphatic sentence saying
// these are not programs — which belongs there, once, rather than
// repeated into every reason.
//
// Names come from internal/verbs, so a renamed tool is a compile error
// here rather than a refusal pointing at something that no longer
// exists.
func tool(name string) string { return `the archivist MCP tool "` + name + `"` }

// bare is a later mention in a sentence whose first mention already
// used tool(): the quoted name alone, once the register is established.
// Still quoted, never parenthesized — see tool() for what that cost.
func bare(name string) string { return `"` + name + `"` }

// tools renders several at once, for the cases where the names share a
// clause rather than each taking their own.
func tools(names ...string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return tool(names[0])
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = `"` + n + `"`
	}
	return "the archivist MCP tools " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// reads are the commands that cannot alter the repository under any
// arguments — the entire pass set.  A closed allow-list is the whole
// safety property: anything absent is refused, so a git version that
// adds a command, or a command whose writing form we overlooked,
// fails closed rather than through.
//
// Toolchains live here too: Go's -buildvcs stamping runs log/status/
// rev-parse, and Gradle version plugins run describe.  Those are the
// invocations that must never break, and cmd/git-proxy/classify_test.go
// pins them.
var reads = map[string]bool{
	"annotate": true, "blame": true, "cat-file": true, "check-attr": true,
	"check-ignore": true, "check-mailmap": true, "cherry": true,
	"count-objects": true, "describe": true, "diff": true, "diff-files": true,
	"diff-index": true, "diff-tree": true, "difftool": true, "for-each-ref": true,
	"fsck": true, "get-tar-commit-id": true, "grep": true, "help": true,
	"log": true, "ls-files": true, "ls-remote": true, "ls-tree": true,
	"merge-base": true, "merge-tree": true, "name-rev": true, "rev-list": true,
	"rev-parse": true, "shortlog": true, "show": true, "show-branch": true,
	"show-index": true, "show-ref": true, "status": true, "unpack-file": true,
	"var": true, "verify-commit": true, "verify-tag": true, "version": true,
	"whatchanged": true,
}

// splitPersonality are commands whose READ forms are worth having and
// whose write forms must not slip through with them.  Each decides for
// itself; the default within each is refuse.
var splitPersonality = map[string]func(args []string) plan{
	"branch":       classifyBranch,
	"config":       classifyConfig,
	"remote":       classifyRemote,
	"stash":        classifyStash,
	"reflog":       classifyReflog,
	"symbolic-ref": classifySymbolicRef,
	"notes":        classifyNotes,
	"worktree":     classifyWorktree,
	"submodule":    classifySubmodule,
	"bundle":       classifyBundle,
}

// refusals are the commands with a known archivist counterpart or a
// known reason for absence, mapped to that reason.  Being explicit here
// (rather than falling through to the generic message) is what makes a
// refusal useful: it names the tool to call, or why the operation does
// not exist, which is usually a design decision rather than an
// oversight.
var refusals = map[string]string{
	"add": "there is no staging area here — " + tool(verbs.Checkpoint) + " records the working tree as it stands, " +
		"so there is nothing to stage.  Just edit the files and call it.",
	"am":    "patch application is not part of the archivist's model; edit the files and use " + tool(verbs.Checkpoint) + ".",
	"apply": "patch application is not part of the archivist's model; edit the files and use " + tool(verbs.Checkpoint) + ".",
	// git 2.49+.  Modifies no refs, HEAD, index, or working tree — but it
	// downloads objects from the remote, and remote access is the
	// archivist's alone.  Moot in practice: a grange is a full clone, so
	// there are no missing blobs to fetch.
	"backfill": "this downloads objects from the remote, and remote access is the archivist's alone.  " +
		"Your workspace is a full clone in any case, so there is nothing missing to fetch.",
	"bisect": "bisect drives HEAD through a search, and the archivist owns HEAD.  Reason about changes with " + tools(verbs.History, verbs.ShowChange) + " instead.",
	"checkout": "checkout means three different things depending on its arguments, and guessing wrong moves HEAD.  Say which you mean with " +
		tools(verbs.StartWork, verbs.SwitchWork, verbs.Restore) + ": begin a line of work, move to an existing one, or put a file back.",
	"cherry-pick":   "no cherry-pick verb: the archivist's history is append-only.  Make the change and record it with " + tool(verbs.Checkpoint) + ".",
	"clean":         "use plain `rm` — the working tree is yours to edit directly, and " + tool(verbs.Checkpoint) + " records whatever it finds.",
	"clone":         "this workspace is provisioned for you and cannot be changed; there is no route to a forge from here either.",
	"commit":        "use " + tool(verbs.Checkpoint) + " instead — it records the whole working tree, refuses the default branch, and validates the message.",
	"fetch":         "remote access is the archivist's alone; " + tool(verbs.SyncFromUpstream) + " brings the default branch forward.",
	"filter-branch": "history rewriting has no counterpart here, deliberately — published work must not change under a reviewer.",
	"gc":            "repository maintenance is not the agent's concern; the workspace is destroyed after the task.",
	// git 2.54+, experimental: `history reword` and `history split`
	// rewrite commits and move every descendant branch onto the rewrite.
	// Gentler than rebase -i, and refused for the same reason.
	// Note the collision: this git command REWRITES, while the archivist
	// tool of the same name READS.  Say so, or an agent reading the
	// refusal concludes the tool it already has is forbidden too.
	"history": "`git history` rewrites commits — reword and split both replace them and carry the branches over.  " +
		"The archivist keeps history append-only: checkpoints accumulate and " + tool(verbs.Restore) + " rolls back, " +
		"because published work must not change under a reviewer.  To fix a message, record the correction forward.  " +
		"(Unrelated to " + bare(verbs.History) + ", the archivist tool of the same name, which only reads.)",
	"init":            "this workspace is already a provisioned clone, and a second repository inside it would not be published.",
	"merge":           "merging an arbitrary branch has no counterpart.  " + tool(verbs.SyncFromUpstream) + " is the one integration the archivist performs: it brings the default branch forward under your line of work.",
	"mv":              "use plain `mv` — the working tree is yours to edit directly, and " + tool(verbs.Checkpoint) + " records the rename as it finds it.",
	"pull":            "remote access is the archivist's alone; " + tool(verbs.SyncFromUpstream) + " brings the default branch forward.",
	"push":            "use " + tool(verbs.Publish) + " instead — it pushes the line of work you are on, and there is no credential in this container for git to use.",
	"rebase":          "the archivist's history is append-only: checkpoints accumulate and " + tool(verbs.Restore) + " rolls back.  There is no rewrite verb, deliberately — published work must not change under a reviewer.",
	"replace":         "object replacement has no counterpart here.",
	"reset":           "use " + tool(verbs.Restore) + " to roll back — it knows whether the branch is published, and rewrites history only while it is still private.",
	"restore":         "use " + tool(verbs.Restore) + " instead: it takes one path, or a checkpoint to roll the tree back to.",
	"revert":          "there is no revert verb: use " + tool(verbs.Restore) + " to reach an earlier checkpoint, then " + bare(verbs.Checkpoint) + " to record the result forward.",
	"rm":              "use plain `rm` — the working tree is yours to edit directly, and " + tool(verbs.Checkpoint) + " records the deletion as it finds it.",
	"sparse-checkout": "the grange is a full checkout; narrowing it would hide files from the checkpoint that records them.",
	"switch":          "use " + tool(verbs.StartWork) + " to begin a line of work (with no name it mints one), or " + bare(verbs.SwitchWork) + " to move to an existing one.",
	"tag":             "tags are a release act, and releases are the human's, not the agent's.",
	"update-ref":      "refs are the archivist's to move; see " + tools(verbs.StartWork, verbs.SwitchWork, verbs.Checkpoint) + ".",
}

// classify decides what to do with one git invocation.  argv excludes
// the program name.
func classify(argv []string) plan {
	sub, args, err := subcommand(argv)
	if err != "" {
		return deny(err)
	}
	switch {
	case sub == "":
		return allow() // bare `git`, `git --version`, `git --help`
	case reads[sub]:
		return readWithSafeFlags(sub, args)
	}
	if f, ok := splitPersonality[sub]; ok {
		return f(args)
	}
	if why, ok := refusals[sub]; ok {
		return deny(why)
	}
	return deny("this git command is not on the read-only list, so it is refused without being examined further.  " +
		"Version control here goes through the archivist's MCP tools — your environment prompt lists them, and you " +
		"call them as tools, not as shell commands.  If a BUILD needs this git command, say so in your report.")
}

// writeFlags turn an otherwise read-only command into one that writes a
// file or executes a program.  They are refused on every passing
// command rather than per-command, because the list is short and the
// alternative is auditing each read's full flag set.
//
//	--output=<file>          diff/log/show/format-patch: writes the file
//	-O<cmd> / --open-files-in-pager  grep: runs the command
//	--ext-diff               diff: runs the configured external differ
var writeFlags = []string{"--output", "-O", "--open-files-in-pager", "--ext-diff"}

// readWithSafeFlags admits a known read only if nothing in its
// arguments turns it into a write.
func readWithSafeFlags(sub string, args []string) plan {
	for _, a := range args {
		for _, bad := range writeFlags {
			if a == bad || strings.HasPrefix(a, bad+"=") || (bad == "-O" && strings.HasPrefix(a, "-O") && a != "-O") {
				return deny("`" + bad + "` makes `" + sub + "` write a file or run a program, so it is not the read it looks like.  " +
					"Read the output instead, or write the file yourself with the shell.")
			}
		}
	}
	return allow()
}

// execConfig are the -c keys that let a configuration value run a
// program.  Setting one turns `git log` into arbitrary execution
// wearing git's name.  The agent already has a shell, so refusing these
// prevents CONCEALMENT rather than execution — a command that runs
// should look like what it is in the transcript and the log.
var execConfig = []string{
	"core.pager", "core.editor", "core.sshcommand", "core.askpass",
	"core.fsmonitor", "core.hookspath", "core.alternaterefscommand",
	"diff.external", "sequence.editor", "gpg.program", "credential.helper",
	"uploadpack.packobjectshook", "init.templatedir", "ssh.variant",
	"alias.", "pager.", "browser.", "help.browser", "difftool.", "mergetool.",
	"filter.", "protocol.", "remote.", "url.", "http.proxy", "safe.directory",
}

// globalSwitches are the pre-subcommand options that take no value and
// change nothing we care about.
var globalSwitches = map[string]bool{
	"-p": true, "--paginate": true, "-P": true, "--no-pager": true,
	"--no-replace-objects": true, "--literal-pathspecs": true,
	"--glob-pathspecs": true, "--noglob-pathspecs": true,
	"--icase-pathspecs": true, "--no-optional-locks": true,
	"--version": true, "--help": true, "-h": true,
	"--html-path": true, "--man-path": true, "--info-path": true,
	"--exec-path": true, // valueless form prints the path; the =<path> form is refused below
}

// subcommand splits the global options off the front and returns the
// subcommand, its arguments, and a refusal reason if the globals cannot
// be understood.
//
// Every unrecognized leading token is a REFUSAL, never a skip.  This is
// not fussiness: git has several value-taking global options, and
// skipping one as though it were a switch makes its VALUE look like the
// subcommand.  `git --namespace log commit -am x` then classifies as
// `log`, passes as a read, and performs a real commit that no log
// records.  A leading token we do not understand is a leading token
// whose arity we do not know, so the only safe reading is to stop.
func subcommand(argv []string) (sub string, args []string, refusal string) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			return a, argv[i+1:], ""
		}
		switch {
		case a == "-C" || a == "-c":
			// Value in the next token.
			if i+1 >= len(argv) {
				return "", nil, "`" + a + "` needs a value."
			}
			i++
			if a == "-c" {
				if why := checkConfigPair(argv[i]); why != "" {
					return "", nil, why
				}
			}
		case strings.HasPrefix(a, "-C") && len(a) > 2:
			// Attached path, e.g. -C/grange/tree.
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			if why := checkConfigPair(a[2:]); why != "" {
				return "", nil, why
			}
		case globalSwitches[a]:
			// Valueless.
		default:
			return "", nil, "`" + a + "` is not a git option this workspace understands, and options it cannot read " +
				"may take a value — which would make that value look like the command.  Refused rather than guessed at."
		}
	}
	return "", nil, ""
}

// checkConfigPair vets one -c key=value setting.
func checkConfigPair(pair string) string {
	key := strings.ToLower(pair)
	if i := strings.Index(key, "="); i >= 0 {
		key = key[:i]
	}
	for _, bad := range execConfig {
		if key == bad || (strings.HasSuffix(bad, ".") && strings.HasPrefix(key, bad)) {
			return "`-c " + pair + "` sets a configuration value that can run a program, which would make this " +
				"invocation arbitrary execution wearing git's name.  Run the command directly instead — " +
				"the shell is yours, and a command that runs should look like what it is."
		}
	}
	return ""
}

func classifyBranch(args []string) plan {
	for _, a := range args {
		switch {
		case a == "--list" || a == "-l" || a == "-a" || a == "--all" || a == "-r" || a == "--remotes" ||
			a == "-v" || a == "-vv" || a == "--verbose" || a == "--show-current" ||
			a == "--merged" || a == "--no-merged" || a == "--contains" || a == "--no-contains" ||
			a == "--points-at" || a == "-i" || a == "--ignore-case" || a == "--color" || a == "--no-color" ||
			a == "--column" || a == "--no-column" || a == "--sort" || a == "--format":
		case strings.HasPrefix(a, "--sort=") || strings.HasPrefix(a, "--format=") ||
			strings.HasPrefix(a, "--contains=") || strings.HasPrefix(a, "--merged=") ||
			strings.HasPrefix(a, "--points-at="):
		case strings.HasPrefix(a, "-"):
			return deny("`git branch " + a + "` changes branches, which is the archivist's.  " +
				tool(verbs.StartWork) + " begins one, " + bare(verbs.SwitchWork) + " moves to one, " +
				tool(verbs.AbandonWork) + " deletes one.  Listing forms of `git branch` are available.")
		default:
			// A bare name: creation, or a filter value for one of the
			// options above.  Cannot tell them apart safely, so refuse.
			return deny("`git branch " + a + "` would create or operate on a branch, which is the archivist's.  " +
				"Call " + tool(verbs.StartWork) + " with no name and it mints `agent/<codename>` for you.  " +
				"`git branch` with no arguments lists what exists.")
		}
	}
	return allow()
}

// configWriteFlags name a form of `git config` that changes something,
// whatever else is on the line.
var configWriteFlags = map[string]bool{
	"--add": true, "--unset": true, "--unset-all": true, "--replace-all": true,
	"--rename-section": true, "--remove-section": true, "--edit": true,
	"-e": true, "--set-all": true, "--fixed-value": false,
}

// classifyConfig follows git's own grammar rather than a list of
// blessed spellings: reading is `config <key>` or `config --get <key>`,
// and writing is `config <key> <value>`.  The ARITY is what
// distinguishes them.
//
// The bare one-operand read matters more than it looks — `git config
// remote.origin.url` is exactly what Go's -buildvcs stamping runs
// (cmd/go/internal/vcs.gitRemoteRepo), and build-scan and release
// tooling reach for the same form.  An earlier version of this function
// allowed only the explicit `--get` spelling and would have broken
// every Go build in the cell.
func classifyConfig(args []string) plan {
	if len(args) == 0 {
		return deny("`git config` with no arguments opens an editor, and there is none here.  " +
			"To read a value: `git config <key>`.")
	}
	var operands int
	var listing bool
	for _, a := range args {
		switch {
		case configWriteFlags[a]:
			return deny("`git config " + a + "` changes configuration.  The repository's identity and safety " +
				"settings are set at provision, and changing them would alter how work is attributed or published.")
		case a == "-l" || a == "--list" || a == "--get-regexp" || a == "--get-urlmatch":
			listing = true
		case a == "--get" || a == "--get-all":
		case strings.HasPrefix(a, "-"):
			// Scope and formatting switches (--local, --show-origin, -z,
			// --type=…) are harmless on a read and meaningless without
			// one; the write forms are caught above.
		default:
			operands++
		}
	}
	switch {
	case listing:
		return allow() // --list takes none, --get-regexp takes a pattern
	case operands == 1:
		return allow() // `config <key>` or `--get <key>`: a read
	case operands == 0:
		return deny("`git config` needs a key to read, e.g. `git config remote.origin.url`.")
	}
	return deny("`git config <key> <value>` writes configuration.  The repository's identity and endpoint are set " +
		"at provision and are not yours to change; " + tool(verbs.CurrentState) + " reports the branch and endpoint.")
}

func classifyRemote(args []string) plan {
	if len(args) == 0 {
		return allow() // lists remote names
	}
	switch args[0] {
	case "-v", "--verbose", "show", "get-url":
		return allow()
	}
	return deny("the remote is set at provision from the endpoint table; changing it would point published work " +
		"somewhere unreviewed.  `git remote -v` and `git remote get-url` are available for reading.")
}

func classifyStash(args []string) plan {
	if len(args) > 0 && (args[0] == "list" || args[0] == "show") {
		return allow()
	}
	return deny("use " + tool(verbs.SetAside) + " to park the working tree and " + bare(verbs.Resume) + " to bring it back.  " +
		"`git stash list` and `git stash show` are available for reading.")
}

func classifyReflog(args []string) plan {
	if len(args) == 0 || args[0] == "show" {
		return allow()
	}
	return deny("only `git reflog show` is available here; expiring or deleting reflog entries is repository maintenance.")
}

func classifySymbolicRef(args []string) plan {
	// Reading takes one ref name; writing takes two, or --delete.
	var operands int
	for _, a := range args {
		switch {
		case a == "-q" || a == "--quiet" || a == "--short":
		case strings.HasPrefix(a, "-"):
			return deny("`git symbolic-ref " + a + "` moves a ref, which is the archivist's.")
		default:
			operands++
		}
	}
	if operands > 1 {
		return deny("`git symbolic-ref <name> <ref>` moves a ref, which is the archivist's.  " +
			"Reading one (`git symbolic-ref --short HEAD`) is available.")
	}
	return allow()
}

func classifyNotes(args []string) plan {
	if len(args) == 0 || args[0] == "list" || args[0] == "show" {
		return allow()
	}
	return deny("notes are not part of the archivist's model; they would not survive to the pull request.")
}

func classifyWorktree(args []string) plan {
	if len(args) > 0 && args[0] == "list" {
		return allow()
	}
	return deny("one workspace per session, by design — see \"Your workspace: the grange\" in your environment prompt.")
}

func classifySubmodule(args []string) plan {
	if len(args) > 0 && (args[0] == "status" || args[0] == "summary") {
		return allow()
	}
	return deny("submodules are not provisioned into the grange and cannot be fetched from here.")
}

func classifyBundle(args []string) plan {
	if len(args) > 0 && (args[0] == "verify" || args[0] == "list-heads") {
		return allow()
	}
	return deny("`git bundle create` writes a file and is a way to move history out of the cell; " +
		"work leaves through " + tool(verbs.Publish) + " and the pull request.")
}
