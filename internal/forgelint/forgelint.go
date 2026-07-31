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

// Package forgelint verifies that the forge repository the archivist will
// push to actually enforces the grange design's safety requirements R1-R8
// (docs/grange.md, "Forge requirements"): PRs-only on the default branch,
// an approval the bot cannot satisfy and cannot keep across new pushes,
// required checks, a minimally-scoped bot, workflow-edit guards, and the
// agent branch namespace.  One forge-agnostic assertion set (Check) runs
// against a Snapshot; GitHub and Gitea backends (github.go, gitea.go) each
// read their protection APIs and normalize into that Snapshot.
//
// The lint runs with an OPERATOR credential outside any cell (R7): reading
// some sections (Actions secrets, collaborator roles) needs repo admin,
// which the bot token must never have.  A section the credential cannot
// read yields an UNVERIFIED verdict, never a silent pass.
package forgelint

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Forge names a supported backend.
type Forge string

const (
	ForgeGitHub Forge = "github"
	ForgeGitea  Forge = "gitea"
)

// HardeningRunbook names the doc section a caller points at when a check
// fails: the steps to bring a repository up to R1-R8.  Both the operator
// lint (cmd/forge-lint) and the provision gate's refusal cite it, so the
// pointer has one source of truth and cannot drift as the section is
// renamed (docs/grange.md).
const HardeningRunbook = `docs/grange.md, "Locking down a project for grange service"`

// Config is the per-repository lint configuration (etc/forge-lint.yaml).
// Every field is REQUIRED — fail-closed like every other config in this
// repo.  It holds identities and expectations only, never a credential:
// the token arrives via the environment.
type Config struct {
	// Forge selects the backend: github | gitea.
	Forge Forge `yaml:"forge"`
	// APIBase is the API root: https://api.github.com for GitHub, or the
	// instance's /api/v1 for Gitea.
	APIBase string `yaml:"apiBase"`
	// Repo is owner/name.
	Repo string `yaml:"repo"`
	// DefaultBranch is the protected integration branch (R1's subject).
	DefaultBranch string `yaml:"defaultBranch"`
	// Operator is the human whose approval satisfies R2.
	Operator string `yaml:"operator"`
	// Bot is the agent identity; a Write collaborator, never an admin.
	Bot string `yaml:"bot"`
	// RequiredChecks are the status contexts merge must wait for (R3).
	RequiredChecks []string `yaml:"requiredChecks"`
	// AgentNamespace is the branch prefix the bot is confined to (R8),
	// e.g. "agent/".
	AgentNamespace string `yaml:"agentNamespace"`
}

// LoadConfig reads and validates the lint configuration.  FAIL-CLOSED: a
// missing file, unknown key, or unset field is an error.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("forge-lint: read config %q: %w", path, err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("forge-lint: parse config %q: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("forge-lint: invalid config %q: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	switch c.Forge {
	case ForgeGitHub, ForgeGitea:
	case "":
		return fmt.Errorf("forge is required (github or gitea)")
	default:
		return fmt.Errorf("forge %q: want github or gitea", c.Forge)
	}
	if !strings.HasPrefix(c.APIBase, "https://") && !strings.HasPrefix(c.APIBase, "http://") {
		return fmt.Errorf("apiBase is required and must be an http(s) URL")
	}
	owner, name, ok := strings.Cut(c.Repo, "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("repo is required as owner/name")
	}
	if c.DefaultBranch == "" {
		return fmt.Errorf("defaultBranch is required")
	}
	if c.Operator == "" {
		return fmt.Errorf("operator is required")
	}
	if c.Bot == "" {
		return fmt.Errorf("bot is required")
	}
	if c.Operator == c.Bot {
		return fmt.Errorf("operator and bot must be different identities — a self-approving bot is exactly what R2 forbids")
	}
	if len(c.RequiredChecks) == 0 {
		return fmt.Errorf("requiredChecks must list at least one status context")
	}
	if c.AgentNamespace == "" || !strings.HasSuffix(c.AgentNamespace, "/") {
		return fmt.Errorf("agentNamespace is required and must end with \"/\" (e.g. \"agent/\")")
	}
	return nil
}

