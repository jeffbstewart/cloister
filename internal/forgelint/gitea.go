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
	"strings"
)

// Gitea reads a repository's protected-branch rules, collaborator roles,
// and Actions secrets from a Gitea instance and normalizes them into a
// Snapshot (docs/grange.md, "Gitea realization").
type Gitea struct {
	cfg    *Config
	token  string
	client *http.Client
}

// NewGitea builds the Gitea backend.  A nil client uses
// http.DefaultClient.
func NewGitea(cfg *Config, token string, client *http.Client) *Gitea {
	if client == nil {
		client = http.DefaultClient
	}
	return &Gitea{cfg: cfg, token: token, client: client}
}

func (g *Gitea) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.APIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if g.token != "" {
		req.Header.Set("Authorization", "token "+g.token)
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
	return json.Unmarshal(body, out)
}

// giteaProtection is the slice of Gitea's BranchProtection the lint reads.
type giteaProtection struct {
	BranchName                  string   `json:"branch_name"`
	RuleName                    string   `json:"rule_name"`
	EnablePush                  bool     `json:"enable_push"`
	RequiredApprovals           int      `json:"required_approvals"`
	DismissStaleApprovals       bool     `json:"dismiss_stale_approvals"`
	EnableStatusCheck           bool     `json:"enable_status_check"`
	StatusCheckContexts         []string `json:"status_check_contexts"`
	EnableApprovalsWhitelist    bool     `json:"enable_approvals_whitelist"`
	ApprovalsWhitelistUsernames []string `json:"approvals_whitelist_username"`
	ProtectedFilePatterns       string   `json:"protected_file_patterns"`
}

// Snapshot reads the protection state.  Reading branch protections on
// Gitea needs repo-admin — the operator credential R7 prescribes — so a
// refusal there aborts: with no rules visible there is nothing to assert.
func (g *Gitea) Snapshot(ctx context.Context) (*Snapshot, error) {
	repoPath := "/repos/" + g.cfg.Repo
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.get(ctx, repoPath, &repo); err != nil {
		return nil, fmt.Errorf("read repository: %w", err)
	}
	s := &Snapshot{DefaultBranch: repo.DefaultBranch}

	var rules []giteaProtection
	if err := g.get(ctx, repoPath+"/branch_protections", &rules); err != nil {
		return nil, fmt.Errorf("list branch protections: %w", err)
	}
	for i := range rules {
		rule := &rules[i]
		name := rule.RuleName
		if name == "" {
			name = rule.BranchName
		}
		if name != g.cfg.DefaultBranch {
			continue
		}
		// A protected branch in Gitea blocks force-pushes and deletion by
		// construction; enable_push=false closes direct pushes entirely.
		s.PRRequired = !rule.EnablePush
		s.ForcePushBlocked = true
		s.DeletionBlocked = true
		s.RequiredApprovals = rule.RequiredApprovals
		s.DismissStale = rule.DismissStaleApprovals
		if rule.EnableStatusCheck {
			s.RequiredChecks = rule.StatusCheckContexts
		}
		operatorOnly := len(rule.ApprovalsWhitelistUsernames) == 1 && rule.ApprovalsWhitelistUsernames[0] == g.cfg.Operator
		switch {
		case !rule.EnableApprovalsWhitelist:
			s.OwnerReviewDetail = "approvals whitelist is off — any approver (the bot included) satisfies the review rule"
		case !operatorOnly:
			s.OwnerReviewDetail = fmt.Sprintf("approvals whitelist = %v, want exactly [%s]", rule.ApprovalsWhitelistUsernames, g.cfg.Operator)
		default:
			s.OwnerReviewOK = true
			s.OwnerReviewDetail = fmt.Sprintf("approvals whitelist names exactly %s", g.cfg.Operator)
		}
		g.applyWorkflowGuard(s, rule)
	}
	if s.OwnerReviewDetail == "" {
		s.OwnerReviewDetail = fmt.Sprintf("no protection rule found for branch %q", g.cfg.DefaultBranch)
		s.WorkflowGuardDetail = s.OwnerReviewDetail
	}

	g.applyAdminSet(ctx, s, repoPath)

	var perm struct {
		Permission string `json:"permission"`
	}
	if err := g.get(ctx, repoPath+"/collaborators/"+g.cfg.Bot+"/permission", &perm); err == nil {
		s.BotPermissionKnown = true
		s.BotPermission = perm.Permission
	}

	// Secrets listing wants admin; unreadable = UNVERIFIED, never a pass.
	var secrets []struct {
		Name string `json:"name"`
	}
	if err := g.get(ctx, repoPath+"/actions/secrets", &secrets); err == nil {
		s.SecretsKnown = true
		for _, sec := range secrets {
			s.SecretNames = append(s.SecretNames, sec.Name)
		}
	}

	// R8 stays client-side on Gitea until the multi-rule-precedence pilot
	// (docs/grange.md): the archivist's refusal carries it alone.
	s.NamespaceKnown = false
	s.NamespaceDetail = "Gitea multi-rule precedence unverified by the pilot — the archivist's client-side namespace refusal carries R8 for now"
	return s, nil
}

// applyWorkflowGuard checks the protected-file-patterns guard on workflow
// definitions — Gitea's structural answer to agent-edited CI (R6).
func (g *Gitea) applyWorkflowGuard(s *Snapshot, rule *giteaProtection) {
	for _, pattern := range strings.Split(rule.ProtectedFilePatterns, ";") {
		if strings.Contains(pattern, ".gitea/workflows") || strings.Contains(pattern, ".github/workflows") {
			s.WorkflowGuardOK = true
			s.WorkflowGuardDetail = fmt.Sprintf("protected file pattern %q shields workflow definitions", strings.TrimSpace(pattern))
			return
		}
	}
	s.WorkflowGuardDetail = fmt.Sprintf("protected_file_patterns %q does not shield .gitea/workflows/**", rule.ProtectedFilePatterns)
}

// applyAdminSet asserts the bypass story: Gitea admins step around
// protection by default, so the admin set must be exactly the operator
// (the owner is implicitly the operator's own account).
func (g *Gitea) applyAdminSet(ctx context.Context, s *Snapshot, repoPath string) {
	var collabs []struct {
		Login       string `json:"login"`
		Permissions struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if err := g.get(ctx, repoPath+"/collaborators", &collabs); err != nil {
		s.BypassDetail = "collaborator list unreadable"
		return
	}
	s.BypassKnown = true
	s.BypassAdminOnly = true
	var admins []string
	for _, c := range collabs {
		if c.Permissions.Admin && c.Login != g.cfg.Operator {
			s.BypassAdminOnly = false
			admins = append(admins, c.Login)
		}
	}
	if s.BypassAdminOnly {
		s.BypassDetail = fmt.Sprintf("gitea admins bypass by default; no admin collaborator besides %s", g.cfg.Operator)
	} else {
		s.BypassDetail = "admin collaborators besides the operator: " + strings.Join(admins, ", ")
	}
}
