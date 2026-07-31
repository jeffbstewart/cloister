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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/archive/archivetest"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forge"
)

// fakeForge is a scriptable forge.Client: the await tests mutate its
// state from the rig's Sleep hook to simulate review activity arriving
// while the verb waits.  The unused authoring methods fail loudly.
type fakeForge struct {
	mu       sync.Mutex
	pr       forge.PR
	reviews  []forge.Review
	comments []forge.ReviewComment
	err      error // when set, every used method fails with it
}

func (f *fakeForge) CreatePR(context.Context, string, string, string, string, string) (forge.PR, error) {
	return forge.PR{}, errors.New("fakeForge: CreatePR is not part of the await surface")
}

func (f *fakeForge) UpdatePR(context.Context, string, int, string, string) (forge.PR, error) {
	return forge.PR{}, errors.New("fakeForge: UpdatePR is not part of the await surface")
}

func (f *fakeForge) Checks(context.Context, string, string) ([]forge.Check, error) {
	return nil, errors.New("fakeForge: Checks is not part of the await surface")
}

func (f *fakeForge) Diff(context.Context, string, int) (string, error) {
	return "", errors.New("fakeForge: Diff is not part of the await surface")
}

func (f *fakeForge) Reply(context.Context, string, int, int64, string) (forge.ReviewComment, error) {
	return forge.ReviewComment{}, errors.New("fakeForge: Reply is not part of the await surface")
}

func (f *fakeForge) FindPR(_ context.Context, _ string, head string) (forge.PR, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return forge.PR{}, false, f.err
	}
	if f.pr.Number == 0 || f.pr.Head != head {
		return forge.PR{}, false, nil
	}
	return f.pr, true, nil
}

func (f *fakeForge) GetPR(context.Context, string, int) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return forge.PR{}, f.err
	}
	return f.pr, nil
}

func (f *fakeForge) Reviews(context.Context, string, int) ([]forge.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]forge.Review(nil), f.reviews...), nil
}

func (f *fakeForge) ReviewComments(context.Context, string, int) ([]forge.ReviewComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]forge.ReviewComment(nil), f.comments...), nil
}

func (f *fakeForge) addReview(state, author string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews = append(f.reviews, forge.Review{Author: author, State: state, Time: time.Unix(1_754_000_100, 0)})
}

func (f *fakeForge) addComment(id int64, author, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, forge.ReviewComment{ID: id, Author: author, Body: body, Time: time.Unix(1_754_000_100, 0)})
}

func (f *fakeForge) setState(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pr.State = state
}

func (f *fakeForge) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakeWait is await_review's pinned clock: Sleep advances a fake now by
// exactly the requested duration and fires the test's per-tick hook — no
// real time ever passes.
type fakeWait struct {
	mu     sync.Mutex
	now    time.Time
	tick   int
	onTick func(tick int)
}

func (w *fakeWait) Now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

func (w *fakeWait) Sleep(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.now = w.now.Add(d)
	w.tick++
	tick := w.tick
	fn := w.onTick
	w.mu.Unlock()
	if fn != nil {
		fn(tick)
	}
	return ctx.Err()
}

