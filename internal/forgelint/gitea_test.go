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

type giteaFixture struct {
	whitelist  string // JSON array of usernames whose approvals count
	adminExtra string // extra admin collaborator entry, or ""
}

func compliantGitea() *giteaFixture {
	return &giteaFixture{whitelist: `["op"]`}
}

func (f *giteaFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(path, body string) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(body))
		})
	}
	reply("/repos/op/repo", `{"default_branch":"main"}`)
	reply("/repos/op/repo/branch_protections", `[{
	  "branch_name":"main","rule_name":"main",
	  "enable_push":false,
	  "required_approvals":1,"dismiss_stale_approvals":true,
	  "enable_status_check":true,"status_check_contexts":["verify"],
	  "enable_approvals_whitelist":true,"approvals_whitelist_username":`+f.whitelist+`,
	  "protected_file_patterns":".gitea/workflows/**;.github/workflows/**"
	}]`)
	collabs := `[{"login":"bot","permissions":{"admin":false}}`
	if f.adminExtra != "" {
		collabs += `,{"login":"` + f.adminExtra + `","permissions":{"admin":true}}`
	}
	collabs += `]`
	reply("/repos/op/repo/collaborators", collabs)
	reply("/repos/op/repo/collaborators/bot/permission", `{"permission":"write"}`)
	reply("/repos/op/repo/actions/secrets", `[]`)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func giteaSnapshot(t *testing.T, f *giteaFixture) (*Config, *Snapshot) {
	t.Helper()
	srv := f.serve(t)
	body := strings.Replace(validConfig, "forge: github", "forge: gitea", 1)
	body = strings.Replace(body, "apiBase: https://api.github.com", "apiBase: "+srv.URL, 1)
	cfg, err := LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := NewGitea(cfg, "tok", srv.Client()).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cfg, snap
}

// TestGiteaCompliantRepoPassesExceptNamespace: on Gitea, R8 stays
// UNVERIFIED by design until the multi-rule pilot — everything else ok.
func TestGiteaCompliantRepoPassesExceptNamespace(t *testing.T) {
	cfg, snap := giteaSnapshot(t, compliantGitea())
	for _, v := range Check(cfg, snap) {
		want := OK
		if v.Req == "R8" {
			want = Unverified
		}
		if v.Status != want {
			t.Errorf("%s = %s (%s), want %s", v.Req, v.Status, v.Detail, want)
		}
	}
}

func TestGiteaBotOnApprovalsWhitelistIsViolation(t *testing.T) {
	f := compliantGitea()
	f.whitelist = `["op","bot"]`
	cfg, snap := giteaSnapshot(t, f)
	verdicts := Check(cfg, snap)
	if got := statusOf(verdicts, "R2"); got != Violation {
		t.Errorf("R2 = %s, want VIOLATION (the bot's approval would count)", got)
	}
}

func TestGiteaExtraAdminIsViolation(t *testing.T) {
	f := compliantGitea()
	f.adminExtra = "sneaky"
	cfg, snap := giteaSnapshot(t, f)
	if got := statusOf(Check(cfg, snap), "R1"); got != Violation {
		t.Errorf("R1 = %s, want VIOLATION (an admin besides the operator bypasses protection)", got)
	}
}
