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

// End-to-end tests: the real Client driving the real Server over httptest,
// exercising the sink contract and its security properties (append-only
// history, server-authoritative timestamps, token gating, rate limits).
package sink

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/cellstate"
	"github.com/jeffbstewart/cloister/internal/runid"
)

const testToken = "test-token-abc123"

func mustRunID(t *testing.T) runid.ID {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatalf("runid.New() failed: %v", err)
	}
	return id
}

// newServer returns a running Server and its state dir.
func newServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	dir := cfg.StateDir
	if dir == "" {
		dir = t.TempDir()
		cfg.StateDir = dir
	}
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func readAudit(t *testing.T, dir string) []audit.Record {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []audit.Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r audit.Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func TestRequiresToken(t *testing.T) {
	if _, err := New(Config{StateDir: t.TempDir()}); err == nil {
		t.Error("New must refuse to start without a token")
	}
}

func TestStreamFinalizeAndFetch(t *testing.T) {
	srv, _ := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)
	id := mustRunID(t)

	stream := c.StartRun(id)
	if _, err := io.WriteString(stream, "hello\nworld\n"); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream close: %v", err)
	}

	got, err := c.FetchLog(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\nworld\n" {
		t.Errorf("fetched %q", got)
	}

	// After finalize, further writes to the run are rejected.
	if err := c.Finalize(id); err != nil {
		t.Fatal(err)
	}
	if err := c.Reupload(id, strings.NewReader("tampered")); err == nil {
		t.Error("finalized run accepted a reupload; history must be immutable")
	}
	got, _ = c.FetchLog(id)
	if string(got) != "hello\nworld\n" {
		t.Errorf("finalized log changed to %q", got)
	}
}

func TestDiffStoreRoundTrip(t *testing.T) {
	srv, dir := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)
	id := mustRunID(t)

	payload := []byte("=== applied diff ===\n--- a/f\n+++ b/f\n@@ @@\n-x\n+y\n")
	if err := c.PutDiff(id, payload); err != nil {
		t.Fatal(err)
	}
	// Stored gzip'd, sharded on the opId's random tail.
	want := filepath.Join(dir, "diffs", id.Shard(), id.String()+".diff.gz")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("diff not stored at %s: %v", want, err)
	}
	got, err := c.FetchDiff(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("fetched %q, want %q", got, payload)
	}

	// A missing diff is a clean error, not a crash.
	if _, err := c.FetchDiff(mustRunID(t)); err == nil {
		t.Error("FetchDiff of a missing op should error")
	}
}

func TestDiffStoreCapTruncates(t *testing.T) {
	srv, _ := newServer(t, Config{MaxDiffBytes: 64})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)
	id := mustRunID(t)

	if err := c.PutDiff(id, bytes.Repeat([]byte("z"), 4096)); err != nil {
		t.Fatal(err)
	}
	got, err := c.FetchDiff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64 {
		t.Errorf("stored %d bytes, want the 64-byte cap", len(got))
	}
}

func TestApprovalRegisterDecidePoll(t *testing.T) {
	srv, _ := newServer(t, Config{ApprovalPoll: 200 * time.Millisecond})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)
	id := mustRunID(t)

	if err := c.RegisterPending(id, "apply_diff", "build.gradle.kts"); err != nil {
		t.Fatal(err)
	}
	if rec, err := c.PollDecision(id); err != nil || rec.Decision != approval.Pending {
		t.Fatalf("initial poll = %v, %v; want pending", rec.Decision, err)
	}
	if err := c.Decide(id, approval.Approved); err != nil {
		t.Fatal(err)
	}
	if rec, _ := c.PollDecision(id); rec.Decision != approval.Approved {
		t.Errorf("after approve, poll = %v, want approved", rec.Decision)
	}
	// A terminal decision is immutable.
	if err := c.Decide(id, approval.Rejected); err == nil {
		t.Error("re-deciding a resolved op should fail")
	}
}

func TestApprovalWithdraw(t *testing.T) {
	srv, _ := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)

	// Withdraw a pending op → it's gone; a poll then 404s (an error).
	id := mustRunID(t)
	if err := c.RegisterPending(id, "extract_url", "https://x.example/p"); err != nil {
		t.Fatal(err)
	}
	if err := c.Withdraw(id); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if _, err := c.PollDecision(id); err == nil {
		t.Error("poll after withdraw should fail (op removed)")
	}
	// Idempotent: withdrawing a gone op is fine.
	if err := c.Withdraw(id); err != nil {
		t.Errorf("second withdraw: %v", err)
	}
	// A decided op can't be withdrawn (409, tolerated by the client), and the
	// decision stands (withdraw is not a decision — one-way glass).
	id2 := mustRunID(t)
	if err := c.RegisterPending(id2, "research", "q"); err != nil {
		t.Fatal(err)
	}
	if err := c.Decide(id2, approval.Approved); err != nil {
		t.Fatal(err)
	}
	if err := c.Withdraw(id2); err != nil {
		t.Errorf("withdraw of a decided op should be tolerated, got %v", err)
	}
	if rec, _ := c.PollDecision(id2); rec.Decision != approval.Approved {
		t.Error("a decided op's decision must survive a withdraw attempt")
	}
}

