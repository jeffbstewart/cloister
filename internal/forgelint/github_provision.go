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
	"fmt"
	"net/url"
)

// ProvisionSnapshot reads the subset of a repository's protection that the
// bot's OWN token can see, live at hand-over — the provision gate's input
// (docs/grange.md, "Provision-time verification").  It differs from
// Snapshot in what it reads, not what it fills: the operator lint lists
// rulesets and reads Actions secrets and collaborator roles; the bot reads
// the effective-rules endpoint (which does not expose the bypass roster),
// its own permission off GET /repos, CODEOWNERS, and namespace probes.
// The unreadable residue — bypass roster (R1), secrets (R6) — is left
// *Known=false, so Check reports it UNVERIFIED and Gate discounts it only
// where safe.
func (g *GitHub) ProvisionSnapshot(ctx context.Context) (*Snapshot, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
		// permissions reports the AUTHENTICATED user's access, so under the
		// bot's token this is the bot's own role (R5) — always readable, and
		// what the R1 bypass residue leans on.
		Permissions struct {
			Admin bool `json:"admin"`
			Push  bool `json:"push"`
		} `json:"permissions"`
	}
	if err := g.get(ctx, "/repos/"+g.cfg.Repo, "", &repo); err != nil {
		return nil, fmt.Errorf("read repository: %w", err)
	}
	s := &Snapshot{DefaultBranch: repo.DefaultBranch}
	s.BotPermissionKnown = true
	switch {
	case repo.Permissions.Admin:
		s.BotPermission = "admin"
	case repo.Permissions.Push:
		s.BotPermission = "write"
	default:
		s.BotPermission = "read"
	}

	// The default branch's effective rules: R1 (minus the bypass roster),
	// R2's flags, R3.  An empty read — a repo whose protection has lapsed —
	// leaves every flag false, which Check turns into R1/R2 violations and
	// the gate into a refusal, no attestation cadence needed.
	rules, err := g.effectiveRules(ctx, g.cfg.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("read effective rules for %s: %w", g.cfg.DefaultBranch, err)
	}
	applyBranchRules(s, rules)
	// The effective-rules endpoint does not report who may bypass; that
	// roster is the operator-credential lint's job.  Leave it unread rather
	// than assert a false "bypass = none" — Check then reports R1 UNVERIFIED,
	// which Gate tolerates only because R5 proves the bot is not an admin
	// (the sole sanctioned bypass).
	s.BypassKnown = false
	s.BypassDetail = "effective-rules endpoint does not expose the bypass roster (the operator-credential lint verifies it)"

	// CODEOWNERS completes R2 (owner review names exactly the operator) and
	// R6's mergeable half (.github/** is owner-guarded).
	g.applyCodeOwners(ctx, s)

	// The Actions-secrets inventory needs repo admin; it stays the operator
	// lint's job.  Left unread → R6 UNVERIFIED, tolerated by the gate.
	s.SecretsKnown = false

	g.probeNamespace(ctx, s)
	return s, nil
}

// effectiveRules reads the rules in force on one branch NAME, existing or
// not — the endpoint evaluates a hypothetical branch, which is what makes
// the R8 namespace probes possible.
func (g *GitHub) effectiveRules(ctx context.Context, branch string) ([]ghRule, error) {
	var rules []ghRule
	// A branch name carries slashes (agent/foo); PathEscape encodes them so
	// the name stays one path segment the API can resolve.
	path := "/repos/" + g.cfg.Repo + "/rules/branches/" + url.PathEscape(branch)
	if err := g.get(ctx, path, "", &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// nsProbeOutside and nsProbeInside are the two branch names the R8 probe
// evaluates.  Neither is ever created — the effective-rules endpoint scores
// them hypothetically.  The inside name is minted under the configured
// namespace so it moves with it.
const nsProbeOutside = "forgelint-namespace-probe"

// probeNamespace establishes R8 without reading the ruleset roster: a name
// OUTSIDE the agent namespace must be creation/update restricted, and an
// agent/… name must NOT be.  Confinement is the two facts together — a repo
// that restricts everything (or nothing) is not confinement.
func (g *GitHub) probeNamespace(ctx context.Context, s *Snapshot) {
	inside := g.cfg.AgentNamespace + "forgelint-namespace-probe"
	outRules, errOut := g.effectiveRules(ctx, nsProbeOutside)
	inRules, errIn := g.effectiveRules(ctx, inside)
	if errOut != nil || errIn != nil {
		s.NamespaceKnown = false
		s.NamespaceDetail = fmt.Sprintf("namespace probes unreadable with this credential (outside %q: %v; inside: %v)", nsProbeOutside, errOut, errIn)
		return
	}
	outsideRestricted := hasRule(outRules, "creation") && hasRule(outRules, "update")
	insideRestricted := hasRule(inRules, "creation") || hasRule(inRules, "update")
	s.NamespaceKnown = true
	s.NamespaceConfined = outsideRestricted && !insideRestricted
	if s.NamespaceConfined {
		s.NamespaceDetail = fmt.Sprintf("branch creation/updates outside %q are restricted, inside are not (probed live)", g.cfg.AgentNamespace)
	} else {
		s.NamespaceDetail = fmt.Sprintf("namespace not confined: %q restricted=%v, %q restricted=%v (want true/false)", nsProbeOutside, outsideRestricted, inside, insideRestricted)
	}
}

// hasRule reports whether a rule of the given type is in force.
func hasRule(rules []ghRule, typ string) bool {
	for _, r := range rules {
		if r.Type == typ {
			return true
		}
	}
	return false
}
