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
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/operator"
)

// The menus are driven against a fake workspace: scripted keystrokes in,
// captured output and a recorded call log out.  What is asserted is the
// session manager's JUDGEMENT — which moves it offers for a given state,
// what it refuses to do on the operator's behalf, and whether the
// archivist's own words survive to the screen.

type fakeArchivist struct {
	states []operator.Status // consumed one per Status call; the last repeats
	calls  []string

	provision   operator.ProvisionResult
	provisioner error
	disposeErr  error // returned for force=false
	forceErr    error // returned for force=true
}

func (f *fakeArchivist) Status(context.Context) (operator.Status, error) {
	f.calls = append(f.calls, "status")
	if len(f.states) == 0 {
		return operator.Status{State: operator.StateEmpty}, nil
	}
	st := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return st, nil
}

func (f *fakeArchivist) Provision(_ context.Context, repo, branch string) (operator.ProvisionResult, error) {
	f.calls = append(f.calls, "provision "+repo+" "+branch)
	if f.provisioner != nil {
		return operator.ProvisionResult{}, f.provisioner
	}
	res := f.provision
	if res.Repo == "" {
		res = operator.ProvisionResult{Repo: shortRepo(repo), Branch: branch, Endpoint: "github.com"}
	}
	return res, nil
}

func (f *fakeArchivist) Dispose(_ context.Context, force bool) (operator.DisposeResult, error) {
	if force {
		f.calls = append(f.calls, "dispose force")
		if f.forceErr != nil {
			return operator.DisposeResult{}, f.forceErr
		}
		return operator.DisposeResult{Repo: "op/repo", Disposed: true}, nil
	}
	f.calls = append(f.calls, "dispose")
	if f.disposeErr != nil {
		return operator.DisposeResult{}, f.disposeErr
	}
	return operator.DisposeResult{Repo: "op/repo", Disposed: true}, nil
}

// rig builds a manager over scripted input.  tmux is replaced by a
// recorder: the tests are about the menus, and a real session would need
// a terminal.
func rig(t *testing.T, arc *fakeArchivist, keys string) (*manager, *bytes.Buffer, *[]string) {
	t.Helper()
	var out bytes.Buffer
	started := []string{}
	// A usable environment prompt: starting the agent now depends on
	// one, because an agent without its rules is the failure this
	// pre-flight exists to prevent.  Tests that want the failure point
	// o.stockPrompt somewhere else.
	stock := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(stock, []byte("the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &manager{
		arc: arc,
		o: options{
			session:     "agent",
			agentCmd:    "qwen",
			grange:      t.TempDir(),
			repos:       filepath.Join(t.TempDir(), "repos"),
			stockPrompt: stock,
		},
		in:  bufio.NewReader(strings.NewReader(keys)),
		out: &out,
		now: func() time.Time { return at },
		start: func(_ options, cmd string) (bool, error) {
			started = append(started, cmd)
			return true, nil // "detached" — ends the invocation
		},
		// Stubbed, and deliberately NOT defaulted to the real thing behind a
		// nil check.  resetHomeAfterDispose deletes $HOME recursively; a nil
		// guard in production code would let a missing wire-up degrade to
		// "the cleaner silently never runs", which is the failure mode
		// hardest to notice.  A test that forgets this panics immediately
		// instead, which is the signal we want.
		resetHome: func() {},
	}
	return m, &out, &started
}

func TestCorruptIsReportedAndNothingIsOffered(t *testing.T) {
	arc := &fakeArchivist{states: []operator.Status{{State: operator.StateCorrupt}}}
	m, out, started := rig(t, arc, "")

	err := m.loop(context.Background())
	if err == nil {
		t.Fatal("a corrupt workspace returned success")
	}
	if !strings.Contains(out.String(), "CORRUPT") || !strings.Contains(out.String(), "host-side") {
		t.Errorf("corrupt report = %q; it must name the state and where recovery lives", out.String())
	}
	// The load-bearing part: it did not try to clean up.
	for _, c := range arc.calls {
		if strings.HasPrefix(c, "dispose") || strings.HasPrefix(c, "provision") {
			t.Errorf("acted on a corrupt workspace: %v", arc.calls)
		}
	}
	if len(*started) != 0 {
		t.Errorf("started a session on a corrupt workspace: %v", *started)
	}
}

func TestProvisionedOffersTheAgentAndStartsIt(t *testing.T) {
	arc := &fakeArchivist{states: []operator.Status{
		{State: operator.StateProvisioned, Repo: "op/repo", Branch: "agent/x", ProvisionedAt: at.Unix()},
	}}
	m, out, started := rig(t, arc, "1\n")

	if err := m.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*started) != 1 || (*started)[0] != "qwen" {
		t.Errorf("started = %v, want the agent", *started)
	}
	if !strings.Contains(out.String(), "op/repo on agent/x") {
		t.Errorf("the menu did not name the workspace: %q", out.String())
	}
}

