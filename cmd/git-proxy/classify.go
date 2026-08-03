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
)

// Classification (docs/git-proxy.md).  The useful cut is not read/write
// but whether a command moves REFS or HEAD: commands that touch only
// the working tree are already safe, because `checkpoint` records the
// tree wholesale and git's index is an implementation detail the
// archivist ignores.  What matters is the state the archivist's verbs
// are meant to author.

type verdict int

const (
	pass      verdict = iota // run the real git unchanged
	translate                // call an archivist verb instead
	refuse                   // never run; say why, and what to use
)

// plan is what classify decides about one invocation.
type plan struct {
	verdict verdict
	verb    string         // archivist verb, when translate
	args    map[string]any // its arguments
	reason  string         // when refuse: the explanation, naming the alternative
}

// reads run unchanged.  Deliberately a closed allow-list rather than
// "anything not known to mutate": an unrecognized command is a refusal
// (see classify), which is a loud error we see immediately instead of a
// silent bypass.  Toolchains live here too — Go's -buildvcs stamping
// runs status/rev-parse/show, Gradle version plugins run describe.
var reads = map[string]bool{
	"annotate": true, "blame": true, "cat-file": true, "check-attr": true,
	"check-ignore": true, "describe": true, "diff": true, "diff-files": true,
	"diff-index": true, "diff-tree": true, "for-each-ref": true, "grep": true,
	"help": true, "log": true, "ls-files": true, "ls-tree": true,
	"merge-base": true, "name-rev": true, "rev-list": true, "rev-parse": true,
	"shortlog": true, "show": true, "show-ref": true, "status": true,
	"var": true, "verify-commit": true, "version": true, "whatchanged": true,
}

// worktreeOnly commands change files without moving refs or HEAD.  They
// pass because the next checkpoint records the tree as it stands, so
// there is nothing for them to desynchronize.
var worktreeOnly = map[string]bool{"mv": true, "clean": true}

// refusals are the commands with no archivist counterpart, mapped to
// the reason.  Being explicit here (rather than falling through to the
// generic unknown-command message) is what makes the refusal useful:
// it names why the operation is absent, which is usually a design
// decision rather than an oversight.
var refusals = map[string]string{
	"add": "there is no staging area here — `checkpoint` records the working tree as it stands, " +
		"so there is nothing to stage.  Just edit and checkpoint.",
	"am":        "patch application is not part of the archivist's model; edit the files and checkpoint.",
	"apply":     "patch application is not part of the archivist's model; edit the files and checkpoint.",
	"bisect":    "bisect drives HEAD through a search; the archivist owns HEAD.  Use `history` and `show_change` to reason about changes instead.",
	"clone":     "this workspace is provisioned for you and cannot be changed; there is no route to a forge from here either.",
	"config":    "the repository's identity and safety settings are set at provision and are not yours to change.",
	"fetch":     "remote access is the archivist's alone; `sync_from_upstream` brings the default branch forward.",
	"gc":        "repository maintenance is not the agent's concern; the workspace is destroyed after the task.",
	"init":      "this workspace is already a provisioned clone, and a second repository inside it would not be published.",
	"rebase":    "the archivist's history is append-only: checkpoints accumulate and `restore` rolls back.  There is no rewrite verb, deliberately — published work must not change under a reviewer.",
	"remote":    "the remote is set at provision from the endpoint table; changing it would point published work somewhere unreviewed.",
	"reset":     "use `restore` to roll back — it knows whether the branch is published, and rewrites history only while it is still private.",
	"revert":    "no revert verb: `restore` to an earlier checkpoint, then checkpoint the result forward.",
	"submodule": "submodules are not provisioned into the grange and cannot be fetched from here.",
	"tag":       "tags are a release act, and releases are the human's, not the agent's.",
	"worktree":  "one workspace per session, by design — see the grange rules in your environment prompt.",
}

// gitQuery lets classification ask the real git a read-only question.
// `git checkout X` is a branch switch or a path restore depending on
// what X is, and only the repository knows which.
type gitQuery interface {
	branchExists(name string) bool
}

