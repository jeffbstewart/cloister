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
	"strings"
	"testing"
)

// ghFixture serves a compliant repository; tests mutate fields to break it.
type ghFixture struct {
	dismissStale  string // "true" | "false"
	secretsStatus int    // 200 or 403
	codeowners    string
}

func compliantGH() *ghFixture {
	return &ghFixture{
		dismissStale:  "true",
		secretsStatus: http.StatusOK,
		codeowners:    "* @op\n/.github/ @op\n",
	}
}

func (f *ghFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(path, body string) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(body))
		})
	}
	reply("/repos/op/repo", `{"default_branch":"main"}`)
	reply("/repos/op/repo/rulesets", `[{"id":1,"target":"branch","enforcement":"active"},{"id":2,"target":"branch","enforcement":"active"}]`)
	reply("/repos/op/repo/rulesets/1", `{
	  "enforcement":"active",
	  "conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"],"exclude":[]}},
	  "rules":[
	    {"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":`+f.dismissStale+`,"require_code_owner_review":true}},
	    {"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"verify"}]}},
	    {"type":"non_fast_forward"},
	    {"type":"deletion"}
	  ],
	  "bypass_actors":[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]
	}`)
	reply("/repos/op/repo/rulesets/2", `{
	  "enforcement":"active",
	  "conditions":{"ref_name":{"include":["~ALL"],"exclude":["refs/heads/agent/**","refs/heads/main"]}},
	  "rules":[{"type":"creation"},{"type":"update"}],
	  "bypass_actors":[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]
	}`)
	mux.HandleFunc("GET /repos/op/repo/contents/.github/CODEOWNERS", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(f.codeowners))
	})
	reply("/repos/op/repo/collaborators/bot/permission", `{"permission":"write","role_name":"write"}`)
	mux.HandleFunc("GET /repos/op/repo/actions/secrets", func(w http.ResponseWriter, _ *http.Request) {
		if f.secretsStatus != http.StatusOK {
			w.WriteHeader(f.secretsStatus)
			return
		}
		w.Write([]byte(`{"total_count":0,"secrets":[]}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func ghSnapshot(t *testing.T, f *ghFixture) (*Config, *Snapshot) {
	t.Helper()
	srv := f.serve(t)
	cfg, err := LoadConfig(writeConfig(t, strings.Replace(validConfig,
		"apiBase: https://api.github.com", "apiBase: "+srv.URL, 1)))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := NewGitHub(cfg, "tok", srv.Client()).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cfg, snap
}

func TestGitHubCompliantRepoPassesAllRequirements(t *testing.T) {
	cfg, snap := ghSnapshot(t, compliantGH())
	for _, v := range Check(cfg, snap) {
		if v.Status != OK {
			t.Errorf("%s = %s (%s), want ok", v.Req, v.Status, v.Detail)
		}
	}
}

func TestGitHubStaleApprovalSurvivesPushesIsViolation(t *testing.T) {
	f := compliantGH()
	f.dismissStale = "false"
	cfg, snap := ghSnapshot(t, f)
	if got := statusOf(Check(cfg, snap), "R2"); got != Violation {
		t.Errorf("R2 = %s, want VIOLATION", got)
	}
}

func TestGitHubUnreadableSecretsAreUnverifiedNotPassed(t *testing.T) {
	f := compliantGH()
	f.secretsStatus = http.StatusForbidden
	cfg, snap := ghSnapshot(t, f)
	if got := statusOf(Check(cfg, snap), "R6"); got != Unverified {
		t.Errorf("R6 = %s, want UNVERIFIED", got)
	}
}

func TestGitHubCodeOwnersNamingTheBotDefeatsOwnerReview(t *testing.T) {
	f := compliantGH()
	f.codeowners = "* @op @bot\n/.github/ @op\n"
	cfg, snap := ghSnapshot(t, f)
	if got := statusOf(Check(cfg, snap), "R2"); got != Violation {
		t.Errorf("R2 = %s, want VIOLATION (bot co-owns the catch-all)", got)
	}
}

func TestCodeownersOwnersLastMatchWins(t *testing.T) {
	catchAll, workflows := codeownersOwners("# c\n* @op\n/.github/ @op @extra\n")
	if len(catchAll) != 1 || catchAll[0] != "@op" {
		t.Errorf("catchAll = %v", catchAll)
	}
	if len(workflows) != 2 {
		t.Errorf("workflows = %v, want the later, more specific owners", workflows)
	}
}
