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

package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GitHub must satisfy the forge-agnostic Client seam.
var _ Client = (*GitHub)(nil)

// fake spins an httptest GitHub and a client aimed at it.
func fake(t *testing.T, handler http.HandlerFunc) *GitHub {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	g, err := NewGitHub(srv.URL+"/", func() (string, error) { return "test-token", nil }, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestCreatePRAccepts201(t *testing.T) {
	g := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/o/r/pulls" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["head"] != "agent/x" || body["base"] != "main" {
			t.Errorf("body = %v", body)
		}
		w.WriteHeader(http.StatusCreated) // 201: the forgelint client would have called this a failure
		json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "title": body["title"], "state": "open",
			"html_url": "https://github.com/o/r/pull/7",
			"head":     map[string]string{"ref": "agent/x", "sha": "abc123"},
			"base":     map[string]string{"ref": "main"},
		})
	})
	pr, err := g.CreatePR(context.Background(), "o/r", "agent/x", "main", "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || pr.State != "open" || pr.Head != "agent/x" || pr.SHA != "abc123" {
		t.Errorf("pr = %+v", pr)
	}
}

func TestFindPR(t *testing.T) {
	g := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head"); got != "o:agent/x" {
			t.Errorf("head filter = %q, want owner-qualified", got)
		}
		w.Write([]byte(`[]`))
	})
	if _, found, err := g.FindPR(context.Background(), "o/r", "agent/x"); err != nil || found {
		t.Errorf("FindPR on empty = %v, %v", found, err)
	}
}

func TestMergedStateFolds(t *testing.T) {
	g := fake(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"number": 3, "state": "closed", "merged": true,
			"head": map[string]string{"ref": "agent/x", "sha": "abc"}, "base": map[string]string{"ref": "main"}})
	})
	pr, err := g.GetPR(context.Background(), "o/r", 3)
	if err != nil || pr.State != "merged" {
		t.Errorf("pr = %+v, %v; a closed+merged PR reports merged", pr, err)
	}
}

func TestChecksPaginates(t *testing.T) {
	calls := 0
	g := fake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		runs := make([]map[string]string, 0, 100)
		n := 100
		if r.URL.Query().Get("page") == "2" {
			n = 1
		}
		for i := 0; i < n; i++ {
			runs = append(runs, map[string]string{"name": "verify", "status": "completed", "conclusion": "success"})
		}
		json.NewEncoder(w).Encode(map[string]any{"check_runs": runs})
	})
	checks, err := g.Checks(context.Background(), "o/r", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(checks) != 101 {
		t.Errorf("calls = %d, checks = %d; want a full page to fetch the next", calls, len(checks))
	}
}

// TestChecksRefusesRunawayPagination: a listing that never returns a
// short page is an error, not a silent truncation that could hide a
// failing check.
func TestChecksRefusesRunawayPagination(t *testing.T) {
	g := fake(t, func(w http.ResponseWriter, r *http.Request) {
		runs := make([]map[string]string, 100)
		for i := range runs {
			runs[i] = map[string]string{"name": "verify", "status": "completed", "conclusion": "success"}
		}
		json.NewEncoder(w).Encode(map[string]any{"check_runs": runs})
	})
	if _, err := g.Checks(context.Background(), "o/r", "abc"); err == nil {
		t.Error("an unterminated listing must surface an error, not a partial result")
	}
}

func TestDiffUsesAcceptHeader(t *testing.T) {
	g := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github.diff" {
			t.Errorf("Accept = %q", got)
		}
		w.Write([]byte("diff --git a/x b/x\n"))
	})
	diff, err := g.Diff(context.Background(), "o/r", 7)
	if err != nil || !strings.HasPrefix(diff, "diff --git") {
		t.Errorf("diff = %q, %v", diff, err)
	}
}

func TestReply(t *testing.T) {
	g := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/7/comments/99/replies" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 100, "in_reply_to_id": 99,
			"user": map[string]string{"login": "operator"}, "body": "done"})
	})
	c, err := g.Reply(context.Background(), "o/r", 7, 99, "done")
	if err != nil || c.ID != 100 || c.InReplyTo != 99 {
		t.Errorf("reply = %+v, %v", c, err)
	}
}

// TestErrorNeverEchoesTheCredential: failure text carries the clipped
// response body, never the request whose header holds the token.
func TestErrorNeverEchoesTheCredential(t *testing.T) {
	g := fake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
	})
	_, err := g.CreatePR(context.Background(), "o/r", "agent/x", "main", "t", "b")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Errorf("error text leaks the credential: %v", err)
	}
	if !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("error text lost the actionable body: %v", err)
	}
}

func TestRepoFromRemote(t *testing.T) {
	cases := []struct {
		remote, want string
		wantErr      bool
	}{
		{"https://github.com/jeff/cloister.git", "jeff/cloister", false},
		{"https://github.com/jeff/cloister", "jeff/cloister", false},
		{"https://github.com/jeff/cloister/", "jeff/cloister", false},
		{"https://gitea.example/jeff/x.git", "", true}, // outside the canonical prefix
		{"https://github.com/jeff", "", true},          // no repo name
		{"https://github.com/jeff/a/b.git", "", true},  // too deep
	}
	for _, tc := range cases {
		got, err := RepoFromRemote("https://github.com/", tc.remote)
		if tc.wantErr != (err != nil) || got != tc.want {
			t.Errorf("RepoFromRemote(%q) = %q, %v; want %q, err=%v", tc.remote, got, err, tc.want, tc.wantErr)
		}
	}
}