// classify decides what to do with one git invocation.  argv excludes
// the program name.
func classify(argv []string, q gitQuery) plan {
	sub, args := subcommand(argv)
	switch {
	case sub == "":
		return plan{verdict: pass} // bare `git` prints usage
	case reads[sub], worktreeOnly[sub]:
		return plan{verdict: pass}
	case sub == "rm":
		return classifyRm(args)
	case sub == "reflog":
		// Reading the reflog is fine; `reflog expire` rewrites it.
		if len(args) == 0 || args[0] == "show" {
			return plan{verdict: pass}
		}
		return plan{verdict: refuse, reason: "only `git reflog show` is available here; expiring the reflog is repository maintenance."}
	case sub == "commit":
		return classifyCommit(args)
	case sub == "push":
		return classifyPush(args)
	case sub == "checkout", sub == "switch":
		return classifyCheckout(sub, args, q)
	case sub == "branch":
		return classifyBranch(args)
	case sub == "stash":
		return classifyStash(args)
	case sub == "restore":
		return classifyRestore(args)
	case sub == "pull", sub == "merge":
		return classifyPull(sub, args)
	}
	if why, ok := refusals[sub]; ok {
		return plan{verdict: refuse, reason: why}
	}
	return plan{verdict: refuse, reason: "not available in this workspace.  " +
		"Version control here goes through the archivist's verbs; if a build genuinely needs this command, tell the operator."}
}

// subcommand splits the global options off the front.  Only the global
// flags that appear in practice are understood, and an unrecognized one
// refuses downstream rather than being skipped — skipping is how a flag
// with a value swallows the subcommand.
func subcommand(argv []string) (string, []string) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			return a, argv[i+1:]
		}
		// -C <path> and -c <cfg> take a value; the rest are switches.
		if a == "-C" || a == "-c" {
			i++
		}
	}
	return "", nil
}

func classifyRm(args []string) plan {
	for _, a := range args {
		if a == "--cached" {
			return plan{verdict: refuse, reason: "`--cached` un-stages without touching the file, and there is no staging area here.  " +
				"Delete the file and `checkpoint`."}
		}
	}
	return plan{verdict: pass} // a plain delete; the next checkpoint records it
}

// classifyCommit maps `git commit` onto checkpoint.  Strict: an
// unrecognized flag refuses rather than being dropped, because a
// dropped flag turns the agent's request into a different request.
func classifyCommit(args []string) plan {
	var message string
	var paths []string
	seenSep := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case seenSep:
			paths = append(paths, a)
		case a == "--":
			seenSep = true
		case a == "-m" || a == "--message":
			if i+1 >= len(args) {
				return plan{verdict: refuse, reason: "`-m` needs a message."}
			}
			i++
			message = args[i]
		case strings.HasPrefix(a, "--message="):
			message = strings.TrimPrefix(a, "--message=")
		case strings.HasPrefix(a, "-m") && len(a) > 2:
			message = a[2:]
		case a == "-a" || a == "--all" || a == "-q" || a == "--quiet":
			// Harmless: checkpoint records the whole tree anyway.
		case a == "--amend":
			return plan{verdict: refuse, reason: "history here is append-only — a published checkpoint must not change under a reviewer.  " +
				"Make the correction and `checkpoint` it forward."}
		case !strings.HasPrefix(a, "-"):
			return plan{verdict: refuse, reason: "commit takes paths after `--`, e.g. `git commit -m msg -- file`.  " +
				"Better still, omit them: a checkpoint of the whole tree cannot record half a rename."}
		default:
			return plan{verdict: refuse, reason: fmt.Sprintf("`%s` has no counterpart in `checkpoint`, so honouring it is not possible.  "+
				"Call the archivist's `checkpoint` directly if you need something this proxy cannot express.", a)}
		}
	}
	if message == "" {
		return plan{verdict: refuse, reason: "a checkpoint needs a message: `git commit -m \"what this records\"`.  " +
			"There is no editor to open here."}
	}
	cargs := map[string]any{"message": message}
	if len(paths) > 0 {
		cargs["paths"] = paths
	}
	return plan{verdict: translate, verb: "checkpoint", args: cargs}
}

// classifyPush maps `git push` onto publish, which takes no arguments:
// it pushes the branch the archivist is already on.  Anything that
// names a different target, or asks for force or deletion, refuses —
// those are exactly the shapes the verb set deliberately cannot express.
func classifyPush(args []string) plan {
	for _, a := range args {
		switch {
		case a == "-u" || a == "--set-upstream" || a == "origin" || a == "-q" || a == "--quiet":
			// publish sets the upstream and origin is the only remote.
		case strings.HasPrefix(a, "-"):
			return plan{verdict: refuse, reason: fmt.Sprintf("`%s` is not something publish can express.  "+
				"Force-push and tag deletion are structurally absent from the archivist's verbs — published work must not change under a reviewer.", a)}
		case strings.Contains(a, ":"):
			return plan{verdict: refuse, reason: "refspecs are not available; `publish` pushes the line of work you are on."}
		default:
			// A branch name: publish only pushes the current branch, and
			// the archivist checks that itself.
		}
	}
	return plan{verdict: translate, verb: "publish", args: map[string]any{}}
}

