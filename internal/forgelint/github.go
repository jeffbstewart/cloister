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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// GitHub reads a repository's rulesets, CODEOWNERS, collaborator roles,
// and Actions secrets, and normalizes them into a Snapshot
// (docs/grange.md, "GitHub realization").
type GitHub struct {
	cfg    *Config
	token  string // empty = unauthenticated (public-repo rulesets still read)
	client *http.Client
}

// NewGitHub builds the GitHub backend.  A nil client uses
// http.DefaultClient.
func NewGitHub(cfg *Config, token string, client *http.Client) *GitHub {
	if client == nil {
		client = http.DefaultClient
	}
	return &GitHub{cfg: cfg, token: token, client: client}
}

// get fetches path (relative to APIBase) into out.  A 403/404 is reported
// as errForbidden so callers can mark the section UNVERIFIED instead of
// failing the run.
var errForbidden = fmt.Errorf("forge credential cannot read this section")

func (g *GitHub) get(ctx context.Context, path, accept string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.APIBase+path, nil)
	if err != nil {
		return err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w (GET %s: %d)", errForbidden, path, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("GET %s: %d: %s", path, resp.StatusCode, clipBody(body))
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = body
		return nil
	}
	return json.Unmarshal(body, out)
}

func clipBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// GitHub API shapes (the fields the lint reads; everything else ignored).
type ghRulesetRef struct {
	ID          int64  `json:"id"`
	Target      string `json:"target"`
	Enforcement string `json:"enforcement"`
}

type ghRuleset struct {
	Enforcement string `json:"enforcement"`
	Conditions  struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules        []ghRule `json:"rules"`
	BypassActors []struct {
		ActorID    int64  `json:"actor_id"`
		ActorType  string `json:"actor_type"`
		BypassMode string `json:"bypass_mode"`
	} `json:"bypass_actors"`
}

// ghRule is one rule as either the rulesets API (nested in a ghRuleset) or
// the effective-rules API (a flat array) reports it — the element shape is
// identical, so one decoder (applyBranchRules) serves the operator lint and
// the bot-credential provision gate alike.
type ghRule struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

// ghAdminRoleID is the RepositoryRole actor id GitHub assigns the
// repository-admin role in ruleset bypass lists.
const ghAdminRoleID = 5

// Snapshot reads the protection state.  Sections the credential cannot
// read surface as *Known=false, not errors; only a repo or ruleset-list
// read failure aborts, since with no rules visible there is nothing to
// assert against.
func (g *GitHub) Snapshot(ctx context.Context) (*Snapshot, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	repoPath := "/repos/" + g.cfg.Repo
	if err := g.get(ctx, repoPath, "", &repo); err != nil {
		return nil, fmt.Errorf("read repository: %w", err)
	}
	s := &Snapshot{DefaultBranch: repo.DefaultBranch}

	var refs []ghRulesetRef
	if err := g.get(ctx, repoPath+"/rulesets", "", &refs); err != nil {
		return nil, fmt.Errorf("list rulesets: %w", err)
	}
	defaultRef := "refs/heads/" + g.cfg.DefaultBranch
	nsRef := "refs/heads/" + g.cfg.AgentNamespace + "**"
	s.NamespaceDetail = fmt.Sprintf("no active ruleset restricts branch creation/updates outside %q (want: include ~ALL, exclude %s and %s, admin-only bypass)", g.cfg.AgentNamespace, nsRef, defaultRef)
	for _, ref := range refs {
		if ref.Target != "branch" || ref.Enforcement != "active" {
			continue
		}
		var rs ghRuleset
		if err := g.get(ctx, fmt.Sprintf("%s/rulesets/%d", repoPath, ref.ID), "", &rs); err != nil {
			return nil, fmt.Errorf("read ruleset %d: %w", ref.ID, err)
		}
		include := rs.Conditions.RefName.Include
		exclude := rs.Conditions.RefName.Exclude
		switch {
		case contains(include, "~DEFAULT_BRANCH") || contains(include, defaultRef):
			g.applyMainRuleset(s, &rs)
		case contains(include, "~ALL") && contains(exclude, nsRef):
			g.applyNamespaceRuleset(s, &rs, exclude, defaultRef)
		}
	}
	if !s.NamespaceKnown {
		// The ruleset list itself was readable, so an absent namespace
		// ruleset is a definite fact — a violation, not an unknown.
		s.NamespaceKnown = true
		s.NamespaceConfined = false
	}

	g.applyCodeOwners(ctx, s)

	var perm struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	if err := g.get(ctx, repoPath+"/collaborators/"+g.cfg.Bot+"/permission", "", &perm); err == nil {
		s.BotPermissionKnown = true
		s.BotPermission = perm.RoleName
		if s.BotPermission == "" {
			s.BotPermission = perm.Permission
		}
	}

	var secrets struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if err := g.get(ctx, repoPath+"/actions/secrets", "", &secrets); err == nil {
		s.SecretsKnown = true
		for _, sec := range secrets.Secrets {
			s.SecretNames = append(s.SecretNames, sec.Name)
		}
	}
	return s, nil
}

