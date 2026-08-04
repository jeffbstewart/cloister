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

// Command workbench is the operator's door into a cloister cell: the
// deterministic session manager that owns the workspace's lifetime.
//
// It replaces the shell script of the same name, and it exists because
// `provision` and `dispose` moved off the agent's MCP surface
// (docs/archivist.md, "Two surfaces").  A workspace's lifetime is a
// SESSION's lifetime — provision, run the agent to completion, dispose
// — and that loop belongs to a program that cannot change its mind
// halfway, driven by the human, not to the model working inside it.
//
// It is deliberately a line-oriented menu rather than a full-screen
// TUI: it must work over `docker exec -it` and a Portainer console, and
// the repo's dependency policy is stdlib plus the MCP SDK — a curses
// library is neither available nor warranted for four prompts.
//
// The agent itself runs under tmux, so its life is decoupled from the
// attachment: a dropped console no longer kills a task mid-flight.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/operator"
)

// version is set at build time (-ldflags -X main.version=…).
var version = "dev"

type options struct {
	archivist string
	session   string
	repos     string
	agentCmd  string
	grange    string
	// stockPrompt is the environment prompt as baked into the image —
	// the file the agent wrapper hands to --append-system-prompt.  The
	// session manager verifies it before every session; see prompt.go
	// for what goes wrong when it is missing, and for why it must not
	// live in the agent's memory file.
	stockPrompt string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "workbench: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("workbench", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.archivist, "archivist", env("ARCHIVIST_OPERATOR_URL", "http://archivist:9600/operator/mcp"),
		"the archivist's OPERATOR MCP surface (not the agent's /mcp)")
	fs.StringVar(&o.session, "session", env("WORKBENCH_SESSION", "agent"), "tmux session name")
	fs.StringVar(&o.repos, "repos", env("WORKBENCH_REPOS", defaultReposPath()),
		"the recent-repositories list; per user, shared across projects")
	fs.StringVar(&o.agentCmd, "agent", env("WORKBENCH_AGENT", "qwen-cloister"), "the agent CLI to run in the session")
	fs.StringVar(&o.grange, "grange", env("GRANGE_ROOT", "/grange"), "the grange root; tree/ is the checkout")
	fs.StringVar(&o.stockPrompt, "prompt", env("CLOISTER_PROMPT", stockPromptPath),
		"the environment prompt the agent wrapper delivers as a system prompt")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err // flag has already printed the message and usage
	}

	// An already-running session is the common case, and it needs no
	// archivist at all: attach and get out of the way.  Doing this FIRST
	// means a sick archivist never stands between the operator and a
	// task already in flight.
	if hasSession(o.session) {
		fmt.Printf("attaching to the live %s session — detach with Ctrl-Z d\n", o.session)
		return attach(o.session)
	}

	ctx := context.Background()
	c, err := operator.Dial(ctx, operator.Config{URL: o.archivist, Name: "workbench", Version: version})
	if err != nil {
		return fmt.Errorf("%w\n  the archivist is the workspace's owner; without it there is no session to start", err)
	}
	defer func() { _ = c.Close() }()

	m := &manager{
		arc:   c,
		o:     o,
		in:    bufio.NewReader(os.Stdin),
		out:   os.Stdout,
		now:   time.Now,
		start: startSession,
	}
	return m.loop(ctx)
}

// lifecycle is the archivist's operator surface as this program uses
// it.  Narrow on purpose: the session manager's whole vocabulary is
// three verbs, and stating that lets the menus be tested against a fake
// workspace instead of a live clone.
type lifecycle interface {
	Status(ctx context.Context) (operator.Status, error)
	Provision(ctx context.Context, repo, branch string) (operator.ProvisionResult, error)
	Dispose(ctx context.Context, force bool) (operator.DisposeResult, error)
}

// manager holds the session manager's collaborators.  start is a field
// so tests can drive the menus without a tmux to run.
type manager struct {
	arc   lifecycle
	o     options
	in    *bufio.Reader
	out   io.Writer
	now   func() time.Time
	start func(o options, cmd string) (bool, error)
}

func (m *manager) printf(format string, a ...any) { fmt.Fprintf(m.out, format, a...) }
func (m *manager) println(a ...any)               { fmt.Fprintln(m.out, a...) }

