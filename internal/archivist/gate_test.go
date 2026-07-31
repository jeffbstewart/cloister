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

package archivist

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeffbstewart/cloister/internal/endpoint"
)

// gateMainRules is the default branch's effective ruleset; DISMISS is
// substituted to break R2 (stale-approval dismissal).
const gateMainRules = `[
  {"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":DISMISS,"require_code_owner_review":true}},
  {"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"verify"}]}},
  {"type":"non_fast_forward"},
  {"type":"deletion"}
]`

// gateServer serves the bot-credential forge view of a compliant repo.
// The branch handler answers by shape — the default branch's rules, an
// agent/… branch unrestricted, any other name creation/update restricted —
// so it does not depend on forgelint's internal probe name.
func gateServer(t *testing.T, dismissStale bool) *httptest.Server {
	t.Helper()
	dismiss := "true"
	if !dismissStale {
		dismiss = "false"
	}
	main := strings.Replace(gateMainRules, "DISMISS", dismiss, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/op/repo", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"default_branch":"main","permissions":{"admin":false,"push":true}}`))
	})
	mux.HandleFunc("GET /repos/op/repo/rules/branches/", func(w http.ResponseWriter, r *http.Request) {
		branch := strings.TrimPrefix(r.URL.Path, "/repos/op/repo/rules/branches/")
		switch {
		case branch == "main":
			w.Write([]byte(main))
		case strings.HasPrefix(branch, "agent/"):
			w.Write([]byte(`[]`))
		default:
			w.Write([]byte(`[{"type":"creation"},{"type":"update"}]`))
		}
	})
	mux.HandleFunc("GET /repos/op/repo/contents/.github/CODEOWNERS", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("* @op\n/.github/ @op\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeForgeLint drops a .github/forge-lint.yaml into a staging tree.
func writeForgeLint(t *testing.T, staging, apiBase, repo string) {
	t.Helper()
	dir := filepath.Join(staging, ".github")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "forge: github\napiBase: " + apiBase + "\nrepo: " + repo +
		"\ndefaultBranch: main\noperator: op\nbot: bot\nrequiredChecks: [verify]\nagentNamespace: agent/\n"
	if err := os.WriteFile(filepath.Join(dir, "forge-lint.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gateEndpoint(t *testing.T, apiURL string) endpoint.Endpoint {
	t.Helper()
	cred := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(cred, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return endpoint.Endpoint{
		Name: "github.com", Canonical: "https://github.com/", Wire: "https://github.com/",
		Forge: endpoint.ForgeGitHub, API: apiURL, APIRelay: "unused:443", CredentialFile: cred,
		Bot: endpoint.Identity{Name: "cloister-bot", Email: "bot@cloister.test"},
	}
}

// forgeGate builds a gate whose transport is the test server's client.
func forgeGate(srv *httptest.Server) ForgeGate {
	return ForgeGate{Dial: func(_, _ string) (*http.Client, error) { return srv.Client(), nil }}
}

func TestForgeGatePassesCompliantRepo(t *testing.T) {
	srv := gateServer(t, true)
	staging := t.TempDir()
	writeForgeLint(t, staging, srv.URL, "op/repo")
	if err := forgeGate(srv).Verify(context.Background(), gateEndpoint(t, srv.URL+"/"), "op/repo", staging); err != nil {
		t.Fatalf("compliant repo refused: %v", err)
	}
}

func TestForgeGateRefusesOnViolation(t *testing.T) {
	srv := gateServer(t, false) // stale approvals survive -> R2 violation
	staging := t.TempDir()
	writeForgeLint(t, staging, srv.URL, "op/repo")
	err := forgeGate(srv).Verify(context.Background(), gateEndpoint(t, srv.URL+"/"), "op/repo", staging)
	var refused *GateRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("gate error = %v, want *GateRefusedError", err)
	}
	if refused.Requirement() != "R2" {
		t.Errorf("first failing requirement = %q, want R2", refused.Requirement())
	}
}

func TestForgeGateRefusesRepoMismatch(t *testing.T) {
	srv := gateServer(t, true)
	staging := t.TempDir()
	writeForgeLint(t, staging, srv.URL, "someone/else") // config governs a different repo
	err := forgeGate(srv).Verify(context.Background(), gateEndpoint(t, srv.URL+"/"), "op/repo", staging)
	if err == nil || !strings.Contains(err.Error(), "pins repo") || !errors.Is(err, ErrNotGrangeReady) {
		t.Fatalf("mismatched config repo = %v, want a not-grange-ready refusal naming the mismatch", err)
	}
}

func TestForgeGateRefusesMissingConfig(t *testing.T) {
	srv := gateServer(t, true)
	staging := t.TempDir() // no .github/forge-lint.yaml
	err := forgeGate(srv).Verify(context.Background(), gateEndpoint(t, srv.URL+"/"), "op/repo", staging)
	if err == nil || !strings.Contains(err.Error(), "Locking down a project") || !errors.Is(err, ErrNotGrangeReady) {
		t.Fatalf("missing config = %v, want a not-grange-ready refusal pointing at the runbook", err)
	}
}

func TestForgeGateRefusesNonGitHub(t *testing.T) {
	srv := gateServer(t, true)
	staging := t.TempDir()
	writeForgeLint(t, staging, srv.URL, "op/repo")
	ep := gateEndpoint(t, srv.URL+"/")
	ep.Forge = endpoint.ForgeGitea
	err := forgeGate(srv).Verify(context.Background(), ep, "op/repo", staging)
	if err == nil || !strings.Contains(err.Error(), "only github") || !errors.Is(err, ErrNotGrangeReady) {
		t.Fatalf("gitea endpoint = %v, want a fail-closed not-grange-ready refusal", err)
	}
}
