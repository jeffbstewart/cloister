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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `
forge: github
apiBase: https://api.github.com
repo: op/repo
defaultBranch: main
operator: op
bot: bot
requiredChecks: [verify]
agentNamespace: agent/
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "forge-lint.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigValid(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Forge != ForgeGitHub || c.Repo != "op/repo" || c.Operator != "op" {
		t.Errorf("unexpected: %+v", c)
	}
}

func TestLoadConfigFailClosed(t *testing.T) {
	tests := map[string]string{
		"unknown key":         validConfig + "bogus: 1\n",
		"missing forge":       strings.Replace(validConfig, "forge: github\n", "", 1),
		"bad forge":           strings.Replace(validConfig, "forge: github", "forge: sourceforge", 1),
		"missing repo":        strings.Replace(validConfig, "repo: op/repo\n", "", 1),
		"repo not owner/name": strings.Replace(validConfig, "repo: op/repo", "repo: just-a-name", 1),
		"missing operator":    strings.Replace(validConfig, "operator: op\n", "", 1),
		"bot == operator":     strings.Replace(validConfig, "bot: bot", "bot: op", 1),
		"no required checks":  strings.Replace(validConfig, "requiredChecks: [verify]", "requiredChecks: []", 1),
		"namespace no slash":  strings.Replace(validConfig, "agentNamespace: agent/", "agentNamespace: agent", 1),
	}
	for name, body := range tests {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: want fail-closed error, got nil", name)
		}
	}
}

// cleanSnapshot is a fully-compliant snapshot for the validConfig repo.
func cleanSnapshot() *Snapshot {
	return &Snapshot{
		DefaultBranch:     "main",
		PRRequired:        true,
		RequiredApprovals: 1,
		DismissStale:      true,
		RequiredChecks:    []string{"verify"},
		ForcePushBlocked:  true,
		DeletionBlocked:   true,
		OwnerReviewOK:     true, OwnerReviewDetail: "owner review by op",
		BypassKnown: true, BypassAdminOnly: true, BypassDetail: "role:admin",
		BotPermissionKnown: true, BotPermission: "write",
		SecretsKnown:    true,
		WorkflowGuardOK: true, WorkflowGuardDetail: "workflows behind op",
		NamespaceKnown: true, NamespaceConfined: true, NamespaceDetail: "confined",
	}
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	c, err := LoadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func statusOf(vs []Verdict, req string) Status {
	for _, v := range vs {
		if v.Req == req {
			return v.Status
		}
	}
	return "missing"
}

func TestCheckCleanSnapshotPasses(t *testing.T) {
	for _, v := range Check(testConfig(t), cleanSnapshot()) {
		if v.Status != OK {
			t.Errorf("%s = %s (%s), want ok", v.Req, v.Status, v.Detail)
		}
	}
}

func TestCheckCatchesViolations(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Snapshot)
		req    string
		want   Status
	}{
		"direct pushes open":     {func(s *Snapshot) { s.PRRequired = false }, "R1", Violation},
		"stranger in bypass":     {func(s *Snapshot) { s.BypassAdminOnly = false }, "R1", Violation},
		"bypass unreadable":      {func(s *Snapshot) { s.BypassKnown = false }, "R1", Unverified},
		"no stale dismissal":     {func(s *Snapshot) { s.DismissStale = false }, "R2", Violation},
		"bot approval counts":    {func(s *Snapshot) { s.OwnerReviewOK = false }, "R2", Violation},
		"check not required":     {func(s *Snapshot) { s.RequiredChecks = nil }, "R3", Violation},
		"bot could merge alone":  {func(s *Snapshot) { s.OwnerReviewOK = false }, "R4", Violation},
		"bot is admin":           {func(s *Snapshot) { s.BotPermission = "admin" }, "R5", Violation},
		"bot role unreadable":    {func(s *Snapshot) { s.BotPermissionKnown = false }, "R5", Unverified},
		"a secret exists":        {func(s *Snapshot) { s.SecretNames = []string{"DEPLOY_KEY"} }, "R6", Violation},
		"secrets unreadable":     {func(s *Snapshot) { s.SecretsKnown = false }, "R6", Unverified},
		"workflows unguarded":    {func(s *Snapshot) { s.WorkflowGuardOK = false }, "R6", Violation},
		"namespace open":         {func(s *Snapshot) { s.NamespaceConfined = false }, "R8", Violation},
		"namespace unverifiable": {func(s *Snapshot) { s.NamespaceKnown = false }, "R8", Unverified},
	}
	for name, tc := range tests {
		s := cleanSnapshot()
		tc.mutate(s)
		if got := statusOf(Check(testConfig(t), s), tc.req); got != tc.want {
			t.Errorf("%s: %s = %s, want %s", name, tc.req, got, tc.want)
		}
	}
}
