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
// (docs/git-proxy.md).  Reads run the real git unchanged; the closed
// core of commands that move refs or HEAD is translated into the
// archivist's verbs; everything else is refused with a reason.
//
// It announces every translation on stderr.  That is the load-bearing
// decision, and it is the same principle that made provision and
// dispose ABSENT from the agent's MCP surface rather than hidden: the
// model must not hold beliefs that quietly diverge from reality.  An
// invisible translation would buy convenience by making the agent wrong
// about what just happened — believing it had rewritten history, or
// staged a subset, when it had not — and the transcript would look
// correct throughout.  A visible one buys the same convenience and
// teaches the verb.
//
// This is NOT a security boundary.  The real binary is moved somewhere
// only this program names, which is obscurity: a determined process in
// this container finds it.  What it buys is that the wrong move is no
// longer the reflexive one.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/mcpclient"
)

// version is set at build time (-ldflags -X main.version=…).
var version = "dev"

// realGit is where the actual binary lives.  Unadvertised on purpose —
// see the package note on what that does and does not buy.
const realGit = "/usr/lib/cloister/libexec/gx"

// callTimeout bounds a translated verb.  publish pushes through the
// relays, which is the slow one; the rest are local.
const callTimeout = 10 * time.Minute

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	real := envOr("CLOISTER_GIT_REAL", realGit)

	// The operator's escape hatch: the first build to trip on an
	// unrecognized command must not cost a session to unblock.
	if os.Getenv("CLOISTER_GIT_PASSTHROUGH") != "" {
		return exec_(real, argv)
	}

	p := classify(argv, &realGitQuery{path: real})
	switch p.verdict {
	case pass:
		// Not logged.  Reads are the high-volume, zero-signal case —
		// every `go build` stamps VCS info — and recording them would
		// bury the two events that carry meaning.
		return exec_(real, argv)

	case refuse:
		logEvent("refused", argv, "")
		fmt.Fprintf(os.Stderr, "cloister: `git %s` is not available here.\n\n%s\n\n"+
			"Your environment prompt lists the archivist's verbs and what each replaces.\n",
			strings.Join(argv, " "), wrap(p.reason))
		return 1

	case translate:
		logEvent("translated", argv, p.verb)
		fmt.Fprintf(os.Stderr, "[cloister] git %s  →  archivist %s\n", strings.Join(argv, " "), p.verb)
		return callArchivist(p)
	}
	return 1
}

// callArchivist runs the translated verb and prints its answer.  A
// refusal is the archivist's own words, verbatim: the reason is the part
// the reader needs, and paraphrasing it is how "op/repo fails R2: stale
// approvals survive" becomes "publish failed".
func callArchivist(p plan) int {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	c, err := mcpclient.Dial(ctx, mcpclient.Config{
		URL:     envOr("ARCHIVIST_MCP_URL", "http://archivist:9600/mcp"),
		Name:    "git-proxy",
		Version: version,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloister: cannot reach the archivist: %v\n", err)
		return 1
	}
	defer c.Close()

	var answer map[string]any
	if err := c.Call(ctx, p.verb, p.args, &answer); err != nil {
		var ref *mcpclient.RefusedError
		if errors.As(err, &ref) {
			fmt.Fprintf(os.Stderr, "cloister: the archivist refused %s:\n%s\n", p.verb, wrap(ref.Message))
			return 1
		}
		fmt.Fprintf(os.Stderr, "cloister: %v\n", err)
		return 1
	}
	// Print the answer as the archivist gave it.  Anything it chose to
	// report — a checkpoint's leftovers, a publish that advanced
	// nothing — is exactly what the caller must not be shielded from.
	for _, line := range render(p.verb, answer) {
		fmt.Println(line)
	}
	return 0
}

// render turns a verb's answer into lines a human (or a model reading a
// terminal) can act on.  Deliberately plain: this is not git's output
// format and should not pretend to be.
func render(verb string, answer map[string]any) []string {
	var out []string
	switch verb {
	case "checkpoint":
		out = append(out, fmt.Sprintf("checkpoint %v", answer["checkpoint"]))
	case "publish":
		if adv, _ := answer["advanced"].(bool); !adv {
			out = append(out, fmt.Sprintf("published %v — but NOTHING NEW was pushed", answer["branch"]))
		} else {
			out = append(out, fmt.Sprintf("published %v to %v", answer["branch"], answer["endpoint"]))
		}
	default:
		if len(answer) == 0 {
			out = append(out, verb+": done")
		}
	}
	// Whatever else the verb reported — notes, leftovers, branch names —
	// follows verbatim, so a field added server-side is never invisible
	// here.
	for _, k := range []string{"branch", "note", "uncommitted", "restored", "rewound"} {
		if v, ok := answer[k]; ok && !strings.Contains(strings.Join(out, "\n"), fmt.Sprint(v)) {
			out = append(out, fmt.Sprintf("%s: %v", k, v))
		}
	}
	if len(out) == 0 {
		out = append(out, verb+": done")
	}
	return out
}

// exec_ runs the real git, passing the terminal through so pagers and
// colour behave, and propagates its exit code.
func exec_(real string, argv []string) int {
	cmd := exec.Command(real, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "cloister: cannot run git (%s): %v\n", real, err)
		return 1
	}
	return 0
}

// realGitQuery answers classification's questions using the real
// binary — `git checkout X` is a branch switch or a path restore
// depending on what X is, and only the repository knows.
type realGitQuery struct{ path string }

func (q *realGitQuery) branchExists(name string) bool {
	if name == "" {
		return false
	}
	cmd := exec.Command(q.path, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	return cmd.Run() == nil
}

// logEvent records translations and refusals.  Never passthrough: that
// is the high-volume, zero-signal case, and burying these two under it
// would cost the log its whole value.  A translation says the agent
// reached for git where a verb exists (a prompt-tuning signal); a
// refusal says it wanted something not on offer (a candidate for a new
// verb or a new translation).
//
// Best-effort: a workspace that cannot write a log is still a workspace.
func logEvent(kind string, argv []string, verb string) {
	path := envOr("CLOISTER_GIT_LOG", defaultLogPath())
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	// Epoch seconds, the house on-disk time format.
	fmt.Fprintf(f, "%d\t%s\t%s\t%s\n", time.Now().Unix(), kind, verb, strings.Join(argv, " "))
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

// wrap breaks a reason into terminal-width lines, because a refusal
// nobody reads teaches nothing.
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

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