// awaitTable mirrors internal/archive's test table: a one-entry github
// endpoint whose credential file exists, carrying the bot identity.
func awaitTable(t *testing.T) *endpoint.Table {
	t.Helper()
	cred := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(cred, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `endpoints:
  - name: github.com
    canonical: https://github.com/
    wire: https://github.com/
    forge: github
    api: https://api.github.com/
    apiRelay: github-api-relay:443
    credentialFile: ` + filepath.ToSlash(cred) + `
    bot:
      name: cloister-bot
      email: bot@cloister.test
`
	p := filepath.Join(t.TempDir(), "endpoints.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	tbl, err := endpoint.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

// openPR is the scripted PR the current branch resolves to.
func openPR() forge.PR {
	return forge.PR{Number: 7, Title: "step five", State: forge.StateOpen,
		URL: "https://github.com/op/repo/pull/7", Head: "agent/pr", Base: "main", SHA: "abc"}
}

// newAwaitServer builds the await rig: a seeded workspace whose origin
// reads as the canonical github URL (so RemoteInfo resolves it against
// the test table; nothing ever dials the network), the scriptable forge,
// the pinned clock, and a collecting auditor.
func newAwaitServer(t *testing.T, ff *fakeForge) (*Server, *fakeWait, *fakeAuditor) {
	t.Helper()
	tmp := t.TempDir()
	_, dir := archivetest.Seed(t, tmp)
	archivetest.GitRun(t, dir, "remote", "set-url", "origin", "https://github.com/op/repo")

	var mu sync.Mutex
	at := time.Unix(1_753_000_000, 0).UTC()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(time.Second)
		return at
	}
	a, err := archive.New(dir, archive.WithClock(clock), archive.WithEndpoints(awaitTable(t)))
	if err != nil {
		t.Fatalf("archive.New(%s): %v", dir, err)
	}
	t.Cleanup(func() { a.Close() })

	g := archive.AdoptArchive(a)
	g.AdoptForge(ff)
	wait := &fakeWait{now: time.Unix(1_754_000_000, 0).UTC()}
	aud := &fakeAuditor{}
	return New(Config{Version: "test", Grange: g, Audit: aud, Now: wait.Now, Sleep: wait.Sleep}), wait, aud
}

func newAwaitFixture(t *testing.T, ff *fakeForge) (*fixture, *fakeWait, *fakeAuditor) {
	t.Helper()
	srv, wait, aud := newAwaitServer(t, ff)
	f := &fixture{session: dial(t, srv)}
	f.ok(t, "start_work", map[string]any{"name": "agent/pr"})
	return f, wait, aud
}

// TestAwaitReviewApproval: an approval arriving mid-wait ends it, the
// answer carries the new review and the deterministic wait length, and
// the completed poll leaves exactly one remote_ok record.
func TestAwaitReviewApproval(t *testing.T) {
	ff := &fakeForge{pr: openPR()}
	f, wait, aud := newAwaitFixture(t, ff)
	wait.onTick = func(tick int) {
		if tick == 2 {
			ff.addReview(forge.ReviewApproved, "jeff")
		}
	}

	res := asJSON(t, f.ok(t, "await_review", map[string]any{"maxWait": "10m"}))
	if got := field[string](t, res, "outcome"); got != "approved" {
		t.Errorf("outcome = %q, want approved", got)
	}
	if got := field[string](t, res, "waited"); got != "30s" {
		t.Errorf("waited = %q, want the two polled intervals (30s)", got)
	}
	reviews := field[[]any](t, res, "new_reviews")
	if len(reviews) != 1 {
		t.Fatalf("new_reviews = %v, want the one approval", reviews)
	}
	if r, ok := reviews[0].(map[string]any); !ok || r["state"] != forge.ReviewApproved || r["author"] != "jeff" {
		t.Errorf("new review = %v, want jeff's approval", reviews[0])
	}

	recs := aud.records()
	if len(recs) != 1 || recs[0].Tool != "await_review" || recs[0].Decision != audit.DecisionRemoteOK {
		t.Fatalf("audit = %+v, want one await_review remote_ok record", recs)
	}
	if d := recs[0].Remote(); d == nil || d.PR != 7 || d.Endpoint != "github.com" || d.Branch != "agent/pr" {
		t.Errorf("audit detail = %+v, want PR 7 on agent/pr at github.com", recs[0].Detail)
	}
}

// TestAwaitReviewBaseline: activity counts from the moment of the call —
// pre-existing reviews and comments never retrigger, and only the
// genuinely new comment comes back.
func TestAwaitReviewBaseline(t *testing.T) {
	ff := &fakeForge{pr: openPR()}
	ff.addReview(forge.ReviewCommented, "jeff")
	ff.addComment(1, "jeff", "old thread")
	f, wait, _ := newAwaitFixture(t, ff)
	wait.onTick = func(tick int) {
		if tick == 1 {
			ff.addComment(2, "jeff", "new thought")
		}
	}

	res := asJSON(t, f.ok(t, "await_review", map[string]any{"maxWait": "10m"}))
	if got := field[string](t, res, "outcome"); got != "commented" {
		t.Errorf("outcome = %q, want commented", got)
	}
	if reviews := field[[]any](t, res, "new_reviews"); len(reviews) != 0 {
		t.Errorf("new_reviews = %v, want none (the baseline review is not news)", reviews)
	}
	comments := field[[]any](t, res, "new_comments")
	if len(comments) != 1 {
		t.Fatalf("new_comments = %v, want just the new one", comments)
	}
	if c, ok := comments[0].(map[string]any); !ok || c["id"] != float64(2) {
		t.Errorf("new comment = %v, want id 2", comments[0])
	}
}

// TestAwaitReviewMerge: a merge mid-wait is terminal regardless of
// review deltas.
func TestAwaitReviewMerge(t *testing.T) {
	ff := &fakeForge{pr: openPR()}
	f, wait, _ := newAwaitFixture(t, ff)
	wait.onTick = func(tick int) {
		if tick == 1 {
			ff.setState(forge.StateMerged)
		}
	}
	res := asJSON(t, f.ok(t, "await_review", nil))
	if got := field[string](t, res, "outcome"); got != "merged" {
		t.Errorf("outcome = %q, want merged", got)
	}
	if got := field[string](t, res, "state"); got != forge.StateMerged {
		t.Errorf("state = %q, want merged", got)
	}
}

// TestAwaitReviewLatestVerdictWins: within one poll window the LATEST
// verdict decides — changes-requested then approval reads as approved,
// and a lone changes-requested reads as itself.
func TestAwaitReviewLatestVerdictWins(t *testing.T) {
	ff := &fakeForge{pr: openPR()}
	f, wait, _ := newAwaitFixture(t, ff)
	wait.onTick = func(tick int) {
		if tick == 1 {
			ff.addReview(forge.ReviewChangesRequested, "jeff")
			ff.addReview(forge.ReviewApproved, "jeff")
		}
	}
	res := asJSON(t, f.ok(t, "await_review", nil))
	if got := field[string](t, res, "outcome"); got != "approved" {
		t.Errorf("outcome = %q, want approved (the later verdict)", got)
	}

	ff2 := &fakeForge{pr: openPR()}
	f2, wait2, _ := newAwaitFixture(t, ff2)
	wait2.onTick = func(tick int) {
		if tick == 1 {
			ff2.addReview(forge.ReviewChangesRequested, "jeff")
		}
	}
	res = asJSON(t, f2.ok(t, "await_review", nil))
	if got := field[string](t, res, "outcome"); got != "changes_requested" {
		t.Errorf("outcome = %q, want changes_requested", got)
	}
}

// TestAwaitReviewTimeout: a quiet wait ends as a successful answer —
// outcome timeout, not a tool error — after exactly maxWait of polling,
// and still leaves its remote_ok record.
func TestAwaitReviewTimeout(t *testing.T) {
	ff := &fakeForge{pr: openPR()}
	f, wait, aud := newAwaitFixture(t, ff)

	res := asJSON(t, f.ok(t, "await_review", map[string]any{"maxWait": "45s"}))
	if got := field[string](t, res, "outcome"); got != "timeout" {
		t.Errorf("outcome = %q, want timeout", got)
	}
	if wait.tick != 3 {
		t.Errorf("polled %d times in a 45s wait at 15s intervals, want 3", wait.tick)
	}
	recs := aud.records()
	if len(recs) != 1 || recs[0].Decision != audit.DecisionRemoteOK {
		t.Errorf("audit = %+v, want one remote_ok record for the quiet wait", recs)
	}
}

// TestAwaitReviewToleratesFlakyPolls: a couple of failed polls do not
// burn the wait — it recovers and still catches the approval.
func TestAwaitReviewToleratesFlakyPolls(t *testing.T) {
	ff := &fakeForge{pr: openPR()}
	f, wait, _ := newAwaitFixture(t, ff)
	wait.onTick = func(tick int) {
		switch tick {
		case 1:
			ff.setErr(errors.New("relay hiccup"))
		case 3:
			ff.setErr(nil)
			ff.addReview(forge.ReviewApproved, "jeff")
		}
	}
	res := asJSON(t, f.ok(t, "await_review", map[string]any{"maxWait": "10m"}))
	if got := field[string](t, res, "outcome"); got != "approved" {
		t.Errorf("outcome = %q, want approved despite the flaky polls", got)
	}
}

// TestAwaitReviewDeadEndpointFails: consecutive poll failures end the
// wait as a remote error rather than spinning silently to the deadline.
func TestAwaitReviewDeadEndpointFails(t *testing.T) {
	ff := &fakeForge{pr: openPR()}
	f, wait, aud := newAwaitFixture(t, ff)
	wait.onTick = func(tick int) {
		if tick == 1 {
			ff.setErr(errors.New("endpoint down"))
		}
	}
	text, isErr := f.call(t, "await_review", map[string]any{"maxWait": "30m"})
	if !isErr || !strings.Contains(text, "consecutive polls failed") {
		t.Fatalf("await against a dead endpoint = %q (err=%v), want the consecutive-failure error", text, isErr)
	}
	recs := aud.records()
	if len(recs) != 1 || recs[0].Decision != audit.DecisionRemoteError {
		t.Errorf("audit = %+v, want one remote_error record", recs)
	}
}

// TestAwaitReviewNoOpenPR: with nothing proposed there is nothing to
// await, and the refusal names the next step.
func TestAwaitReviewNoOpenPR(t *testing.T) {
	f, _, _ := newAwaitFixture(t, &fakeForge{})
	text, isErr := f.call(t, "await_review", nil)
	if !isErr || !strings.Contains(text, "no open PR") {
		t.Errorf("await without a PR = %q (err=%v), want the no-open-PR refusal", text, isErr)
	}
}

// TestAwaitReviewBadArguments: maxWait is validated before anything is
// resolved or polled, and unknown keys die at the strict decoder.
func TestAwaitReviewBadArguments(t *testing.T) {
	f := newFixtureWith(t, Config{Version: "test"})
	for name, args := range map[string]map[string]any{
		"unparseable": {"maxWait": "banana"},
		"negative":    {"maxWait": "-5m"},
		"over cap":    {"maxWait": "25h"},
		"unknown key": {"pr": 7},
	} {
		text, isErr := f.call(t, "await_review", args)
		if !isErr || !strings.Contains(text, "bad arguments") {
			t.Errorf("%s (%v) = %q (err=%v), want a bad-arguments refusal", name, args, text, isErr)
		}
	}
}

// TestAwaitReviewProgressNotifications: a caller that supplies a
// progress token is told what the wait is doing — the opening notice and
// a heartbeat per quiet poll — before the call unblocks.
func TestAwaitReviewProgressNotifications(t *testing.T) {
	ff := &fakeForge{pr: openPR()}
	srv, wait, _ := newAwaitServer(t, ff)
	wait.onTick = func(tick int) {
		if tick == 2 {
			ff.addReview(forge.ReviewApproved, "jeff")
		}
	}

	var mu sync.Mutex
	var notes []string
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.mcp.Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()
			notes = append(notes, req.Params.Message)
		},
	})
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "start_work", Arguments: map[string]any{"name": "agent/pr"},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "await_review", Arguments: map[string]any{"maxWait": "10m"},
		Meta: mcp.Meta{"progressToken": "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("await_review errored: %v", res.Content)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 2 {
		t.Fatalf("notes = %q, want the opening notice and one heartbeat", notes)
	}
	if !strings.Contains(notes[0], "PR #7") || !strings.Contains(notes[0], "Waiting up to") {
		t.Errorf("opening notice = %q, want the PR and the bound named", notes[0])
	}
	if !strings.Contains(notes[1], "Still waiting") {
		t.Errorf("heartbeat = %q, want a still-waiting note", notes[1])
	}
}