// applyBranchRules folds a branch's effective rules — the pull-request,
// status-check, force-push, and deletion protections — into the snapshot.
// It is deliberately bypass-agnostic: the rulesets API carries the bypass
// roster (applyMainRuleset adds it) while the effective-rules API does not
// (the provision gate leaves BypassKnown false).
func applyBranchRules(s *Snapshot, rules []ghRule) {
	for _, rule := range rules {
		switch rule.Type {
		case "pull_request":
			var p struct {
				RequiredApprovingReviewCount int  `json:"required_approving_review_count"`
				DismissStaleReviewsOnPush    bool `json:"dismiss_stale_reviews_on_push"`
				RequireCodeOwnerReview       bool `json:"require_code_owner_review"`
			}
			if json.Unmarshal(rule.Parameters, &p) == nil {
				s.PRRequired = true
				s.RequiredApprovals = p.RequiredApprovingReviewCount
				s.DismissStale = p.DismissStaleReviewsOnPush
				// OwnerReviewOK is completed by applyCodeOwners: the rule
				// is worthless if CODEOWNERS names anyone but the operator.
				s.OwnerReviewOK = p.RequireCodeOwnerReview
			}
		case "required_status_checks":
			var p struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			}
			if json.Unmarshal(rule.Parameters, &p) == nil {
				for _, c := range p.RequiredStatusChecks {
					s.RequiredChecks = append(s.RequiredChecks, c.Context)
				}
			}
		case "non_fast_forward":
			s.ForcePushBlocked = true
		case "deletion":
			s.DeletionBlocked = true
		}
	}
}

// applyMainRuleset folds the default branch's ruleset into the snapshot.
func (g *GitHub) applyMainRuleset(s *Snapshot, rs *ghRuleset) {
	applyBranchRules(s, rs.Rules)
	s.BypassKnown = true
	s.BypassAdminOnly = true
	var actors []string
	for _, a := range rs.BypassActors {
		actors = append(actors, fmt.Sprintf("%s:%d(%s)", a.ActorType, a.ActorID, a.BypassMode))
		if a.ActorType != "RepositoryRole" || a.ActorID != ghAdminRoleID {
			s.BypassAdminOnly = false
		}
	}
	if len(actors) == 0 {
		s.BypassDetail = "none (not even the operator — stricter than required)"
	} else {
		s.BypassDetail = strings.Join(actors, ", ")
	}
}

