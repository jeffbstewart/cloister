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

// await_review completes the authorship loop (docs/archivist.md): the
// agent publishes, proposes, then waits on the operator without being
// told to look — a bounded long-poll against the endpoint, emitting MCP
// progress notifications while it waits, the same pattern the scribe
// uses for approval holds.  The operator reviews when they review;
// nobody has to announce it.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forge"
	"github.com/jeffbstewart/cloister/internal/mcpserve"
)

const (
	// awaitPollInterval spaces the endpoint polls: fast enough that the
	// agent learns of review activity promptly, slow enough that an
	// hour-long wait costs a few hundred API calls, far under the
	// credential's rate limit.
	awaitPollInterval = 15 * time.Second
	// awaitDefaultWait applies when maxWait is omitted; awaitMaxWait
	// bounds it.  The long-poll is bounded by design — an agent that
	// wants to wait longer calls again.
	awaitDefaultWait = 15 * time.Minute
	awaitMaxWait     = time.Hour
	// awaitMaxPollErrors ends the wait after this many CONSECUTIVE
	// failed polls: one flaky poll must not burn a long wait, but a dead
	// endpoint must not spin silently until the deadline either.
	awaitMaxPollErrors = 4
)

// registerAwaitReview registers await_review WITHOUT add's
// lock-around-the-handler — the one verb that skips it.  The wait spans
// minutes, and s.mu guards the working tree and the grange's
// live-workspace pointer, which the poll never touches: the handler
// takes the lock itself just long enough to resolve its target, then
// waits on the captured forge client with the lock released, so a wait
// never wedges the rest of the verb surface behind a sleeper.
func (s *Server) registerAwaitReview() {
	s.mcp.AddTool(&mcp.Tool{
		Name: "await_review",
		Description: "Block until review activity on the current branch's open PR: a new review (approval or changes requested), " +
			"new review comments, or merge/close.  Activity counts from the moment of the call — reviews already posted do not retrigger.  " +
			"Emits progress notifications while it waits; outcome \"timeout\" means maxWait passed quietly (call again to keep waiting).  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"maxWait": mcpserve.Str(`how long to wait, as a Go duration ("90s", "15m"); default 15m, max 1h`),
			},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.awaitReview)
}