func TestApprovalRegistrationRateLimited(t *testing.T) {
	srv, _ := newServer(t, Config{ApprovalPerSec: 1}) // burst 2×
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)

	var errs int
	for i := 0; i < 6; i++ {
		if c.RegisterPending(mustRunID(t), "research", "q") != nil {
			errs++
		}
	}
	if errs == 0 {
		t.Error("a burst of approval registrations should hit the rate limit")
	}
}

func TestApprovalTimeout(t *testing.T) {
	srv, _ := newServer(t, Config{ApprovalTimeout: 40 * time.Millisecond, ApprovalPoll: 100 * time.Millisecond})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)
	id := mustRunID(t)

	if err := c.RegisterPending(id, "apply_diff", "build.gradle.kts"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond) // exceed the 40ms approval timeout
	rec, err := c.PollDecision(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Decision != approval.Timeout {
		t.Errorf("poll after timeout = %v, want rejected_timeout", rec.Decision)
	}
}

func TestApprovalPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newServer(t, Config{StateDir: dir})
	ts := httptest.NewServer(srv.Handler())
	c := NewClient(ts.URL, testToken)
	id := mustRunID(t)
	if err := c.RegisterPending(id, "apply_diff", "x.gradle.kts"); err != nil {
		t.Fatal(err)
	}
	if err := c.Decide(id, approval.Approved); err != nil {
		t.Fatal(err)
	}
	ts.Close()
	srv.Close()

	// A fresh server over the same /state reloads the pending record + decision.
	srv2, _ := newServer(t, Config{StateDir: dir})
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()
	c2 := NewClient(ts2.URL, testToken)
	if rec, err := c2.PollDecision(id); err != nil || rec.Decision != approval.Approved {
		t.Errorf("decision not persisted across restart: %v, %v", rec.Decision, err)
	}
}

func TestApprovalsUIApproveRejectAndCSRF(t *testing.T) {
	srv, _ := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)
	// No redirects, so we can assert the 303 the form returns.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// A pending op with a diff shows on the (unauthenticated) approvals page.
	id := mustRunID(t)
	if err := c.RegisterPending(id, "apply_diff", "build.gradle.kts"); err != nil {
		t.Fatal(err)
	}
	if err := c.PutDiff(id, []byte("=== applied diff ===\n+plugins {}\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/approvals")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{id.String(), "build.gradle.kts", "plugins {}"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("approvals page missing %q", want)
		}
	}

	// A POST without the CSRF token is refused.
	bad, err := noRedirect.PostForm(ts.URL+"/approvals/"+id.String()+"/decision", url.Values{"decision": {"approve"}})
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusForbidden {
		t.Errorf("missing-CSRF POST = %d, want 403", bad.StatusCode)
	}

	// With the CSRF token, Approve resolves the op to approved.
	ok, err := noRedirect.PostForm(ts.URL+"/approvals/"+id.String()+"/decision",
		url.Values{"csrf": {srv.csrfToken}, "decision": {"approve"}})
	if err != nil {
		t.Fatal(err)
	}
	ok.Body.Close()
	if ok.StatusCode != http.StatusSeeOther {
		t.Errorf("approve POST = %d, want 303", ok.StatusCode)
	}
	if rec, _ := c.PollDecision(id); rec.Decision != approval.Approved {
		t.Errorf("after UI approve, decision = %v, want approved", rec.Decision)
	}

	// Reject resolves a second op to rejected.
	id2 := mustRunID(t)
	c.RegisterPending(id2, "apply_diff", "settings.gradle.kts")
	rej, err := noRedirect.PostForm(ts.URL+"/approvals/"+id2.String()+"/decision",
		url.Values{"csrf": {srv.csrfToken}, "decision": {"reject"}})
	if err != nil {
		t.Fatal(err)
	}
	rej.Body.Close()
	if rec, _ := c.PollDecision(id2); rec.Decision != approval.Rejected {
		t.Errorf("after UI reject, decision = %v, want rejected", rec.Decision)
	}
}