// loop is the session manager proper: read the workspace's state, offer
// only the moves that state allows, and come back after the agent exits.
func (m *manager) loop(ctx context.Context) error {
	o := m.o
	for {
		st, err := m.arc.Status(ctx)
		if err != nil {
			return err
		}
		switch st.State {
		case operator.StateCorrupt:
			// Reported, never acted on.  Dispose refuses a markerless
			// tree at any force — that rail is what keeps it structurally
			// unable to wipe a host tree — so there is genuinely nothing
			// to offer here but the truth and where to take it.
			m.printf(`
The workspace is CORRUPT: %s/tree holds something that is neither empty
nor a clean provision — a checkout whose provenance marker is missing,
or a promote that died before its last write.

Nothing in this container can fix it, and nothing should try: dispose
refuses a markerless tree at any force, on purpose.  Recovery is
host-side — inspect the grange volume, save anything worth saving, and
recreate it.
`, o.grange)
			return errors.New("corrupt workspace")

		case operator.StateProvisioned:
			done, err := m.provisionedMenu(ctx, st)
			if err != nil || done {
				return err
			}

		case operator.StateEmpty:
			done, err := m.emptyMenu(ctx)
			if err != nil || done {
				return err
			}

		default:
			return fmt.Errorf("the archivist reports an unknown workspace state %q", st.State)
		}
	}
}

// provisionedMenu is the menu for a live workspace.  Returns done=true
// when the operator is finished with this invocation.
func (m *manager) provisionedMenu(ctx context.Context, st operator.Status) (bool, error) {
	m.printf("\nworkspace: %s   (provisioned %s)\n", st.Provisioned(), ago(st.ProvisionedAt, m.now()))
	m.printf("  1) start the agent (%s)\n", m.o.agentCmd)
	m.println("  2) shell")
	m.println("  3) finish — dispose the workspace")
	m.println("  q) quit, leaving the workspace as it is")

	switch choice, err := m.prompt("> "); {
	case err != nil:
		return true, err
	case choice == "q":
		return true, nil
	case choice == "" || choice == "1":
		// Immediately before the agent starts — and it does not start if
		// its rules cannot be delivered.  An agent without them looks
		// entirely normal right up until it does something expensive.
		if !m.reportPrompt() {
			return false, nil
		}
		return m.start(m.o, m.o.agentCmd)
	case choice == "2":
		return m.start(m.o, "bash")
	case choice == "3":
		return m.dispose(ctx)
	default:
		m.printf("unknown choice %q\n", choice)
		return false, nil
	}
}

// emptyMenu picks a repository and provisions it.
func (m *manager) emptyMenu(ctx context.Context) (bool, error) {
	list := loadRepos(m.o.repos)
	m.println("\nno workspace — pick a repository to provision:")
	for i, e := range list.entries {
		line := fmt.Sprintf("  %d) %s", i+1, shortRepo(e.Repo))
		if e.Branch != "" {
			line += "  [" + e.Branch + "]"
		}
		m.printf("%s  (%s)\n", line, ago(e.Used, m.now()))
	}
	if len(list.entries) == 0 {
		m.println("  (nothing remembered yet)")
	}
	m.println("  or paste a repository URL")
	if len(list.entries) > 0 {
		m.println("  f <n>) forget an entry        q) quit")
	} else {
		m.println("  q) quit")
	}

	choice, err := m.prompt("> ")
	if err != nil {
		return true, err
	}
	var repo, branch string
	switch {
	case choice == "q" || choice == "":
		return true, nil

	case strings.HasPrefix(choice, "f "):
		n, err := strconv.Atoi(strings.TrimSpace(choice[2:]))
		if err != nil {
			m.println("forget takes an entry number, e.g. f 2")
			return false, nil
		}
		e, ok := list.forget(n)
		if !ok {
			m.printf("no entry %d\n", n)
			return false, nil
		}
		if err := list.save(); err != nil {
			m.printf("(the list could not be written: %v — forgotten for this session only)\n", err)
		}
		m.printf("forgot %s\n", shortRepo(e.Repo))
		return false, nil

	case strings.Contains(choice, "/") || strings.Contains(choice, ":"):
		repo = choice // a URL, not an index

	default:
		n, err := strconv.Atoi(choice)
		if err != nil {
			m.printf("unknown choice %q\n", choice)
			return false, nil
		}
		e, ok := list.at(n)
		if !ok {
			m.printf("no entry %d\n", n)
			return false, nil
		}
		repo, branch = e.Repo, e.Branch
	}

	// The branch: blank means the agent's start_work mints a codename
	// once it is working, which is the house default (a name coined
	// before the work exists ages badly).  A remembered branch is
	// offered back because resuming is the common case.
	def := "the default branch; the agent names its own line of work"
	if branch != "" {
		def = branch
	}
	answer, err := m.prompt(fmt.Sprintf("line of work to resume [%s]: ", def))
	if err != nil {
		return true, err
	}
	if answer != "" {
		branch = answer
	}

	m.printf("provisioning %s — clone through the relays under the forge-lint gate, this takes a minute…\n", shortRepo(repo))
	pctx, cancel := context.WithTimeout(ctx, operator.ProvisionTimeout)
	defer cancel()
	res, err := m.arc.Provision(pctx, repo, branch)
	if err != nil {
		var ref *operator.RefusedError
		if errors.As(err, &ref) {
			// The archivist's own words: they name the failing forge
			// requirement and where the runbook covers it.  Paraphrasing
			// would strip exactly what the operator needs.
			m.printf("\nrefused: %s\n", ref.Message)
			return false, nil
		}
		return true, err
	}

	// Recorded only now — a successful provision — so the list never
	// accumulates typos or unreachable hosts.
	list.touch(repo, res.Branch, m.now())
	if err := list.save(); err != nil {
		m.printf("(the recent-repositories list could not be written: %v)\n", err)
	}

	where := res.Repo
	if res.Branch != "" {
		where += " on " + res.Branch
	}
	m.printf("provisioned %s via %s\n", where, res.Endpoint)
	return false, nil
}