// TestDisposeRefusalIsShownVerbatimAndDiscardIsSeparate: the refusal
// names the unpublished work, and destroying it must be a second,
// explicit act — never a retry the manager performs on its own.
func TestDisposeRefusalIsShownVerbatimAndDiscardIsSeparate(t *testing.T) {
	const reason = "archivist: 2 checkpoints are not yet at the endpoint"
	arc := &fakeArchivist{
		states:     []operator.Status{{State: operator.StateProvisioned, Repo: "op/repo"}},
		disposeErr: &operator.RefusedError{Verb: "dispose", Message: reason},
	}
	// Choose dispose, then decline the discard, then quit.
	m, out, _ := rig(t, arc, "3\nno\nq\n")
	if err := m.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), reason) {
		t.Errorf("the archivist's reason was not shown: %q", out.String())
	}
	for _, c := range arc.calls {
		if c == "dispose force" {
			t.Fatalf("the manager forced a refused dispose on its own: %v", arc.calls)
		}
	}
	if !strings.Contains(out.String(), "kept.") {
		t.Errorf("declining the discard was not confirmed: %q", out.String())
	}
}

// TestDiscardNeedsTheExactWord: anything but DISCARD keeps the
// workspace.  A bare "y" must not destroy work.
func TestDiscardNeedsTheExactWord(t *testing.T) {
	refusal := &operator.RefusedError{Verb: "dispose", Message: "unpublished work"}
	for _, answer := range []string{"y", "yes", "discard", "DISCARD "} {
		arc := &fakeArchivist{
			states:     []operator.Status{{State: operator.StateProvisioned, Repo: "op/repo"}},
			disposeErr: refusal,
		}
		m, _, _ := rig(t, arc, "3\n"+answer+"\nq\n")
		if err := m.loop(context.Background()); err != nil {
			t.Fatal(err)
		}
		forced := false
		for _, c := range arc.calls {
			if c == "dispose force" {
				forced = true
			}
		}
		// "DISCARD " trims to DISCARD and IS the confirmation; the rest are not.
		if want := answer == "DISCARD "; forced != want {
			t.Errorf("answer %q forced=%v, want %v", answer, forced, want)
		}
	}
}

// TestCorruptRefusesEvenTheDiscard: dispose refuses a markerless tree at
// any force, and the manager must report that rather than escalate.
func TestCorruptRefusesEvenTheDiscard(t *testing.T) {
	arc := &fakeArchivist{
		states:     []operator.Status{{State: operator.StateProvisioned, Repo: "op/repo"}},
		disposeErr: &operator.RefusedError{Verb: "dispose", Message: "no provenance marker"},
		forceErr:   &operator.RefusedError{Verb: "dispose", Message: "no provenance marker"},
	}
	m, out, _ := rig(t, arc, "3\nDISCARD\nq\n")
	if err := m.loop(context.Background()); err != nil {
		t.Fatalf("a refused force-dispose must not be a fatal error: %v", err)
	}
	if !strings.Contains(out.String(), "still refused") {
		t.Errorf("the second refusal was not reported: %q", out.String())
	}
}

// TestProvisionRecordsOnlyOnSuccess: a refused provision must leave the
// recent-repositories list untouched, which is what keeps typos and
// unreachable hosts out of it.
func TestProvisionRecordsOnlyOnSuccess(t *testing.T) {
	arc := &fakeArchivist{
		states:      []operator.Status{{State: operator.StateEmpty}},
		provisioner: &operator.RefusedError{Verb: "provision", Message: "op/repo fails R2: stale approvals survive"},
	}
	m, out, _ := rig(t, arc, "https://github.com/op/repo\n\nq\n")
	if err := m.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "R2") {
		t.Errorf("the gate's verdict was not shown: %q", out.String())
	}
	if l := loadRepos(m.o.repos); len(l.entries) != 0 {
		t.Errorf("a refused provision was remembered: %+v", l.entries)
	}
}