// applyNamespaceRuleset folds the R8 confinement ruleset into the
// snapshot: creation and update restricted everywhere but the agent
// namespace (and the default branch, which has its own stricter rules),
// bypass admin-only.
func (g *GitHub) applyNamespaceRuleset(s *Snapshot, rs *ghRuleset, exclude []string, defaultRef string) {
	var creation, update bool
	for _, rule := range rs.Rules {
		switch rule.Type {
		case "creation":
			creation = true
		case "update":
			update = true
		}
	}
	adminOnly := true
	for _, a := range rs.BypassActors {
		if a.ActorType != "RepositoryRole" || a.ActorID != ghAdminRoleID {
			adminOnly = false
		}
	}
	s.NamespaceKnown = true
	s.NamespaceConfined = creation && update && adminOnly && contains(exclude, defaultRef)
	if s.NamespaceConfined {
		s.NamespaceDetail = fmt.Sprintf("branch creation/updates outside %q restricted server-side, admin-only bypass", g.cfg.AgentNamespace)
	} else {
		s.NamespaceDetail = fmt.Sprintf("namespace ruleset found but incomplete: creation=%v update=%v adminOnlyBypass=%v excludesDefault=%v", creation, update, adminOnly, contains(exclude, defaultRef))
	}
}

// applyCodeOwners fetches CODEOWNERS and completes the R2 and R6 verdicts:
// the owner-review rule only bites if the catch-all pattern names exactly
// the operator, and workflow files need an owner so their changes cannot
// merge without the operator.
func (g *GitHub) applyCodeOwners(ctx context.Context, s *Snapshot) {
	var raw []byte
	found := ""
	// Prefer the working tree: in CI the lint runs inside the checkout
	// being merged, and the CODEOWNERS that will govern after the merge is
	// the one to assert.  The API fallback serves operator runs outside a
	// checkout (it reads the default branch's copy).
	locations := []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}
	for _, path := range locations {
		if b, err := os.ReadFile(path); err == nil {
			raw, found = b, path+" (working tree)"
			break
		}
	}
	if found == "" {
		for _, path := range locations {
			if err := g.get(ctx, "/repos/"+g.cfg.Repo+"/contents/"+path, "application/vnd.github.raw+json", &raw); err == nil {
				found = path
				break
			}
		}
	}
	if found == "" {
		s.OwnerReviewOK = false
		s.OwnerReviewDetail = "no CODEOWNERS file — require-code-owner-review has no owner to require"
		s.WorkflowGuardDetail = "no CODEOWNERS file guards .github/** (workflow changes can merge without the operator)"
		return
	}
	ownerRef := "@" + g.cfg.Operator
	catchAll, workflows := codeownersOwners(string(raw))
	switch {
	case !s.OwnerReviewOK: // the ruleset didn't require owner review at all
		s.OwnerReviewDetail = "require-code-owner-review is off — any approver (the bot included) satisfies the review rule"
	case len(catchAll) != 1 || catchAll[0] != ownerRef:
		s.OwnerReviewOK = false
		s.OwnerReviewDetail = fmt.Sprintf("%s catch-all owners = %v, want exactly [%s]", found, catchAll, ownerRef)
	default:
		s.OwnerReviewDetail = fmt.Sprintf("owner review required and %s catch-all names exactly %s", found, ownerRef)
	}
	if len(workflows) == 1 && workflows[0] == ownerRef {
		s.WorkflowGuardOK = true
		s.WorkflowGuardDetail = fmt.Sprintf("%s puts .github/** behind %s", found, ownerRef)
	} else {
		s.WorkflowGuardDetail = fmt.Sprintf("%s owners for .github/workflows = %v, want exactly [%s]", found, workflows, ownerRef)
	}
}

// codeownersOwners parses a CODEOWNERS body and returns the owners of the
// catch-all pattern and of the workflow directory (last matching pattern
// wins, per CODEOWNERS semantics).  The matcher covers the pattern shapes
// this lint prescribes (*, /dir/, /dir/**), not the full gitignore
// grammar.
func codeownersOwners(body string) (catchAll, workflows []string) {
	for _, line := range strings.Split(body, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pattern, owners := fields[0], fields[1:]
		if pattern == "*" || pattern == "**" {
			catchAll = owners
			workflows = owners // a later, more specific pattern overrides
			continue
		}
		p := strings.TrimPrefix(pattern, "/")
		p = strings.TrimSuffix(strings.TrimSuffix(p, "**"), "/")
		if p != "" && strings.HasPrefix(".github/workflows/", p+"/") || p == ".github/workflows" {
			workflows = owners
		}
	}
	return catchAll, workflows
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
