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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBodyBytes caps any response body read; the consumer is a model
// context, not a browser.
const maxBodyBytes = 4 << 20

// maxPages bounds pagination sweeps; a PR with more than a thousand
// review comments has outgrown this surface anyway.
const maxPages = 10

// GitHub speaks the GitHub REST API.  The token is read per call (the
// mounted credential file is the rotation point) and travels only in
// the Authorization header; the HTTP client is injected so production
// hands in one pinned to the api relay and tests hand in httptest's.
type GitHub struct {
	apiBase string // e.g. "https://api.github.com/", always "/"-terminated
	token   func() (string, error)
	client  *http.Client
}

// NewGitHub builds the GitHub backend.  client must be the caller's
// deliberately-constructed transport — there is no default here, so the
// relay pinning cannot be forgotten.
func NewGitHub(apiBase string, token func() (string, error), client *http.Client) (*GitHub, error) {
	if !strings.HasSuffix(apiBase, "/") {
		return nil, fmt.Errorf("forge: apiBase %q must end in \"/\"", apiBase)
	}
	if token == nil || client == nil {
		return nil, fmt.Errorf("forge: a token source and an http client are required")
	}
	return &GitHub{apiBase: apiBase, token: token, client: client}, nil
}

// do issues one API call.  Any 2xx is success; the error text for a
// failure carries a clipped body and NEVER the request (whose header
// holds the credential).
func (g *GitHub) do(ctx context.Context, method, path, accept string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("forge: marshal %s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiBase+strings.TrimPrefix(path, "/"), reqBody)
	if err != nil {
		return fmt.Errorf("forge: %s %s: %w", method, path, err)
	}
	tok, err := g.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("forge: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("forge: %s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("forge: %s %s: %s: %s", method, path, resp.Status, clip(raw, 200))
	}
	switch v := out.(type) {
	case nil:
	case *[]byte:
		*v = raw
	default:
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("forge: %s %s: unparseable response: %w", method, path, err)
		}
	}
	return nil
}

// paged issues GET calls across ?page=1..maxPages, appending each
// page's items via collect, stopping at the first short page.
func (g *GitHub) paged(ctx context.Context, path string, collect func(json.RawMessage) (int, error)) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		var raw json.RawMessage
		p := fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page)
		if err := g.do(ctx, http.MethodGet, p, "", nil, &raw); err != nil {
			return err
		}
		n, err := collect(raw)
		if err != nil {
			return err
		}
		if n < 100 {
			return nil
		}
	}
	return nil
}

func clip(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ghPR is the wire shape the PR methods read.
type ghPR struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
		Sha string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p ghPR) pr() PR {
	state := p.State
	if p.Merged {
		state = "merged"
	}
	return PR{Number: p.Number, Title: p.Title, State: state, URL: p.HTMLURL,
		Head: p.Head.Ref, Base: p.Base.Ref, Sha: p.Head.Sha}
}

func (g *GitHub) CreatePR(ctx context.Context, repo, head, base, title, body string) (PR, error) {
	var out ghPR
	err := g.do(ctx, http.MethodPost, "repos/"+repo+"/pulls", "", map[string]string{
		"head": head, "base": base, "title": title, "body": body,
	}, &out)
	if err != nil {
		return PR{}, err
	}
	return out.pr(), nil
}

func (g *GitHub) FindPR(ctx context.Context, repo, head string) (PR, bool, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var out []ghPR
	err := g.do(ctx, http.MethodGet,
		"repos/"+repo+"/pulls?state=open&head="+owner+":"+head, "", nil, &out)
	if err != nil {
		return PR{}, false, err
	}
	if len(out) == 0 {
		return PR{}, false, nil
	}
	return out[0].pr(), true, nil
}

func (g *GitHub) UpdatePR(ctx context.Context, repo string, number int, title, body string) (PR, error) {
	var out ghPR
	err := g.do(ctx, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", repo, number), "",
		map[string]string{"title": title, "body": body}, &out)
	if err != nil {
		return PR{}, err
	}
	return out.pr(), nil
}

func (g *GitHub) GetPR(ctx context.Context, repo string, number int) (PR, error) {
	var out ghPR
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", repo, number), "", nil, &out); err != nil {
		return PR{}, err
	}
	return out.pr(), nil
}

func (g *GitHub) Checks(ctx context.Context, repo, sha string) ([]Check, error) {
	var checks []Check
	err := g.paged(ctx, "repos/"+repo+"/commits/"+sha+"/check-runs", func(raw json.RawMessage) (int, error) {
		var page struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, fmt.Errorf("forge: unparseable check-runs page: %w", err)
		}
		for _, c := range page.CheckRuns {
			checks = append(checks, Check{Name: c.Name, Status: c.Status, Conclusion: c.Conclusion})
		}
		return len(page.CheckRuns), nil
	})
	return checks, err
}

func (g *GitHub) Reviews(ctx context.Context, repo string, number int) ([]Review, error) {
	var reviews []Review
	err := g.paged(ctx, fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number), func(raw json.RawMessage) (int, error) {
		var page []struct {
			User        struct{ Login string }
			State       string    `json:"state"`
			Body        string    `json:"body"`
			SubmittedAt time.Time `json:"submitted_at"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, fmt.Errorf("forge: unparseable reviews page: %w", err)
		}
		for _, r := range page {
			reviews = append(reviews, Review{Author: r.User.Login, State: r.State, Body: r.Body, Time: r.SubmittedAt})
		}
		return len(page), nil
	})
	return reviews, err
}

func (g *GitHub) ReviewComments(ctx context.Context, repo string, number int) ([]ReviewComment, error) {
	var comments []ReviewComment
	err := g.paged(ctx, fmt.Sprintf("repos/%s/pulls/%d/comments", repo, number), func(raw json.RawMessage) (int, error) {
		var page []ghReviewComment
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, fmt.Errorf("forge: unparseable comments page: %w", err)
		}
		for _, c := range page {
			comments = append(comments, c.comment())
		}
		return len(page), nil
	})
	return comments, err
}

type ghReviewComment struct {
	ID        int64 `json:"id"`
	InReplyTo int64 `json:"in_reply_to_id"`
	User      struct{ Login string }
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func (c ghReviewComment) comment() ReviewComment {
	return ReviewComment{ID: c.ID, InReplyTo: c.InReplyTo, Author: c.User.Login,
		Path: c.Path, Line: c.Line, Body: c.Body, Time: c.CreatedAt}
}

func (g *GitHub) Diff(ctx context.Context, repo string, number int) (string, error) {
	var raw []byte
	err := g.do(ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", repo, number),
		"application/vnd.github.diff", nil, &raw)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (g *GitHub) Reply(ctx context.Context, repo string, number int, commentID int64, body string) (ReviewComment, error) {
	var out ghReviewComment
	err := g.do(ctx, http.MethodPost,
		fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", repo, number, commentID), "",
		map[string]string{"body": body}, &out)
	if err != nil {
		return ReviewComment{}, err
	}
	return out.comment(), nil
}