// Snapshot is the forge-agnostic view of the repository's protection that
// the assertion set runs against.  A backend fills it two ways: a full
// operator read (GitHub.Snapshot) or a partial bot-credential read for the
// provision gate (GitHub.ProvisionSnapshot), which deliberately leaves the
// admin-only sections unread.  The *Known flags mark sections the
// credential could not read — whether because it lacks the scope or because
// that reader does not fetch them: those requirements report UNVERIFIED
// rather than silently passing (or failing).
type Snapshot struct {
	DefaultBranch string // as the forge reports it

	// The default branch's protection (R1-R3).
	PRRequired        bool
	RequiredApprovals int
	DismissStale      bool
	RequiredChecks    []string
	ForcePushBlocked  bool
	DeletionBlocked   bool

	// OwnerReviewOK: the mechanism that makes the bot's approval worthless
	// holds — GitHub: require-code-owner-review plus a CODEOWNERS whose
	// catch-all names exactly the operator; Gitea: an approvals whitelist
	// of exactly the operator.
	OwnerReviewOK     bool
	OwnerReviewDetail string

	// Bypass: who may step around the default branch's rules.  Admin-only
	// is the sanctioned shape (the operator's emergency override).
	BypassKnown     bool
	BypassAdminOnly bool
	BypassDetail    string

	// The bot's repository role (R5).
	BotPermissionKnown bool
	BotPermission      string

	// Workflow-edit guard and secrets audit (R6).
	WorkflowGuardOK     bool
	WorkflowGuardDetail string
	SecretsKnown        bool
	SecretNames         []string

	// Branch-namespace confinement (R8).
	NamespaceKnown    bool
	NamespaceConfined bool
	NamespaceDetail   string
}

// Status is a verdict's outcome.
type Status string

const (
	OK         Status = "ok"
	Violation  Status = "VIOLATION"
	Unverified Status = "UNVERIFIED"
)

// Verdict is one requirement's outcome.
type Verdict struct {
	Req    string // "R1".."R8"
	Status Status
	Detail string
}