func TestProvisionRemembersTheRepoAndBranch(t *testing.T) {
	arc := &fakeArchivist{states: []operator.Status{
		{State: operator.StateEmpty},
		{State: operator.StateProvisioned, Repo: "op/repo", Branch: "agent/brisk-otter"},
	}}
	// A URL, a typed branch, then quit at the provisioned menu.
	m, _, _ := rig(t, arc, "https://github.com/op/repo\nagent/brisk-otter\nq\n")
	if err := m.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	l := loadRepos(m.o.repos)
	if len(l.entries) != 1 {
		t.Fatalf("remembered %+v, want one entry", l.entries)
	}
	if l.entries[0].Repo != "https://github.com/op/repo" || l.entries[0].Branch != "agent/brisk-otter" {
		t.Errorf("remembered %+v", l.entries[0])
	}
	if l.entries[0].Used != at.Unix() {
		t.Errorf("stamp = %d, want the injected clock %d", l.entries[0].Used, at.Unix())
	}
}

// TestRememberedRepoIsPickableByNumber closes the loop the LRU exists
// for: the second visit is a digit, not a URL, and the remembered
// branch comes back as the default.
func TestRememberedRepoIsPickableByNumber(t *testing.T) {
	arc := &fakeArchivist{states: []operator.Status{{State: operator.StateEmpty}}}
	m, _, _ := rig(t, arc, "1\n\nq\n")

	seed := loadRepos(m.o.repos)
	seed.touch("https://github.com/op/remembered", "agent/prior", at.Add(-time.Hour))
	if err := seed.save(); err != nil {
		t.Fatal(err)
	}

	if err := m.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "provision https://github.com/op/remembered agent/prior"
	for _, c := range arc.calls {
		if c == want {
			return
		}
	}
	t.Errorf("calls = %v, want %q — the entry's branch should default", arc.calls, want)
}

func TestForgetDropsAnEntryWithoutProvisioning(t *testing.T) {
	arc := &fakeArchivist{states: []operator.Status{{State: operator.StateEmpty}}}
	m, out, _ := rig(t, arc, "f 1\nq\n")

	seed := loadRepos(m.o.repos)
	seed.touch("https://github.com/op/typo", "", at)
	if err := seed.save(); err != nil {
		t.Fatal(err)
	}

	if err := m.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l := loadRepos(m.o.repos); len(l.entries) != 0 {
		t.Errorf("still remembered: %+v", l.entries)
	}
	if !strings.Contains(out.String(), "forgot") {
		t.Errorf("no confirmation: %q", out.String())
	}
	for _, c := range arc.calls {
		if strings.HasPrefix(c, "provision") {
			t.Errorf("forget provisioned something: %v", arc.calls)
		}
	}
}

// TestClosedStdinEndsRatherThanSpins: a `docker exec` without -i gives a
// menu no input at all.  It must terminate, not loop on EOF forever.
func TestClosedStdinEndsRatherThanSpins(t *testing.T) {
	arc := &fakeArchivist{states: []operator.Status{{State: operator.StateProvisioned, Repo: "op/repo"}}}
	m, _, _ := rig(t, arc, "")

	done := make(chan error, 1)
	go func() { done <- m.loop(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("closed stdin returned success; it should say what went wrong")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the menu spun on EOF instead of ending")
	}
}

// TestUnknownChoiceReprompts: a fat-fingered menu entry loops back
// rather than falling through to some default action.
func TestUnknownChoiceReprompts(t *testing.T) {
	arc := &fakeArchivist{states: []operator.Status{{State: operator.StateProvisioned, Repo: "op/repo"}}}
	m, out, started := rig(t, arc, "9\nq\n")
	if err := m.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unknown choice") {
		t.Errorf("output = %q, want the choice rejected", out.String())
	}
	if len(*started) != 0 {
		t.Errorf("an unknown choice started %v", *started)
	}
}

func TestStatusErrorEndsTheSession(t *testing.T) {
	arc := &failingArchivist{}
	m, _, _ := rig(t, &fakeArchivist{}, "")
	m.arc = arc
	if err := m.loop(context.Background()); err == nil {
		t.Error("an unreachable archivist returned success")
	}
}

type failingArchivist struct{ fakeArchivist }

func (f *failingArchivist) Status(context.Context) (operator.Status, error) {
	return operator.Status{}, errors.New("connection refused")
}
