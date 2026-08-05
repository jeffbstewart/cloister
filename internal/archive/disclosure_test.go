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

package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/archive/archivetest"
	"github.com/jeffbstewart/cloister/internal/endpoint"
)

// The disclosure gate as archive sees it: an optional hook that can refuse
// a provision on the repository's NAME alone.  What the acknowledgment
// means, and what shape it takes, is internal/disclosure's business — these
// tests are about the two properties that belong here.
//
//  1. A refusal stops the provision, and the workspace stays EMPTY.
//  2. It runs BEFORE THE CLONE.  That ordering is the substantive claim:
//     the gate asks whether this repository's source may leave the machine,
//     which is a question about the repository rather than about anything
//     inside it.  A gate that ran after the clone would have fetched the
//     tree it just decided not to permit, and would cost the operator a
//     multi-minute wait to learn it.

// disclosureRig is grangeRig with a disclosure hook, and a counter proving
// whether the cloner ever ran.
type disclosureRig struct {
	*grangeRig
	clones int
}

func newDisclosureRig(t *testing.T, gate func(repo string) error) *disclosureRig {
	t.Helper()
	archivetest.RequireGit(t)
	tmp := t.TempDir()
	base := &grangeRig{
		t:      t,
		root:   filepath.Join(tmp, "grange"),
		origin: filepath.Join(tmp, "origin.git"),
		gate:   &stubGate{},
		clock:  &stepClock{t: clockEpoch, step: time.Second},
	}
	seedForgeOrigin(t, base.origin, tmp)
	if err := os.MkdirAll(base.root, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &disclosureRig{grangeRig: base}
	g, err := NewGrange(GrangeConfig{
		Root:       base.root,
		Table:      testTable(t),
		Gate:       base.gate,
		Now:        base.clock.Now,
		Disclosure: gate,
		Cloner: func(ctx context.Context, ep endpoint.Endpoint, url, dst string) error {
			r.clones++
			return base.cloner(ctx, ep, url, dst)
		},
	})
	if err != nil {
		t.Fatalf("NewGrange: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	base.g = g
	return r
}

var errUndisclosed = errors.New("provision refused: not acknowledged")

func TestDisclosureRefusalStopsProvisionBeforeCloning(t *testing.T) {
	var sawRepo string
	r := newDisclosureRig(t, func(repo string) error {
		sawRepo = repo
		return errUndisclosed
	})

	_, err := r.g.Provision(context.Background(), grangeRepoURL, BranchName{})
	if !errors.Is(err, errUndisclosed) {
		t.Fatalf("Provision err = %v, want the disclosure refusal", err)
	}
	// The gate is given the resolved "owner/name", not the URL — the
	// acknowledgment variable is derived from it, so a URL here would
	// produce a different variable for the same repository.
	if sawRepo != "op/repo" {
		t.Errorf("the gate saw repo %q, want %q — the acknowledgment is keyed on owner/name", sawRepo, "op/repo")
	}
	// THE ordering claim.
	if r.clones != 0 {
		t.Errorf("the repository was cloned %d time(s) despite the refusal — a repo we have decided not to permit must never be fetched, and the operator must not wait out a clone to be told no", r.clones)
	}
	// And nothing was left behind.
	st, err := r.g.state()
	if err != nil {
		t.Fatal(err)
	}
	if st != StateEmpty {
		t.Errorf("workspace state = %s after a refused provision, want %s", st, StateEmpty)
	}
}

// TestDisclosureRefusalLeavesProvisionRetryable: the operator's fix is to
// set a variable and try again, so the refusal must not have poisoned the
// workspace on its way out.
func TestDisclosureRefusalLeavesProvisionRetryable(t *testing.T) {
	refuse := true
	r := newDisclosureRig(t, func(string) error {
		if refuse {
			return errUndisclosed
		}
		return nil
	})
	ctx := context.Background()

	if _, err := r.g.Provision(ctx, grangeRepoURL, BranchName{}); !errors.Is(err, errUndisclosed) {
		t.Fatalf("Provision err = %v, want the refusal", err)
	}
	refuse = false // the operator acknowledged and redeployed
	if _, err := r.g.Provision(ctx, grangeRepoURL, BranchName{}); err != nil {
		t.Fatalf("Provision after acknowledgment: %v", err)
	}
	if r.clones != 1 {
		t.Errorf("cloned %d times, want exactly 1 (none for the refusal, one for the success)", r.clones)
	}
}

// TestNoDisclosureGateIsTheOrdinaryCase: a cell that sends its source
// nowhere must be completely unaffected.  This is the qwen cell, and it is
// every cell today.
func TestNoDisclosureGateIsTheOrdinaryCase(t *testing.T) {
	r := newDisclosureRig(t, nil)
	if _, err := r.g.Provision(context.Background(), grangeRepoURL, BranchName{}); err != nil {
		t.Fatalf("Provision with no disclosure gate: %v", err)
	}
	if r.clones != 1 {
		t.Errorf("cloned %d times, want 1", r.clones)
	}
}
