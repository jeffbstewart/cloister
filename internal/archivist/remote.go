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
	// publish needs only the engine (git push); it registers always and
	// refuses cleanly in local-only mode.
	s.add(&mcp.Tool{
		Name: "publish",
		Description: "Push the current line of work to its endpoint and record the upstream, flipping the branch to published " +
			"(after which restore and sync switch to forward motion).  Refused on the default branch.  Audited.",
		InputSchema: &jsonschema.Schema{Type: "object", AdditionalProperties: mcpserve.NoExtras()},
	}, s.publish)

	if s.cfg.Forge == nil {
		return
	}

	s.add(&mcp.Tool{
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

	s.add(&mcp.Tool{
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

	s.add(&mcp.Tool{
		Name: "read_reviews",
		Description: "A pull request's reviews, review-thread comments, and diff — the current branch's PR by default, or an " +
			"explicit number.  The diff is included for a caller that was not the PR's author.  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"pr": mcpserve.Integer("PR number; omit for the current branch's open PR"),
			},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.readReviews)

	s.add(&mcp.Tool{
		Name:        "reply_to_review",
		Description: "Respond on a review thread (thread = the id of any comment in it, from read_reviews).  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"pr":     mcpserve.Integer("the PR number the thread belongs to"),
				"thread": mcpserve.Integer("the review-comment id to reply to"),
				"body":   mcpserve.Str("the reply (markdown)"),
			},
			Required:             []string{"pr", "thread", "body"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.replyToReview)
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
	d.Op = op
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
	case errors.Is(err, archive.ErrDefaultBranch), errors.Is(err, archive.ErrNoEndpoints):
		return audit.DecisionRemoteRefused
	}
	return audit.DecisionRemoteError
}

func (s *Server) publish(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, err := s.cfg.Archive.Publish(ctx)
	s.auditRemote("publish", audit.RemoteDetail{Endpoint: info.Endpoint, Branch: info.Branch}, remoteDecision(err))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.JSONResult(map[string]any{"branch": info.Branch, "endpoint": info.Endpoint, "published": true}), nil
}

func (s *Server) propose(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	ep, repo, err := s.cfg.Archive.RemoteInfo(ctx)
	if err != nil {
		s.auditRemote("propose", audit.RemoteDetail{}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	st, err := s.cfg.Archive.CurrentState(ctx)
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	if st.Branch == "" || st.Branch == st.Default {
		err := fmt.Errorf("archive: propose: work is proposed from a line of work, not %q — start_work and publish first", st.Branch)
		s.auditRemote("propose", audit.RemoteDetail{Endpoint: ep.Name, Branch: st.Branch}, audit.DecisionRemoteRefused)
		return mcpserve.ErrResult(err.Error()), nil
	}

	d := audit.RemoteDetail{Endpoint: ep.Name, Branch: st.Branch}
	pr, found, err := s.cfg.Forge.FindPR(ctx, repo, st.Branch)
	if err == nil {
		if found {
			pr, err = s.cfg.Forge.UpdatePR(ctx, repo, pr.Number, a.Title, a.Body)
		} else {
			pr, err = s.cfg.Forge.CreatePR(ctx, repo, st.Branch, st.Default, a.Title, a.Body)
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
func (s *Server) resolvePR(ctx context.Context, repo string, number int) (forge.PR, error) {
	if number > 0 {
		return s.cfg.Forge.GetPR(ctx, repo, number)
	}
	st, err := s.cfg.Archive.CurrentState(ctx)
	if err != nil {
		return forge.PR{}, err
	}
	if st.Branch == "" || st.Branch == st.Default {
		return forge.PR{}, fmt.Errorf("archivist: no PR number given and %q has no line-of-work PR — pass pr explicitly", st.Branch)
	}
	pr, found, err := s.cfg.Forge.FindPR(ctx, repo, st.Branch)
	if err != nil {
		return forge.PR{}, err
	}
	if !found {
		return forge.PR{}, fmt.Errorf("archivist: branch %s has no open PR — propose first, or pass pr explicitly", st.Branch)
	}
	return pr, nil
}

func (s *Server) checkProgress(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		PR int `json:"pr"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	ep, repo, err := s.cfg.Archive.RemoteInfo(ctx)
	if err != nil {
		s.auditRemote("check_progress", audit.RemoteDetail{PR: a.PR}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	pr, err := s.resolvePR(ctx, repo, a.PR)
	if err != nil {
		s.auditRemote("check_progress", audit.RemoteDetail{Endpoint: ep.Name, PR: a.PR}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	checks, err := s.cfg.Forge.Checks(ctx, repo, pr.Sha)
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

func (s *Server) readReviews(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		PR int `json:"pr"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	ep, repo, err := s.cfg.Archive.RemoteInfo(ctx)
	if err != nil {
		s.auditRemote("read_reviews", audit.RemoteDetail{PR: a.PR}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	pr, err := s.resolvePR(ctx, repo, a.PR)
	if err != nil {
		s.auditRemote("read_reviews", audit.RemoteDetail{Endpoint: ep.Name, PR: a.PR}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	reviews, err := s.cfg.Forge.Reviews(ctx, repo, pr.Number)
	var comments []forge.ReviewComment
	if err == nil {
		comments, err = s.cfg.Forge.ReviewComments(ctx, repo, pr.Number)
	}
	diff := ""
	if err == nil {
		diff, err = s.cfg.Forge.Diff(ctx, repo, pr.Number)
	}
	s.auditRemote("read_reviews", audit.RemoteDetail{Endpoint: ep.Name, Branch: pr.Head, PR: pr.Number}, remoteDecision(err))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}

	outReviews := make([]map[string]any, 0, len(reviews))
	for _, r := range reviews {
		outReviews = append(outReviews, map[string]any{
			"author": r.Author, "state": r.State, "body": r.Body,
			"time": r.Time.UTC().Format(time.RFC3339),
		})
	}
	outComments := make([]map[string]any, 0, len(comments))
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
		outComments = append(outComments, m)
	}
	cappedDiff, truncated := capDiff(diff)
	out := map[string]any{
		"pr": pr.Number, "url": pr.URL, "state": pr.State,
		"reviews": outReviews, "comments": outComments, "diff": cappedDiff,
	}
	if truncated {
		out["diff_truncated"] = true
	}
	return mcpserve.JSONResult(out), nil
}

func (s *Server) replyToReview(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		PR     int    `json:"pr"`
		Thread int64  `json:"thread"`
		Body   string `json:"body"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	if a.PR <= 0 || a.Thread <= 0 {
		return mcpserve.ErrResult("bad arguments: pr and thread must be positive ids"), nil
	}
	if a.Body == "" || len(a.Body) > maxReplyBytes {
		return mcpserve.ErrResult("bad arguments: a reply body is required, at most " + strconv.Itoa(maxReplyBytes) + " bytes"), nil
	}
	ep, repo, err := s.cfg.Archive.RemoteInfo(ctx)
	if err != nil {
		s.auditRemote("reply_to_review", audit.RemoteDetail{PR: a.PR}, remoteDecision(err))
		return mcpserve.ErrResult(err.Error()), nil
	}
	c, err := s.cfg.Forge.Reply(ctx, repo, a.PR, a.Thread, a.Body)
	s.auditRemote("reply_to_review",
		audit.RemoteDetail{Endpoint: ep.Name, PR: a.PR, Target: strconv.FormatInt(a.Thread, 10)},
		remoteDecision(err))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.JSONResult(map[string]any{"id": c.ID, "in_reply_to": c.InReplyTo}), nil
}
