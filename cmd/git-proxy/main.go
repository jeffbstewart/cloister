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

// Command git-proxy is installed as `git` inside the agent cell
// (docs/git-proxy.md).  It answers one question about every
// invocation — is this a git command we positively know to be
// read-only? — and either runs the real git unchanged or refuses with a
// reason naming the archivist MCP tool to call instead.
//
// It does NOT translate.  An earlier design mapped a "closed core" of
// mutating commands onto archivist tool calls, and review found the
// same defect in every corner of it: git's argument grammar is rich
// enough that a translator either reimplements it faithfully or
// silently performs a different operation than the one asked for.
// `git checkout main -- file.go` moved HEAD instead of restoring a
// file; `git commit -m title -m body` dropped the subject line;
// `git merge other` synced the default branch instead.  Every one
// looked like success.  Convenience was being paid for in exactly the
// currency this system refuses to spend — an agent confidently wrong
// about what just happened.
//
// So the proxy has no opinion it cannot defend.  Reads run; everything
// else is a refusal that says what to call instead, and the agent calls
// it itself, getting the tool's real answer rather than a rendering of
// it.
//
// This is NOT a security boundary.  The real binary is moved to a path
// this program names, but /usr/lib/git-core/git is a hardlink to the
// same inode and must stay for GIT_EXEC_PATH, so a determined process
// finds it.  What it buys is that the wrong move is no longer the
// reflexive one.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// version is set at build time (-ldflags -X main.version=…).
var version = "dev"

// identifyFlag makes the proxy positively identifiable.  The image
// build asserts that `git` answers it, which is the only way to tell
// the proxy from the real binary at the install site — `git version`
// passes straight through and so proves nothing.  A later layer that
// restored the real git to /usr/bin/git would otherwise satisfy every
// check the Dockerfile could make.
const identifyFlag = "--cloister-proxy-version"

// realGit is where the actual binary lives.  Unadvertised on purpose —
// see the package note on what that does and does not buy.  Not
// configurable by environment: an override would let anything in this
// container point the shim at a binary of its choosing, and nothing
// legitimate needs it.  Keep in sync with docker/workbench/Dockerfile.
//
// It must be an ABSOLUTE path to a binary that is not this one.  Both
// halves are load-bearing, and enforced in runGit: a bare name would be
// resolved through PATH, where this program is installed as `git`, and
// the proxy would exec itself forever.  That is not hypothetical — it
// forks until the machine dies, and it took eleven thousand processes
// to notice.
var realGit = "/usr/lib/cloister/libexec/gx"

// depthEnv counts how deep we are in nested git invocations.  The path
// check in runGit stops the direct loop; this stops the indirect ones,
// which the proxy cannot see: git's own script subcommands shell out to
// `git` by name, a pager or alias can, and a repository hook certainly
// can.  Any of those resolves through PATH and lands back here.
//
// Legitimate nesting is shallow — git-submodule calling rev-parse is
// depth 2 — so a generous cap separates it cleanly from a loop, which
// reaches the cap in milliseconds.
const depthEnv = "CLOISTER_GIT_PROXY_DEPTH"

// maxDepth is the cap.  Deliberately well above real nesting: the point
// is to catch runaway recursion, not to police how git organizes its
// own subcommands.
const maxDepth = 8

// passthroughMarker disables the proxy entirely — the operator's escape
// hatch for the first build that trips on a command we did not
// anticipate.
//
// A FILE rather than an environment variable, and deliberately one the
// agent cannot create: the cell runs read_only with cap_drop ALL as uid
// 1000 (docker/cell.yaml), so /etc is not writable from inside, while
// the operator sets it with a bind mount or `docker exec -u 0`.  An
// env var would have been settable by the agent itself, documented in
// the very repository it is often granted, and inherited by every
// subprocess — one export in a build script and git supervision is off
// for the session.
const passthroughMarker = "/etc/cloister/git-passthrough"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) == 1 && argv[0] == identifyFlag {
		fmt.Println(version)
		return 0
	}
	// Before anything else, including the escape hatch: a runaway must
	// stop even when the proxy has been told to stand aside, because
	// passthrough is the mode that execs most eagerly.
	if depth() >= maxDepth {
		fmt.Fprintf(os.Stderr, "cloister: git invoked itself %d deep and was stopped.\n"+
			"Something resolves `git` back to this proxy — a hook, an alias, a pager, or a\n"+
			"misconfigured install.  Nothing ran.\n", maxDepth)
		return 1
	}
	if _, err := os.Stat(passthroughMarker); err == nil {
		// Logged and announced.  This is the one path where the proxy
		// stands aside, so it is the last place that should be silent.
		logEvent("passthrough", argv)
		fmt.Fprintf(os.Stderr, "[cloister] git supervision is disabled (%s); running git unchanged.\n", passthroughMarker)
		return runGit(argv)
	}

	switch p := classify(argv); p.verdict {
	case pass:
		// Not logged.  Reads are the high-volume, zero-signal case —
		// every `go build` stamps VCS info — and recording them would
		// bury the refusals, which are the whole point of the log.
		return runGit(argv)

	case refuse:
		logEvent("refused", argv)
		// The order here is deliberate.  A live session read a refusal,
		// saw a tool name, and typed it at the shell — then the next one,
		// and the next, until the context filled with "command not found"
		// and the model came apart.  The names are quoted rather than
		// parenthesized (see tool()), and the sentence saying they are
		// NOT programs sits immediately under the reason that names them,
		// not at the bottom where it was being read too late.
		fmt.Fprintf(os.Stderr, "cloister: %s is refused in this workspace.\n\n%s\n\n"+
			"The quoted names above are MCP tools on the archivist.  They are NOT programs:\n"+
			"there is nothing to type at a shell, no such command exists, and trying costs\n"+
			"you a turn.  Invoke them the way you invoke any MCP tool.  Only read-only git\n"+
			"commands run here; your environment prompt lists what replaces the rest.\n",
			display(argv), wrap(p.reason))
		return 1

	default:
		// Unreachable today.  Said out loud anyway: a program installed
		// as /usr/bin/git that exits non-zero in silence is the worst
		// failure available to it.
		fmt.Fprintf(os.Stderr, "cloister: internal error: unhandled verdict %v for %s\n", p.verdict, display(argv))
		return 1
	}
}

