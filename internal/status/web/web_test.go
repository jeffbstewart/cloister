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

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/agency"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/cellstate"
	"github.com/jeffbstewart/cloister/internal/runid"
)

type fixture struct {
	ts *httptest.Server
	id runid.ID
}

func mustRunID(t *testing.T) runid.ID {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatalf("runid.New() failed: %v", err)
	}
	return id
}

// newFixture fabricates a state volume the way the builder writes it: a
// status.json, an audit.jsonl (with a hostile param value), and one run
// log (with hostile content).
func newFixture(t *testing.T, busy bool) *fixture {
	t.Helper()
	state := t.TempDir()
	logs := filepath.Join(state, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	id := mustRunID(t)

	st := cellstate.Status{Busy: busy, UpdatedAt: time.Now().UTC()}
	if busy {
		st.Active = &cellstate.ActiveRun{RunID: id, Action: "test", StartedAt: time.Now().UTC()}
	} else {
		st.LastRun = &cellstate.RunSummary{RunID: id, Action: "test", Status: "ok"}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "status.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	al, err := audit.Open(filepath.Join(state, "audit.jsonl"), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	exit := 0
	rec := audit.New(id, "test", audit.DecisionRun, 0)
	rec.Status = "ok"
	rec.Detail = &audit.CommandDetail{
		Params:   map[string]string{"filter": "<script>alert(1)</script>"},
		Argv:     []string{"./gradlew", "test", "--tests", "<img src=x onerror=alert(2)>"},
		ExitCode: &exit,
		LogPath:  "/state/logs/" + id.String() + ".log",
	}
	if err := al.Append(rec); err != nil {
		t.Fatal(err)
	}
	al.Close()

	logBody := "line1 <script>evil()</script>\nline2\nline3\n"
	if err := os.WriteFile(filepath.Join(logs, id.String()+".log"), []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(New(Config{StateDir: state, Version: "test"}).Handler())
	t.Cleanup(ts.Close)
	return &fixture{ts: ts, id: id}
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// TestInferenceTabShowsSnapshot: with the agency's status volume mounted
// and a snapshot present, the /inference tab renders nodes, classes, and
// the op ledger — on its own 10s reload cadence — while the dashboard
// stays a short page that only links to it.
func TestInferenceTabShowsSnapshot(t *testing.T) {
	agencyDir := t.TempDir()
	snap := agency.Snapshot{
		WrittenAt: time.Now().UTC(),
		Nodes: map[string]agency.NodeStatus{
			"infer": {
				URL: "http://infer:11434", Present: true,
				Pinned: []string{"coder-model:30b"}, ResidencyKnown: true, Resident: []string{"coder-model:30b"},
				MaxInFlight: 4, InFlight: 1, QueuedInteractive: 2, QueuedBatch: 3,
			},
		},
		Classes: map[string]agency.ClassStatus{
			"interactive-code": {
				Priority: agency.PriorityInteractive,
				Deadline: agency.Duration(90 * time.Second), MaxDeadline: agency.Duration(5 * time.Minute),
				QueueWait: agency.Duration(10 * time.Second), MaxQueueWait: agency.Duration(time.Minute),
				Chain: []agency.ChainLinkStatus{{Node: "infer", Model: "coder-model:30b"}},
			},
		},
		Ops: []agency.OpRecord{{
			FinishedAt: time.Now().UTC(), Caller: "librarian", Class: "interactive-code",
			ServedBy: "infer/coder-model:30b", Status: 200, Tokens: 42,
		}},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agencyDir, agency.StatusFileName), b, 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(New(Config{StateDir: t.TempDir(), Version: "test", AgencyStatusDir: agencyDir}).Handler())
	defer ts.Close()
	_, body := get(t, ts.URL+"/inference")
	for _, want := range []string{
		"inference (agency)",
		`content="10"`,          // the tab's own reload cadence
		">present<",             // node reachability
		"coder-model:30b",       // pinned/resident models
		"1/4",                   // in-flight / maxInFlight
		"2/3",                   // queued interactive/batch
		"interactive-code",      // the class
		"1m30s",                 // deadline rendered as a duration string
		"infer/coder-model:30b", // chain link and op served-by
		"librarian",             // op caller
		">42<",                  // op tokens
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inference tab missing %q:\n%s", want, body)
		}
	}

	// The dashboard stays short: no snapshot tables, just the nav tab.
	_, dash := get(t, ts.URL+"/")
	if strings.Contains(dash, "coder-model:30b") {
		t.Error("dashboard still renders the inference tables, want them only on /inference")
	}
	if !strings.Contains(dash, `href="/inference"`) {
		t.Error("dashboard nav missing the inference tab link")
	}
	if !strings.Contains(dash, `content="5"`) {
		t.Error("dashboard lost its 5s reload cadence")
	}
}

// TestInferenceTabAbsentSnapshot: an unmounted volume or a not-yet-written
// snapshot renders as "no agency snapshot" — never an error page.
func TestInferenceTabAbsentSnapshot(t *testing.T) {
	cases := map[string]string{
		"no dir configured": "",
		"empty mount":       t.TempDir(),
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(New(Config{StateDir: t.TempDir(), Version: "test", AgencyStatusDir: dir}).Handler())
			defer ts.Close()
			resp, body := get(t, ts.URL+"/inference")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if !strings.Contains(body, "no agency snapshot") {
				t.Errorf("inference tab missing the no-snapshot notice:\n%s", body)
			}
		})
	}
}

func TestPendingApprovalCollapsedWhenResolved(t *testing.T) {
	// A gated op that has RESOLVED: it wrote pending_approval, then applied, under
	// the same opId.  The stale pending line must not show; only the terminal one.
	state := t.TempDir()
	al, err := audit.Open(filepath.Join(state, "audit.jsonl"), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	op := mustRunID(t)
	pending := audit.New(op, "apply_diff", "pending_approval", 0)
	pending.Detail = &audit.MutationDetail{Path: "build.gradle.kts"}
	al.Append(pending)
	applied := audit.New(op, "apply_diff", "applied", 0)
	applied.Detail = &audit.MutationDetail{Path: "build.gradle.kts"}
	al.Append(applied)
	al.Close()

	ts := httptest.NewServer(New(Config{StateDir: state, Version: "test"}).Handler())
	defer ts.Close()
	_, body := get(t, ts.URL+"/")
	if strings.Contains(body, "pending_approval</a>") {
		t.Error("resolved op still shows a stale pending_approval line")
	}
	if !strings.Contains(body, ">applied</td>") {
		t.Error("resolved op should show its terminal applied line")
	}

	// A genuinely-pending op (no terminal yet) still shows pending_approval.
	state2 := t.TempDir()
	al2, _ := audit.Open(filepath.Join(state2, "audit.jsonl"), audit.Options{})
	stillPending := audit.New(mustRunID(t), "apply_diff", "pending_approval", 0)
	stillPending.Detail = &audit.MutationDetail{Path: "x.gradle.kts"}
	al2.Append(stillPending)
	al2.Close()
	ts2 := httptest.NewServer(New(Config{StateDir: state2, Version: "test"}).Handler())
	defer ts2.Close()
	if _, body2 := get(t, ts2.URL+"/"); !strings.Contains(body2, "pending_approval</a>") {
		t.Error("a genuinely pending op should still show pending_approval")
	}
}

func TestDashboardShowsActiveRun(t *testing.T) {
	f := newFixture(t, true)
	resp, body := get(t, f.ts.URL+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{"RUNNING", "test", "/log/" + f.id.String() + "?tail=200"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q:\n%s", want, body)
		}
	}
}

func TestDashboardShowsIdleWithLastRun(t *testing.T) {
	f := newFixture(t, false)
	_, body := get(t, f.ts.URL+"/")
	for _, want := range []string{"idle", "/log/" + f.id.String()} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q:\n%s", want, body)
		}
	}
}