func (s *Server) awaitReview(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		MaxWait string `json:"maxWait"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	maxWait := awaitDefaultWait
	if a.MaxWait != "" {
		d, err := time.ParseDuration(a.MaxWait)
		if err != nil {
			return mcpserve.ErrResult("bad arguments: maxWait: " + err.Error()), nil
		}
		if d <= 0 || d > awaitMaxWait {
			return mcpserve.ErrResult(fmt.Sprintf("bad arguments: maxWait %v is outside (0, %v]", d, awaitMaxWait)), nil
		}
		maxWait = d
	}

	ep, repo, pr, fc, errRes := s.awaitTarget(ctx)
	if errRes != nil {
		return errRes, nil
	}
	d := audit.RemoteDetail{Endpoint: ep.Name, Branch: pr.Head, PR: pr.Number}

	// The baseline: activity is measured from the moment of the call, so
	// feedback the agent has already had every chance to read does not
	// retrigger.  Merge and close are absolute states, never baselined —
	// they end the wait regardless of when they happened.
	baseReviews, err := fc.Reviews(ctx, repo, pr.Number)
	var baseComments []forge.ReviewComment
	if err == nil {
		baseComments, err = fc.ReviewComments(ctx, repo, pr.Number)
	}
	if err != nil {
		s.auditRemote("await_review", d, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	seen := make(map[int64]bool, len(baseComments))
	for _, c := range baseComments {
		seen[c.ID] = true
	}

	notify := mcpserve.ProgressNotifier(ctx, req)
	if notify != nil {
		notify(fmt.Sprintf("Waiting up to %v for review activity on PR #%d (%s). This request blocks until something happens.",
			maxWait, pr.Number, pr.URL))
	}

	start := s.now()
	deadline := start.Add(maxWait)
	failures := 0
	var lastErr error // the most recent failed poll, pending exoneration
	for {
		remaining := deadline.Sub(s.now())
		if remaining <= 0 {
			if failures > 0 {
				// The wait ran out while the endpoint was failing: "no
				// activity" was never verified for the final window, and
				// claiming a quiet timeout would be a lie under a success
				// status.
				s.auditRemote("await_review", d, audit.DecisionRemoteError)
				return mcpserve.ErrResult(fmt.Sprintf(
					"await_review: the wait expired with its last %d poll(s) failing (last: %v) — activity in the final window is unverified; call again to retry",
					failures, lastErr)), nil
			}
			// A quiet wait is a completed operation, not a failure: the
			// answer is "no activity yet", and the agent decides whether
			// to keep waiting.
			s.auditRemote("await_review", d, audit.DecisionRemoteOK)
			return mcpserve.JSONResult(map[string]any{
				"pr": pr.Number, "url": pr.URL, "state": pr.State,
				"outcome": "timeout", "waited": maxWait.String(),
				"note": "no review activity within maxWait — read_reviews shows the full current state; activity landing between calls joins the next baseline and will not retrigger",
			}), nil
		}
		switch err := s.sleep(ctx, min(awaitPollInterval, remaining)); {
		case errors.Is(err, errDraining):
			// Lame duck: the archivist is restarting.  Answer with the
			// same shape a quiet expiry uses — a completed wait that
			// found nothing yet — so the agent simply calls again once
			// the new process is up.  Returning promptly is what lets
			// the drain finish instead of holding it for the full
			// maxWait.
			s.auditRemote("await_review", d, audit.DecisionRemoteOK)
			return mcpserve.JSONResult(map[string]any{
				"pr": pr.Number, "url": pr.URL, "state": pr.State,
				"outcome": "interrupted", "waited": s.now().Sub(start).Round(time.Second).String(),
				"note": "the archivist is restarting — no review activity seen yet; call again",
			}), nil
		case err != nil:
			// The caller hung up mid-wait.  The polls already made still
			// touched the endpoint, so the abnormal end leaves a record.
			s.auditRemote("await_review", d, audit.DecisionRemoteError)
			return nil, err
		}

		cur, err := fc.GetPR(ctx, repo, pr.Number)
		var reviews []forge.Review
		var comments []forge.ReviewComment
		if err == nil {
			reviews, err = fc.Reviews(ctx, repo, pr.Number)
		}
		if err == nil {
			comments, err = fc.ReviewComments(ctx, repo, pr.Number)
		}
		if err != nil {
			failures++
			lastErr = err
			if failures >= awaitMaxPollErrors {
				s.auditRemote("await_review", d, audit.DecisionRemoteError)
				return mcpserve.ErrResult(fmt.Sprintf(
					"await_review: %d consecutive polls failed (last: %v) — the endpoint may be unreachable; call again to retry",
					failures, err)), nil
			}
			log.Printf("archivist: await_review poll failed (%d/%d): %v", failures, awaitMaxPollErrors, err)
			continue
		}
		failures = 0

		var newReviews []forge.Review
		if len(reviews) > len(baseReviews) {
			newReviews = reviews[len(baseReviews):]
		}
		var newComments []forge.ReviewComment
		for _, c := range comments {
			if !seen[c.ID] {
				newComments = append(newComments, c)
			}
		}
		if outcome := awaitOutcome(cur.State, newReviews, newComments); outcome != "" {
			s.auditRemote("await_review", d, audit.DecisionRemoteOK)
			return mcpserve.JSONResult(map[string]any{
				"pr": cur.Number, "url": cur.URL, "state": cur.State,
				"outcome":      outcome,
				"new_reviews":  reviewsOut(newReviews),
				"new_comments": commentsOut(newComments),
				"waited":       s.now().Sub(start).Round(time.Second).String(),
			}), nil
		}
		if notify != nil {
			notify(fmt.Sprintf("Still waiting for review activity on PR #%d — %v elapsed.",
				pr.Number, s.now().Sub(start).Round(time.Second)))
		}
	}
}

// awaitTarget resolves await_review's endpoint, repository, PR, and forge
// client under the serialization lock — the only part of the wait that
// touches the grange or the working tree.  Resolution failures audit the
// way prTarget's do; a non-nil errRes is the caller's early return.
func (s *Server) awaitTarget(ctx context.Context) (ep endpoint.Endpoint, repo string, pr forge.PR, fc forge.Client, errRes *mcp.CallToolResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	arc, err := s.cfg.Grange.Archive()
	if err != nil {
		return ep, "", pr, nil, mcpserve.ErrResult(err.Error())
	}
	fc, err = s.cfg.Grange.Forge()
	if err != nil {
		return ep, "", pr, nil, mcpserve.ErrResult(err.Error())
	}
	ep, repo, err = arc.RemoteInfo(ctx)
	if err != nil {
		s.auditRemote("await_review", audit.RemoteDetail{}, remoteDecision(err))
		return ep, "", pr, nil, mcpserve.ErrResult(err.Error())
	}
	// The current branch's open PR, resolved inline rather than through
	// resolvePR: this verb takes no pr argument (it waits on the agent's
	// own authorship loop), so resolvePR's "pass pr explicitly" advice
	// would send the caller into a wall.  These refusals name the next
	// step that actually exists here.
	st, err := arc.CurrentState(ctx)
	if err != nil {
		s.auditRemote("await_review", audit.RemoteDetail{Endpoint: ep.Name}, remoteDecision(err))
		return ep, "", pr, nil, mcpserve.ErrResult(err.Error())
	}
	if st.Branch == "" || st.Branch == st.Default {
		err := fmt.Errorf("archivist: await_review: %q is not a line of work — start_work, publish, and propose first", st.Branch)
		s.auditRemote("await_review", audit.RemoteDetail{Endpoint: ep.Name}, audit.DecisionRemoteRefused)
		return ep, "", pr, nil, mcpserve.ErrResult(err.Error())
	}
	pr, found, err := fc.FindPR(ctx, repo, st.Branch)
	if err != nil {
		s.auditRemote("await_review", audit.RemoteDetail{Endpoint: ep.Name, Branch: st.Branch}, remoteDecision(err))
		return ep, "", pr, nil, mcpserve.ErrResult(err.Error())
	}
	if !found {
		err := fmt.Errorf("archivist: await_review: %s has no open PR — publish and propose first", st.Branch)
		s.auditRemote("await_review", audit.RemoteDetail{Endpoint: ep.Name, Branch: st.Branch}, audit.DecisionRemoteRefused)
		return ep, "", pr, nil, mcpserve.ErrResult(err.Error())
	}
	return ep, repo, pr, fc, nil
}

// awaitOutcome names what ended the wait, or "" while nothing has: a
// terminal PR state wins; otherwise the latest verdict among the new
// reviews (reviews arrive chronological, and a later approval supersedes
// an earlier changes-requested); otherwise bare commentary.
func awaitOutcome(state string, reviews []forge.Review, comments []forge.ReviewComment) string {
	switch state {
	case forge.StateMerged:
		return "merged"
	case forge.StateClosed:
		return "closed"
	}
	verdict := ""
	for _, r := range reviews {
		switch r.State {
		case forge.ReviewChangesRequested:
			verdict = "changes_requested"
		case forge.ReviewApproved:
			verdict = "approved"
		}
	}
	if verdict != "" {
		return verdict
	}
	if len(reviews) > 0 || len(comments) > 0 {
		return "commented"
	}
	return ""
}

// now is the injected clock behind the wait's deadline arithmetic.
func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// errDraining ends a wait because the process is shutting down — not a
// failure of the operation, and not the caller hanging up.
var errDraining = errors.New("archivist: draining")

// sleep waits d out, or ends early when the caller hangs up (its error)
// or the process begins draining (errDraining), whichever comes first.
func (s *Server) sleep(ctx context.Context, d time.Duration) error {
	if s.cfg.Sleep != nil {
		return s.cfg.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.cfg.Draining:
		return errDraining
	case <-t.C:
		return nil
	}
}