// runGit runs the real git, passing the terminal through so pagers and
// colour behave, and propagates its exit code.  There is no fallback to
// it from any refusal path — a refused command must not run by another
// route.
func runGit(argv []string) int {
	if err := checkRealGit(); err != nil {
		fmt.Fprintf(os.Stderr, "cloister: refusing to run git: %v\n", err)
		return 1
	}
	cmd := exec.Command(realGit, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", depthEnv, depth()+1))
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		// Deliberately without the path: it is obscurity only, but there
		// is no reason to volunteer it in agent-visible output.
		fmt.Fprintf(os.Stderr, "cloister: git could not be run: %v\n", err)
		return 1
	}
	return 0
}

// depth reads the nesting counter.  An unreadable value counts as the
// cap: a garbled counter means something is manipulating the
// environment, and stopping is the safe reading.
func depth() int {
	v := os.Getenv(depthEnv)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return maxDepth
	}
	return n
}

// checkRealGit refuses to exec anything that could be this program.
//
// The absolute-path requirement is what keeps PATH out of it: this
// binary is installed AS `git`, so resolving a bare name would find
// itself.  os.SameFile then catches the case a path comparison would
// miss — a hardlink or a copy under another name is still the same
// inode, and still a loop.
func checkRealGit() error {
	if !filepath.IsAbs(realGit) {
		return fmt.Errorf("the configured git (%q) is not an absolute path, and resolving it "+
			"through PATH would find this proxy", realGit)
	}
	target, err := os.Stat(realGit)
	if err != nil {
		return fmt.Errorf("the real git is missing from its expected location: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil // cannot compare; the absolute-path check still holds
	}
	mine, err := os.Stat(self)
	if err != nil {
		return nil
	}
	if os.SameFile(target, mine) {
		return fmt.Errorf("the configured git is this proxy itself (%q) — running it would recurse "+
			"until the machine dies", realGit)
	}
	return nil
}

// display renders an argv for a human, quoting any element that would
// otherwise read as several.  Without it `git commit -m "fix the
// parser"` echoes back as four bare words and the reader misjudges what
// was rejected.
func display(argv []string) string {
	parts := make([]string, 0, len(argv)+1)
	parts = append(parts, "`git")
	for _, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'") {
			parts = append(parts, fmt.Sprintf("%q", a))
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ") + "`"
}

// logEvent records refusals and passthrough runs.  Never a pass: that
// is the high-volume, zero-signal case, and burying the refusals under
// it would cost the log its whole value.  A refusal says the agent
// wanted something not on offer — a candidate for a new archivist tool,
// a new read to allow, or a line in the environment prompt.  Together
// they are a curriculum for what to build next.
//
// Best-effort: a workspace that cannot write a log is still a
// workspace.
func logEvent(kind string, argv []string) {
	path := defaultLogPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	rotate(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }() // best-effort, as above
	// %q on the arguments, so a newline or tab inside one cannot forge a
	// record.  A commit message is free text the model authors, and this
	// log is meant to be read by a parser eventually.  Epoch seconds,
	// the house on-disk time format.
	fmt.Fprintf(f, "%d\t%s\t%q\n", time.Now().Unix(), kind, argv)
}

// maxLogBytes bounds the log.  An agent that loops on a refusal — which
// the environment prompt warns against, so it happens — would otherwise
// append without limit to the per-project volume, and a full volume
// breaks the session for a diagnostic nobody asked for.
const maxLogBytes = 4 << 20

// rotate keeps one previous generation.  Two files, no config, no
// scheduler: enough that the recent past survives a noisy loop, and
// bounded enough that nothing has to watch it.
func rotate(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < maxLogBytes {
		return
	}
	_ = os.Rename(path, path+".1") // best-effort, like everything here
}

// defaultLogPath puts the log on the agent's HOME — the per-project
// volume, which outlives the grange.  Not /grange: that is destroyed at
// dispose, and the log's whole value is being read afterwards.
func defaultLogPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".cloister", "git-proxy.log")
}

// wrap breaks a reason into 72-column lines, because a refusal nobody
// reads teaches nothing.
func wrap(s string) string {
	const width = 72
	var b strings.Builder
	col := 0
	for _, word := range strings.Fields(s) {
		if col > 0 && col+1+len(word) > width {
			b.WriteString("\n")
			col = 0
		} else if col > 0 {
			b.WriteString(" ")
			col++
		}
		b.WriteString(word)
		col += len(word)
	}
	return b.String()
}