func classifyCheckout(sub string, args []string, q gitQuery) plan {
	var create bool
	var target string
	var paths []string
	seenSep := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case seenSep:
			paths = append(paths, a)
		case a == "--":
			seenSep = true
		case a == "-b" || a == "-c" || a == "-B" || a == "-C":
			create = true
		case a == "-q" || a == "--quiet":
		case strings.HasPrefix(a, "-"):
			return plan{verdict: refuse, reason: fmt.Sprintf("`%s %s` has no counterpart; use `start_work`, `switch_work`, or `restore`.", sub, a)}
		case target == "":
			target = a
		default:
			paths = append(paths, a)
		}
	}
	switch {
	case len(paths) > 0 && target == "":
		return plan{verdict: translate, verb: "restore", args: map[string]any{"path": paths[0]}}
	case create && target != "":
		return plan{verdict: translate, verb: "start_work", args: map[string]any{"name": target}}
	case target == "":
		return plan{verdict: refuse, reason: fmt.Sprintf("`%s` needs a line of work to switch to; `switch_work` names one.", sub)}
	case q.branchExists(target):
		return plan{verdict: translate, verb: "switch_work", args: map[string]any{"name": target}}
	}
	return plan{verdict: refuse, reason: fmt.Sprintf("%q is not a branch here.  To start a line of work use `start_work` "+
		"(with no name, and it mints one); to discard edits to a file use `restore`.", target)}
}

func classifyBranch(args []string) plan {
	var del bool
	var name string
	for _, a := range args {
		switch {
		case a == "-d" || a == "-D" || a == "--delete":
			del = true
		case a == "-a" || a == "--all" || a == "-l" || a == "--list" || a == "-r" || a == "--remotes" || a == "-v" || a == "--verbose":
			// listing forms
		case strings.HasPrefix(a, "-"):
			return plan{verdict: refuse, reason: fmt.Sprintf("`branch %s` has no counterpart; `start_work`, `switch_work`, and `abandon_work` are the branch verbs.", a)}
		default:
			name = a
		}
	}
	switch {
	case del && name != "":
		return plan{verdict: translate, verb: "abandon_work", args: map[string]any{"name": name}}
	case del:
		return plan{verdict: refuse, reason: "name the line of work to abandon."}
	case name != "":
		return plan{verdict: refuse, reason: "creating a branch this way skips the naming rule; `start_work` with no name mints `agent/<codename>`."}
	}
	return plan{verdict: pass} // a listing
}

func classifyStash(args []string) plan {
	if len(args) == 0 || args[0] == "push" || args[0] == "save" {
		return plan{verdict: translate, verb: "set_aside", args: map[string]any{}}
	}
	switch args[0] {
	case "pop", "apply":
		return plan{verdict: translate, verb: "resume", args: map[string]any{}}
	case "list", "show":
		return plan{verdict: pass}
	}
	return plan{verdict: refuse, reason: "`set_aside` parks the tree and `resume` brings it back; the other stash forms have no counterpart."}
}

func classifyRestore(args []string) plan {
	var paths []string
	for _, a := range args {
		if a == "--" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			return plan{verdict: refuse, reason: fmt.Sprintf("`restore %s` has no counterpart; the archivist's `restore` takes a path or a checkpoint.", a)}
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		return plan{verdict: refuse, reason: "name a path to restore."}
	}
	return plan{verdict: translate, verb: "restore", args: map[string]any{"path": paths[0]}}
}

func classifyPull(sub string, args []string) plan {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-q" && a != "--quiet" && a != "--rebase" && a != "--ff-only" {
			return plan{verdict: refuse, reason: fmt.Sprintf("`%s %s` has no counterpart; `sync_from_upstream` brings the default branch forward.", sub, a)}
		}
	}
	return plan{verdict: translate, verb: "sync_from_upstream", args: map[string]any{}}
}