// Check runs the R1-R8 assertion set against a snapshot.  Verdicts come
// back in requirement order; the caller decides how UNVERIFIED maps to an
// exit code (fail-closed by default).
func Check(cfg *Config, s *Snapshot) []Verdict {
	var v []Verdict
	verdict := func(req string, problems []string, okDetail string) {
		if len(problems) > 0 {
			v = append(v, Verdict{req, Violation, strings.Join(problems, "; ")})
			return
		}
		v = append(v, Verdict{req, OK, okDetail})
	}

	// R1: the default branch takes changes via PR only, and only the
	// operator's admin role can bypass.
	var p []string
	if s.DefaultBranch != cfg.DefaultBranch {
		p = append(p, fmt.Sprintf("forge default branch is %q, config expects %q", s.DefaultBranch, cfg.DefaultBranch))
	}
	if !s.PRRequired {
		p = append(p, "direct pushes to the default branch are not blocked (require a pull request)")
	}
	if !s.ForcePushBlocked {
		p = append(p, "force-pushes to the default branch are not blocked")
	}
	if !s.DeletionBlocked {
		p = append(p, "deletion of the default branch is not blocked")
	}
	switch {
	case !s.BypassKnown:
		if len(p) > 0 {
			v = append(v, Verdict{"R1", Violation, strings.Join(p, "; ")})
		} else {
			v = append(v, Verdict{"R1", Unverified, "bypass list unreadable with this credential: " + s.BypassDetail})
		}
	default:
		if !s.BypassAdminOnly {
			p = append(p, "bypass is not admin-role-only: "+s.BypassDetail)
		}
		verdict("R1", p, fmt.Sprintf("changes land via PR only; force-push and deletion blocked; bypass = %s", s.BypassDetail))
	}

	// R2: an approval the bot cannot satisfy and cannot keep.
	p = nil
	if s.RequiredApprovals < 1 {
		p = append(p, "merge does not require an approving review")
	}
	if !s.DismissStale {
		p = append(p, "new pushes do not dismiss prior approvals (approve, then push malice, then merge)")
	}
	if !s.OwnerReviewOK {
		p = append(p, "the bot's approval can satisfy the review requirement: "+s.OwnerReviewDetail)
	}
	verdict("R2", p, fmt.Sprintf("≥%d approval required, stale approvals dismissed, %s", s.RequiredApprovals, s.OwnerReviewDetail))

	// R3: required status checks.
	p = nil
	for _, want := range cfg.RequiredChecks {
		if !slices.Contains(s.RequiredChecks, want) {
			p = append(p, fmt.Sprintf("required check %q is not enforced", want))
		}
	}
	verdict("R3", p, "required checks enforced: "+strings.Join(s.RequiredChecks, ", "))

	// R4: no forge expresses "only a human may press merge" directly; the
	// structural equivalent is that a merge needs the operator's LIVE
	// approval (R2's owner-review + dismissal), and the bot holds no
	// bypass.  R4 therefore stands or falls with R1+R2.
	p = nil
	if !s.PRRequired || s.RequiredApprovals < 1 || !s.OwnerReviewOK || !s.DismissStale {
		p = append(p, "without PR-only + operator-only approval + stale dismissal, the bot could complete a merge alone")
	}
	verdict("R4", p, "structural: merging needs the operator's live approval, which any bot push dismisses")

	// R5: the bot is a plain writer.
	switch {
	case !s.BotPermissionKnown:
		v = append(v, Verdict{"R5", Unverified, fmt.Sprintf("bot %q's repository role unreadable with this credential", cfg.Bot)})
	case s.BotPermission != "write":
		v = append(v, Verdict{"R5", Violation, fmt.Sprintf("bot %q holds role %q, want exactly write (token scopes are not API-readable; keep the PAT fine-grained)", cfg.Bot, s.BotPermission)})
	default:
		v = append(v, Verdict{"R5", OK, fmt.Sprintf("bot %q is a write collaborator, nothing more (token scopes are not API-readable; keep the PAT fine-grained)", cfg.Bot)})
	}

	// R6: agent-editable CI must not run with secrets or weaken checks.
	p = nil
	if !s.WorkflowGuardOK {
		p = append(p, "workflow changes are not guarded: "+s.WorkflowGuardDetail)
	}
	switch {
	case !s.SecretsKnown:
		if len(p) > 0 {
			v = append(v, Verdict{"R6", Violation, strings.Join(p, "; ")})
		} else {
			v = append(v, Verdict{"R6", Unverified, "Actions secrets unreadable with this credential (needs repo admin); " + s.WorkflowGuardDetail})
		}
	default:
		if len(s.SecretNames) > 0 {
			p = append(p, "Actions secrets exist ("+strings.Join(s.SecretNames, ", ")+") — zero-secret CI is the policy")
		}
		verdict("R6", p, "zero Actions secrets; "+s.WorkflowGuardDetail)
	}

	// R7: this run is R7 — the protection configuration was readable via
	// API with an operator credential.
	v = append(v, Verdict{"R7", OK, "protection configuration read via the forge API"})

	// R8: the bot's branches live in its namespace, server-side where the
	// forge can express it.
	switch {
	case !s.NamespaceKnown:
		v = append(v, Verdict{"R8", Unverified, s.NamespaceDetail})
	case !s.NamespaceConfined:
		v = append(v, Verdict{"R8", Violation, s.NamespaceDetail})
	default:
		v = append(v, Verdict{"R8", OK, s.NamespaceDetail})
	}
	return v
}
