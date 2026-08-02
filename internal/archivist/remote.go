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
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forge"
	"github.com/jeffbstewart/cloister/internal/mcpserve"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// The remote verbs: audited, ungated (docs/archivist.md).  Every
// endpoint touch leaves a record; none waits for approval.  The
// reviewing human is the boundary — these verbs only carry work to
// where review happens.

// maxReplyBytes bounds a review reply; GitHub's own cap is 64 KiB.
const maxReplyBytes = 60 << 10

func (s *Server) registerRemoteTools() {
	// publish needs only the engine (git push); the PR verbs also need the
	// endpoint's forge client, which is why they register with addForge.
	// All refuse cleanly until a workspace is provisioned.
	s.addArc(&mcp.Tool{
		Name: "publish",
		Description: "Push the line of work the archivist is ALREADY on — start_work creates one; this does not.  Records the " +
			"upstream and flips the branch to published (after which restore and sync switch to forward motion).  Refused on " +
			"the default branch, and on any branch outside the repository's agent namespace.  Audited.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: mcpserve.NoExtras()},
	}, s.publish)

	s.addForge(&mcp.Tool{
		Name: "propose",
		Description: "Open the pull request for the current published branch — or update its title and body when one is already open.  " +
			"publish first; the PR's base is the default branch.  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"title": mcpserve.Str("the PR title"),
				"body":  mcpserve.Str("the PR description (markdown)"),
			},
			Required:             []string{"title", "body"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.propose)

	s.addForge(&mcp.Tool{
		Name: "check_progress",
		Description: "A pull request's state and CI check results — the current branch's PR by default, or an explicit number " +
			"(the corrector reviews PRs it did not author).  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"pr": mcpserve.Integer("PR number; omit for the current branch's open PR"),
			},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.checkProgress)

	s.addForge(&mcp.Tool{
		Name: "read_reviews",
		Description: "A pull request's reviews, review-thread comments, and full diff — the current branch's PR by default, or an " +
			"explicit number (the corrector reviews PRs it did not author).  The diff is always included; diff_truncated marks a capped one.  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"pr": mcpserve.Integer("PR number; omit for the current branch's open PR"),
			},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.readReviews)

	s.addForge(&mcp.Tool{
		Name: "reply_to_review",
		Description: "Respond on a review thread — thread is the id of ANY comment in it (from read_reviews); the reply is " +
			"attached to the thread's root automatically.  pr defaults to the current branch's open PR.  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"pr":     mcpserve.Integer("PR number; omit for the current branch's open PR"),
				"thread": mcpserve.Integer("a review-comment id from read_reviews (any comment in the thread)"),
				"body":   mcpserve.Str("the reply (markdown)"),
			},
			Required:             []string{"thread", "body"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.replyToReview)

	s.registerAwaitReview()
}

// auditRemote appends one remote-op record.  Append failure is logged,
// never fatal — the operation itself already happened or refused.
func (s *Server) auditRemote(op string, d audit.RemoteDetail, decision audit.Decision) {
	if s.cfg.Audit == nil {
		return
	}
	id, err := runid.New()
	if err != nil {
		log.Printf("archivist: mint remote op id: %v", err)
		return
	}
	rec := audit.New(id, op, decision, 0)
	rec.Detail = &d
	if err := s.cfg.Audit.Append(rec); err != nil {
		log.Printf("archivist: audit append failed: %v", err)
	}
}

// remoteDecision folds an error into the audit decision vocabulary:
// the archivist's own refusals are DecisionRemoteRefused, everything
// else that failed is DecisionRemoteError.
func remoteDecision(err error) audit.Decision {
	switch {
	case err == nil:
		return audit.DecisionRemoteOK
	case errors.Is(err, archive.ErrDefaultBranch), errors.Is(err, archive.ErrNoEndpoints),
		errors.Is(err, archive.ErrOutsideNamespace):
		return audit.DecisionRemoteRefused
	}
	return audit.DecisionRemoteError
}

func (s *Server) publish(ctx context.Context, _ *mcp.CallToolRequest, arc *archive.Archive) (*mcp.CallToolResult, error) {
	info, err := arc.Publish(ctx)
	s.auditRemote("publish", audit.RemoteDetail{Endpoint: info.Endpoint, Branch: info.Branch}, remoteDecision(err))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.JSONResult(map[string]any{"branch": info.Branch, "endpoint": info.Endpoint, "published": true}), nil
}

func (s *Server) propose(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive, fc forge.Client) (*mcp.CallToolResult, error) {
	var a struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	if a.Title == "" {
		return mcpserve.ErrResult("bad arguments: a title is required"), nil
	}
	ep, repo, err := arc.RemoteInfo(ctx)
	if err != nil {
		s.auditRemote("propose", audit.RemoteDetail{}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	st, err := arc.CurrentState(ctx)
	if err != nil {
		s.auditRemote("propose", audit.RemoteDetail{Endpoint: ep.Name}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	if st.Branch == "" || st.Branch == st.Default {
		err := fmt.Errorf("archivist: propose: work is proposed from a line of work, not %q — start_work first", st.Branch)
		s.auditRemote("propose", audit.RemoteDetail{Endpoint: ep.Name, Branch: st.Branch}, audit.DecisionRemoteRefused)
		return mcpserve.ErrResult(err.Error()), nil
	}
	if !st.Published {
		// The PR's head must exist on the remote — publishing is what puts
		// it there.  Catching it here turns the forge's opaque 422 into an
		// actionable refusal that names the next step.
		err := fmt.Errorf("archivist: propose: %s is not published — publish first, then propose", st.Branch)
		s.auditRemote("propose", audit.RemoteDetail{Endpoint: ep.Name, Branch: st.Branch}, audit.DecisionRemoteRefused)
		return mcpserve.ErrResult(err.Error()), nil
	}

	d := audit.RemoteDetail{Endpoint: ep.Name, Branch: st.Branch}
	pr, found, err := fc.FindPR(ctx, repo, st.Branch)
	if err == nil {
		if found {
			pr, err = fc.UpdatePR(ctx, repo, pr.Number, a.Title, a.Body)
		} else {
			pr, err = fc.CreatePR(ctx, repo, st.Branch, st.Default, a.Title, a.Body)
		}
	}
	d.PR = pr.Number
	s.auditRemote("propose", d, remoteDecision(err))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.JSONResult(map[string]any{
		"pr": pr.Number, "url": pr.URL, "state": pr.State, "updated": found,
	}), nil
}

// resolvePR turns an optional explicit number into a concrete PR: the
// number as given, or the current branch's open PR.
func (s *Server) resolvePR(ctx context.Context, arc *archive.Archive, fc forge.Client, repo string, number int) (forge.PR, error) {
	if number > 0 {
		return fc.GetPR(ctx, repo, number)
	}
	st, err := arc.CurrentState(ctx)
	if err != nil {
		return forge.PR{}, err
	}
	if st.Branch == "" || st.Branch == st.Default {
		return forge.PR{}, fmt.Errorf("archivist: no PR number given and %q has no line-of-work PR — pass pr explicitly", st.Branch)
	}
	pr, found, err := fc.FindPR(ctx, repo, st.Branch)
	if err != nil {
		return forge.PR{}, err
	}
	if !found {
		return forge.PR{}, fmt.Errorf("archivist: branch %s has no open PR — propose first, or pass pr explicitly", st.Branch)
	}
	return pr, nil
}

// prTarget decodes the optional {pr} argument and resolves the PR a
// read verb acts on, auditing its own failures under op.  On success it
// returns the endpoint, repo, PR, and a nil result; on failure the
// result is the caller's early return.
func (s *Server) prTarget(ctx context.Context, op string, req *mcp.CallToolRequest, arc *archive.Archive, fc forge.Client) (endpoint.Endpoint, string, forge.PR, *mcp.CallToolResult) {
	var a struct {
		PR int `json:"pr"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return endpoint.Endpoint{}, "", forge.PR{}, mcpserve.ErrResult("bad arguments: " + err.Error())
	}
	ep, repo, err := arc.RemoteInfo(ctx)
	if err != nil {
		s.auditRemote(op, audit.RemoteDetail{PR: a.PR}, remoteDecision(err))
		return endpoint.Endpoint{}, "", forge.PR{}, mcpserve.ErrResult(err.Error())
	}
	pr, err := s.resolvePR(ctx, arc, fc, repo, a.PR)
	if err != nil {
		s.auditRemote(op, audit.RemoteDetail{Endpoint: ep.Name, PR: a.PR}, remoteDecision(err))
		return endpoint.Endpoint{}, "", forge.PR{}, mcpserve.ErrResult(err.Error())
	}
	return ep, repo, pr, nil
}

func (s *Server) checkProgress(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive, fc forge.Client) (*mcp.CallToolResult, error) {
	ep, repo, pr, errRes := s.prTarget(ctx, "check_progress", req, arc, fc)
	if errRes != nil {
		return errRes, nil
	}
	checks, err := fc.Checks(ctx, repo, pr.SHA)
	s.auditRemote("check_progress", audit.RemoteDetail{Endpoint: ep.Name, Branch: pr.Head, PR: pr.Number}, remoteDecision(err))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	outChecks := make([]map[string]any, 0, len(checks))
	for _, c := range checks {
		outChecks = append(outChecks, map[string]any{"name": c.Name, "status": c.Status, "conclusion": c.Conclusion})
	}
	return mcpserve.JSONResult(map[string]any{
		"pr": pr.Number, "url": pr.URL, "title": pr.Title, "state": pr.State,
		"branch": pr.Head, "checks": outChecks,
	}), nil
}

func (s *Server) readReviews(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive, fc forge.Client) (*mcp.CallToolResult, error) {
	ep, repo, pr, errRes := s.prTarget(ctx, "read_reviews", req, arc, fc)
	if errRes != nil {
		return errRes, nil
	}
	reviews, err := fc.Reviews(ctx, repo, pr.Number)
	var comments []forge.ReviewComment
	if err == nil {
		comments, err = fc.ReviewComments(ctx, repo, pr.Number)
	}
	diff := ""
	if err == nil {
		diff, err = fc.Diff(ctx, repo, pr.Number)
	}
	s.auditRemote("read_reviews", audit.RemoteDetail{Endpoint: ep.Name, Branch: pr.Head, PR: pr.Number}, remoteDecision(err))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}

	cappedDiff, truncated := capDiff(diff)
	out := map[string]any{
		"pr": pr.Number, "url": pr.URL, "state": pr.State,
		"reviews": reviewsOut(reviews), "comments": commentsOut(comments), "diff": cappedDiff,
	}
	if truncated {
		out["diff_truncated"] = true
	}
	return mcpserve.JSONResult(out), nil
}

func (s *Server) replyToReview(ctx context.Context, req *mcp.CallToolRequest, arc *archive.Archive, fc forge.Client) (*mcp.CallToolResult, error) {
	var a struct {
		PR     int    `json:"pr"`
		Thread int64  `json:"thread"`
		Body   string `json:"body"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	if a.Thread <= 0 {
		return mcpserve.ErrResult("bad arguments: thread must be a review-comment id from read_reviews"), nil
	}
	if a.Body == "" || len(a.Body) > maxReplyBytes {
		return mcpserve.ErrResult("bad arguments: a reply body is required, at most " + strconv.Itoa(maxReplyBytes) + " bytes"), nil
	}
	ep, repo, err := arc.RemoteInfo(ctx)
	if err != nil {
		s.auditRemote("reply_to_review", audit.RemoteDetail{PR: a.PR}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	pr, err := s.resolvePR(ctx, arc, fc, repo, a.PR)
	if err != nil {
		s.auditRemote("reply_to_review", audit.RemoteDetail{Endpoint: ep.Name, PR: a.PR}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	// The forge accepts a reply only against a thread's ROOT comment;
	// read_reviews surfaces every comment (replies included), so an agent
	// naturally holds a reply's id.  Resolve it to the root so the verb's
	// description ("any comment in the thread") is true.
	root, err := s.threadRoot(ctx, fc, repo, pr.Number, a.Thread)
	if err != nil {
		s.auditRemote("reply_to_review", audit.RemoteDetail{Endpoint: ep.Name, PR: pr.Number}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	c, err := fc.Reply(ctx, repo, pr.Number, root, a.Body)
	s.auditRemote("reply_to_review",
		audit.RemoteDetail{Endpoint: ep.Name, PR: pr.Number, Target: strconv.FormatInt(root, 10)},
		remoteDecision(err))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.JSONResult(map[string]any{"id": c.ID, "in_reply_to": c.InReplyTo}), nil
}

// reviewsOut renders submitted reviews for a tool answer.
func reviewsOut(reviews []forge.Review) []map[string]any {
	out := make([]map[string]any, 0, len(reviews))
	for _, r := range reviews {
		out = append(out, map[string]any{
			"author": r.Author, "state": r.State, "body": r.Body,
			"time": r.Time.UTC().Format(time.RFC3339),
		})
	}
	return out
}

// commentsOut renders review-thread comments for a tool answer.
func commentsOut(comments []forge.ReviewComment) []map[string]any {
	out := make([]map[string]any, 0, len(comments))
	for _, c := range comments {
		m := map[string]any{
			"id": c.ID, "author": c.Author, "body": c.Body,
			"time": c.Time.UTC().Format(time.RFC3339),
		}
		if c.InReplyTo != 0 {
			m["in_reply_to"] = c.InReplyTo
		}
		if c.Path != "" {
			m["path"] = c.Path
			m["line"] = c.Line
		}
		out = append(out, m)
	}
	return out
}

// threadRoot walks a comment id up its in-reply-to chain to the thread's
// root — the only id the forge's reply endpoint accepts.  An id that is
// already a root (or is not found, e.g. a stale id) is returned as-is,
// so the forge gives the authoritative error rather than this guessing.
func (s *Server) threadRoot(ctx context.Context, fc forge.Client, repo string, number int, id int64) (int64, error) {
	comments, err := fc.ReviewComments(ctx, repo, number)
	if err != nil {
		return 0, err
	}
	parent := make(map[int64]int64, len(comments))
	for _, c := range comments {
		parent[c.ID] = c.InReplyTo
	}
	for {
		up, ok := parent[id]
		if !ok || up == 0 {
			return id, nil
		}
		id = up
	}
}