// dispose finishes the task.  A refusal over unpublished work is the
// system doing its job, so it is shown in full and the discard is
// offered as a separate, explicit act.
func (m *manager) dispose(ctx context.Context) (bool, error) {
	dctx, cancel := context.WithTimeout(ctx, operator.DisposeTimeout)
	defer cancel()

	res, err := m.arc.Dispose(dctx, false)
	if err == nil {
		if res.AlreadyEmpty {
			m.println("the workspace was already empty")
		} else {
			m.printf("disposed %s — the workspace is empty\n", res.Repo)
		}
		return false, nil
	}
	var ref *operator.RefusedError
	if !errors.As(err, &ref) {
		return true, err
	}

	m.printf("\nrefused: %s\n", ref.Message)
	m.println("\nDiscarding is irreversible: whatever the refusal names above is gone,")
	m.println("and only work already at the forge survives.")
	confirm, err := m.prompt("type DISCARD to destroy the workspace anyway, anything else to keep it: ")
	if err != nil {
		return true, err
	}
	if confirm != "DISCARD" {
		m.println("kept.")
		return false, nil
	}

	fctx, fcancel := context.WithTimeout(ctx, operator.DisposeTimeout)
	defer fcancel()
	res, err = m.arc.Dispose(fctx, true)
	if err != nil {
		// A corrupt workspace refuses even here, by design.
		var ref *operator.RefusedError
		if errors.As(err, &ref) {
			m.printf("still refused: %s\n", ref.Message)
			return false, nil
		}
		return true, err
	}
	m.printf("discarded %s — the workspace is empty\n", res.Repo)
	return false, nil
}

// startSession runs the agent under tmux and waits.  It does NOT exec:
// coming back here is the point — when the agent exits, the operator is
// returned to the menu, where disposing is one keystroke away.  A
// detach (Ctrl-Z d) leaves the session running and ends this
// invocation, which is the other half of why the agent lives in tmux at
// all.
func startSession(o options, cmd string) (bool, error) {
	dir := o.grange
	// Never mkdir tree/: an empty tree reads as CORRUPT to the
	// archivist's disk-derived state machine.
	if fi, err := os.Stat(filepath.Join(o.grange, "tree")); err == nil && fi.IsDir() {
		dir = filepath.Join(o.grange, "tree")
	}
	tm := exec.Command("tmux", "new-session", "-A", "-s", o.session, "-n", "main", "-c", dir, cmd)
	tm.Stdin, tm.Stdout, tm.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := tm.Run(); err != nil {
		// tmux exits non-zero when the program inside did; that is the
		// agent's news, not a workbench failure.
		fmt.Printf("(session ended: %v)\n", err)
	}
	if hasSession(o.session) {
		fmt.Printf("\ndetached — %s is still running.  `workbench` reattaches.\n", o.session)
		return true, nil
	}
	return false, nil
}

func hasSession(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

func attach(name string) error {
	tm := exec.Command("tmux", "attach", "-t", name)
	tm.Stdin, tm.Stdout, tm.Stderr = os.Stdin, os.Stdout, os.Stderr
	return tm.Run()
}

// prompt reads one trimmed line.  EOF ends the session manager rather
// than looping forever on a closed stdin — a workbench run without a
// terminal (a stray `docker exec` with no -i) must not spin.
func (m *manager) prompt(label string) (string, error) {
	m.printf("%s", label)
	line, err := m.in.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no input (run this from an interactive terminal: docker exec -it … workbench)")
	}
	return strings.TrimSpace(line), nil
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// defaultReposPath puts the list on the per-user caches bind, which is
// shared across that user's projects — the whole point of remembering
// repositories is that the next cell already knows them.  HOME is
// per-project, so it would forget every time.
func defaultReposPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/agent"
	}
	return filepath.Join(home, "caches", "cloister", "repos")
}
