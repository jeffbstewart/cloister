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

package forgelint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compliantMainRules is the default branch's effective ruleset as the
// bot's effective-rules read returns it — note it carries no bypass roster,
// which is exactly the residue the gate must reason around.
const compliantMainRules = `[
  {"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":true,"require_code_owner_review":true}},
  {"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"verify"}]}},
  {"type":"non_fast_forward"},
  {"type":"deletion"}
]`

// provFixture serves the bot-credential view of a compliant repository;
// tests break individual fields to exercise the gate.
type provFixture struct {
	botAdmin     bool
	codeowners   string
	mainRules    string // effective rules on the default branch
	mainStatus   int    // 200, or 403 to simulate an unreadable branch
	outsideRules string // effective rules on a non-namespace branch (R8)
	insideRules  string // effective rules on an agent/ branch (R8)
}

func compliantProv() *provFixture {
	return &provFixture{
		codeowners:   "* @op\n/.github/ @op\n",
		mainRules:    compliantMainRules,
		mainStatus:   http.StatusOK,
		outsideRules: `[{"type":"creation"},{"type":"update"}]`,
		insideRules:  `[]`,
	}
}

func (f *provFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	admin := "false"
	if f.botAdmin {
		admin = "true"
	}
	mux.HandleFunc("GET /repos/op/repo", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"default_branch":"main","permissions":{"admin":` + admin + `,"push":true}}`))
	})
	// One subtree handler serves every branch the reader evaluates — the
	// default branch and the two hypothetical R8 probes — keyed on the
	// decoded name, so PathEscape's %2F round-trips without mux surprises.
	mux.HandleFunc("GET /repos/op/repo/rules/branches/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/repos/op/repo/rules/branches/") {
		case "main":
			if f.mainStatus != http.StatusOK {
				w.WriteHeader(f.mainStatus)
				return
			}
			w.Write([]byte(f.mainRules))
		case nsProbeBase:
			w.Write([]byte(f.outsideRules))
		case "agent/" + nsProbeBase:
			w.Write([]byte(f.insideRules))
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /repos/op/repo/contents/.github/CODEOWNERS", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(f.codeowners))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func provSnapshot(t *testing.T, f *provFixture) (*Config, *Snapshot, error) {
	t.Helper()
	srv := f.serve(t)
	cfg, err := LoadConfig(writeConfig(t, strings.Replace(validConfig,
		"apiBase: https://api.github.com", "apiBase: "+srv.URL, 1)))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := NewGitHub(cfg, "tok", srv.Client()).ProvisionSnapshot(context.Background())
	return cfg, snap, err
}

func TestProvisionGatePassesCompliantRepo(t *testing.T) {
	cfg, snap, err := provSnapshot(t, compliantProv())
	if err != nil {
		t.Fatal(err)
	}
	verdicts := Check(cfg, snap)
	if r := Gate(verdicts); !r.OK {
		t.Fatalf("compliant repo blocked on %v", r.Blocking)
	}
	// The residue is UNVERIFIED, never a false pass, and R5 is the fact that
	// makes R1's residue safe to discount.
	if got := statusOf(verdicts, "R1"); got != Unverified {
		t.Errorf("R1 = %s, want UNVERIFIED (bypass roster is admin-only)", got)
	}
	if got := statusOf(verdicts, "R6"); got != Unverified {
		t.Errorf("R6 = %s, want UNVERIFIED (secrets inventory is admin-only)", got)
	}
	if got := statusOf(verdicts, "R5"); got != OK {
		t.Errorf("R5 = %s, want ok (self-permission is bot-readable)", got)
	}
	if snap.BypassKnown || snap.SecretsKnown {
		t.Errorf("residue must stay unread: BypassKnown=%v SecretsKnown=%v", snap.BypassKnown, snap.SecretsKnown)
	}
}

func TestProvisionGateRefuses(t *testing.T) {
	tests := map[string]struct {
		mutate func(*provFixture)
		want   string // a requirement that must appear in the refusal
	}{
		"stale approvals survive a push": {
			func(f *provFixture) {
				f.mainRules = strings.Replace(f.mainRules, `"dismiss_stale_reviews_on_push":true`, `"dismiss_stale_reviews_on_push":false`, 1)
			}, "R2",
		},
		"bot is a repository admin": {
			func(f *provFixture) { f.botAdmin = true }, "R5",
		},
		"namespace is not confined": {
			func(f *provFixture) { f.insideRules = `[{"type":"creation"},{"type":"update"}]` }, "R8",
		},
		"protection has lapsed (empty rules)": {
			func(f *provFixture) { f.mainRules = `[]` }, "R1",
		},
		"codeowners lets the bot approve": {
			func(f *provFixture) { f.codeowners = "* @op @bot\n/.github/ @op\n" }, "R2",
		},
	}
	for name, tc := range tests {
		f := compliantProv()
		tc.mutate(f)
		cfg, snap, err := provSnapshot(t, f)
		if err != nil {
			t.Errorf("%s: snapshot error: %v", name, err)
			continue
		}
		r := Gate(Check(cfg, snap))
		if r.OK {
			t.Errorf("%s: gate passed, want refusal", name)
			continue
		}
		found := false
		for _, v := range r.Blocking {
			if v.Req == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: blocking %v, want %s among them", name, r.Blocking, tc.want)
		}
	}
}

func TestProvisionCodeownersIgnoresWorkingTree(t *testing.T) {
	// The gate reads a repo it has NOT checked out; a CODEOWNERS sitting in
	// the archivist's CWD (its own jail, a stale grange) must not be mistaken
	// for the target repo's.  Poison the CWD with a bot-naming CODEOWNERS —
	// which would sink R2 if read — and confirm the operator-only API copy
	// still governs and the gate passes.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "CODEOWNERS"), []byte("* @op @bot\n/.github/ @op\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg, snap, err := provSnapshot(t, compliantProv())
	if err != nil {
		t.Fatal(err)
	}
	if r := Gate(Check(cfg, snap)); !r.OK {
		t.Fatalf("working-tree CODEOWNERS leaked into the provision read; blocked on %v", r.Blocking)
	}
}

func TestProvisionSnapshotUnreadableBranchIsError(t *testing.T) {
	// A branch the credential cannot read must fail closed — an error the
	// gate surfaces as a refusal, never an empty (and falsely clean) read.
	f := compliantProv()
	f.mainStatus = http.StatusForbidden
	if _, _, err := provSnapshot(t, f); err == nil {
		t.Fatal("unreadable default-branch rules: want error, got nil")
	}
}
