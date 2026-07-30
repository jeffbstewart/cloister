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

// Package forge is the archivist's PR-authoring seam: the review-flow
// operations behind propose, check_progress, read_reviews, and
// reply_to_review, forge-agnostically.  It is the writing counterpart
// of internal/forgelint's read-only Snapshot — a deliberate second
// interface, because snapshotting protection facts and authoring pull
// requests share a transport but nothing else.
//
// GitHub is M1's only backend; the interface is shaped so the Gitea
// adapter slots in behind the same verbs.
package forge

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Canonical, forge-agnostic vocabulary the verbs report.  GitHub emits
// these strings natively; a future adapter (Gitea) maps its own into
// them, so an agent sees one vocabulary regardless of backend.
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateMerged = "merged"

	CheckQueued     = "queued"
	CheckInProgress = "in_progress"
	CheckCompleted  = "completed"

	ReviewApproved         = "APPROVED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
	ReviewCommented        = "COMMENTED"
)

// PR is one pull request as the verbs report it.
type PR struct {
	Number int
	Title  string
	State  string // StateOpen | StateClosed | StateMerged
	URL    string // the human-facing page
	Head   string // branch name
	Base   string
	SHA    string // head commit
}

// Check is one CI check run on a PR's head.
type Check struct {
	Name       string
	Status     string // CheckQueued | CheckInProgress | CheckCompleted
	Conclusion string // "success", "failure", ... ("" until completed)
}

// ReviewComment is one comment on a PR review thread.
type ReviewComment struct {
	ID        int64
	InReplyTo int64 // 0 for a thread root
	Author    string
	Path      string // file the thread anchors to ("" for a PR-level comment)
	Line      int
	Body      string
	Time      time.Time
}

// Review is one submitted review (the approve/request-changes verdicts
// that gate a merge).
type Review struct {
	Author string
	State  string // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", ...
	Body   string
	Time   time.Time
}

// Client is the PR-authoring surface one forge backend provides.  Every
// method takes the repository as "owner/name".
type Client interface {
	// CreatePR opens a pull request from head onto base.
	CreatePR(ctx context.Context, repo, head, base, title, body string) (PR, error)
	// FindPR resolves the open PR whose head is the named branch, if any.
	FindPR(ctx context.Context, repo, head string) (PR, bool, error)
	// UpdatePR retitles/rebodies an existing PR.
	UpdatePR(ctx context.Context, repo string, number int, title, body string) (PR, error)
	// GetPR fetches one PR by number.
	GetPR(ctx context.Context, repo string, number int) (PR, error)
	// Checks lists the check runs for a commit.
	Checks(ctx context.Context, repo, sha string) ([]Check, error)
	// Reviews lists a PR's submitted reviews.
	Reviews(ctx context.Context, repo string, number int) ([]Review, error)
	// ReviewComments lists a PR's review-thread comments.
	ReviewComments(ctx context.Context, repo string, number int) ([]ReviewComment, error)
	// Diff fetches a PR's unified diff.
	Diff(ctx context.Context, repo string, number int) (string, error)
	// Reply answers on the review thread rooted at commentID.
	Reply(ctx context.Context, repo string, number int, commentID int64, body string) (ReviewComment, error)
}

// RepoFromRemote derives "owner/name" from a remote URL and its
// endpoint's canonical prefix — the archivist's repositories are
// always designated canonically, so this is string arithmetic, not URL
// parsing.
func RepoFromRemote(canonical, remoteURL string) (string, error) {
	rest, ok := strings.CutPrefix(remoteURL, canonical)
	if !ok {
		return "", fmt.Errorf("forge: remote %q is not under %q", remoteURL, canonical)
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || !repoSegmentOK(owner) || !repoSegmentOK(name) {
		return "", fmt.Errorf("forge: remote %q does not name an owner/name repository", remoteURL)
	}
	return owner + "/" + name, nil
}

// repoSegmentOK vets one path segment of a derived repository: the
// GitHub/Gitea alphabet, and never "."/".." — so a crafted origin
// (github.com/../x) cannot smuggle path traversal into an API path.
func repoSegmentOK(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