// TestHTMLEscapesHostileParams: agent-supplied param values AND the
// recorded argv must never reach the operator's browser as markup — the
// argv is attacker-influenced (a tampered manifest picks it).
func TestHTMLEscapesHostileParams(t *testing.T) {
	f := newFixture(t, false)
	for _, page := range []string{"/", "/audit"} {
		_, body := get(t, f.ts.URL+page)
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Errorf("%s: hostile param rendered unescaped", page)
		}
		if strings.Contains(body, "<img src=x onerror=alert(2)>") {
			t.Errorf("%s: hostile argv element rendered unescaped", page)
		}
		if !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s: escaped param value not shown", page)
		}
		// The resolved command is displayed (escaped) so tampering is visible.
		if !strings.Contains(body, "./gradlew") {
			t.Errorf("%s: resolved argv not displayed", page)
		}
	}
}

// TestLogServedAsPlainText: hostile log content is fine verbatim because
// the response is text/plain with nosniff — the browser will not run it.
func TestLogServedAsPlainText(t *testing.T) {
	f := newFixture(t, false)
	resp, body := get(t, f.ts.URL+"/log/"+f.id.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff header missing")
	}
	if !strings.Contains(body, "<script>evil()</script>") {
		t.Error("log content altered; must be verbatim in text/plain")
	}
}

func TestLogTail(t *testing.T) {
	f := newFixture(t, false)
	_, body := get(t, f.ts.URL+"/log/"+f.id.String()+"?tail=2")
	if body != "line2\nline3\n" {
		t.Errorf("tail=2 = %q, want last two lines", body)
	}
}

func TestLogRejectsInvalidRunIDs(t *testing.T) {
	f := newFixture(t, false)
	for _, bad := range []string{"not-a-uuid", f.id.String() + ".log", "..%2F..%2Fetc%2Fpasswd"} {
		resp, _ := get(t, f.ts.URL+"/log/"+bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("/log/%s → %d, want 400", bad, resp.StatusCode)
		}
	}
	// Valid id with no log file is a 404, not a 400.
	resp, _ := get(t, f.ts.URL+"/log/"+mustRunID(t).String())
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing log → %d, want 404", resp.StatusCode)
	}
}

func TestEmptyStateDir(t *testing.T) {
	ts := httptest.NewServer(New(Config{StateDir: t.TempDir(), Version: "test"}).Handler())
	defer ts.Close()
	resp, body := get(t, ts.URL+"/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "no runs recorded yet") {
		t.Errorf("fresh cell dashboard: %d %q", resp.StatusCode, body)
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(t, false)
	resp, _ := get(t, f.ts.URL+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
}