func TestAuthRejectsBadToken(t *testing.T) {
	srv, _ := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// No token.
	resp, err := http.Post(ts.URL+"/api/audit", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token audit POST = %d, want 401", resp.StatusCode)
	}

	// Wrong token via the client.
	bad := NewClient(ts.URL, "wrong")
	if err := bad.Append(audit.New(mustRunID(t), "x", audit.DecisionRun, 0)); err == nil {
		t.Error("wrong token accepted")
	}

	// Status pages need no token.
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz needs no auth; got %d", resp.StatusCode)
	}
}

// TestAuditTimestampIsServerAuthoritative: a client-supplied ts must be
// discarded and replaced by the sink's clock, so a stolen token cannot
// backdate history.
func TestAuditTimestampIsServerAuthoritative(t *testing.T) {
	srv, dir := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)

	forged := audit.New(mustRunID(t), "build", audit.DecisionRun, 0)
	forged.Time = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := c.Append(forged); err != nil {
		t.Fatal(err)
	}
	recs := readAudit(t, dir)
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	if recs[0].Time.Year() == 1999 {
		t.Errorf("forged client timestamp survived: %s", recs[0].Time)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	srv, _ := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)
	id := mustRunID(t)

	if err := c.PutStatus(cellstate.Status{
		Busy:   true,
		Active: &cellstate.ActiveRun{RunID: id, Action: "test", StartedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	st, err := cellstate.Read(srv.statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Busy || st.Active == nil || st.Active.RunID != id {
		t.Errorf("status not persisted: %+v", st)
	}
	if st.UpdatedAt.IsZero() {
		t.Error("sink did not stamp UpdatedAt")
	}
}

func TestPerRunSizeCap(t *testing.T) {
	srv, _ := newServer(t, Config{MaxRunBytes: 1024, LogBytesPerSec: 1 << 30})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)
	id := mustRunID(t)

	stream := c.StartRun(id)
	_, _ = io.Copy(stream, bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
	_ = stream.Close()

	got, _ := c.FetchLog(id)
	if int64(len(got)) > 1024 {
		t.Errorf("stored %d bytes, want <= the 1024 cap", len(got))
	}
}

func getCode(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// incompressible builds deterministic high-entropy bytes (an LCG) so gzip can't
// shrink them — needed to exercise the size-based retention prune.
func incompressible(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func TestTranscriptStoreAndServe(t *testing.T) {
	srv, _ := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)

	id := mustRunID(t)
	body := "prompt: scholar-prompt-1\nquery: how do X\nsearch \"X\" -> 2 results: https://a/1 https://b/2\nanswer: use X\n"
	if err := c.PutTranscript(id, []byte(body)); err != nil {
		t.Fatal(err)
	}
	// Served on the public read route, text/plain + nosniff (agent-influenced text).
	resp, err := http.Get(ts.URL + "/research/" + id.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET transcript = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("transcript view missing nosniff")
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("transcript round-trip:\n got %q\nwant %q", got, body)
	}
	if code := getCode(t, ts.URL+"/research/"+mustRunID(t).String()); code != http.StatusNotFound {
		t.Errorf("unknown transcript = %d, want 404", code)
	}
}

func TestTranscriptRetentionPrunesOldest(t *testing.T) {
	srv, _ := newServer(t, Config{TranscriptRetention: 100 << 10}) // 100 KiB total
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)

	var ids []runid.ID
	for i := 0; i < 3; i++ { // each ~64 KiB incompressible → total ~192 KiB > cap
		id := mustRunID(t)
		ids = append(ids, id)
		if err := c.PutTranscript(id, incompressible(64<<10, byte(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if code := getCode(t, ts.URL+"/research/"+ids[2].String()); code != http.StatusOK {
		t.Errorf("newest transcript = %d, want 200 (kept)", code)
	}
	if code := getCode(t, ts.URL+"/research/"+ids[0].String()); code != http.StatusNotFound {
		t.Errorf("oldest transcript = %d, want 404 (pruned oldest-first)", code)
	}
}

func TestAuditRateLimit(t *testing.T) {
	srv, _ := newServer(t, Config{AuditPerSec: 1}) // burst 2
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.URL, testToken)

	var rejected bool
	for i := 0; i < 10; i++ {
		if err := c.Append(audit.New(mustRunID(t), "x", audit.DecisionRun, 0)); err != nil {
			rejected = true
			break
		}
	}
	if !rejected {
		t.Error("audit rate limit never tripped over a 10-record burst")
	}
}

// TestTraversalRejected: an untrusted runId in the API path must be
// rejected by runid.Parse, not turned into a filesystem path.
func TestTraversalRejected(t *testing.T) {
	srv, _ := newServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/runs/not-a-uuid/log", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid runId = %d, want 400", resp.StatusCode)
	}
}
