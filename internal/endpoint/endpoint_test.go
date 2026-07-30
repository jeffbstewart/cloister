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

package endpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validTable = `endpoints:
  - name: github.com
    canonical: https://github.com/
    wire: https://github.com/
    forge: github
    api: https://api.github.com/
    credentialFile: /run/secrets/github-token
    bot:
      name: example-agent
      email: bot@example.test
  - name: gitea
    canonical: https://gitea.example.test:8443/
    wire: http://gitea-relay:3000/
    forge: gitea
    api: https://gitea.example.test:8443/api/v1/
    credentialFile: /run/secrets/gitea-token
    bot:
      name: example-gitea-agent
      email: gitea-bot@example.test
`

func writeTable(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "endpoints.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValidTable(t *testing.T) {
	tab, err := Load(writeTable(t, validTable))
	if err != nil {
		t.Fatal(err)
	}
	e, ok := tab.ByName("github.com")
	if !ok || e.Bot.Name != "example-agent" || e.Forge != ForgeGitHub {
		t.Errorf("ByName(github.com) = %+v, %v", e, ok)
	}
}

func TestLoadFailClosed(t *testing.T) {
	cases := map[string]string{
		"unknown key":       strings.Replace(validTable, "api:", "apiBase:", 1),
		"missing forge":     strings.Replace(validTable, "forge: github", "forge: \"\"", 1),
		"bad forge":         strings.Replace(validTable, "forge: github", "forge: svn", 1),
		"no trailing slash": strings.Replace(validTable, "canonical: https://github.com/", "canonical: https://github.com", 1),
		"non-http scheme":   strings.Replace(validTable, "canonical: https://github.com/", "canonical: ssh://git@github.com/", 1),
		"empty credential":  strings.Replace(validTable, "credentialFile: /run/secrets/github-token", "credentialFile: \"\"", 1),
		"identity brackets": strings.Replace(validTable, "name: example-agent", "name: <script>", 1),
		"email not address": strings.Replace(validTable, "email: bot@example.test", "email: nope", 1),
		"duplicate name":    strings.Replace(validTable, "name: gitea", "name: github.com", 1),
		"empty table":       "endpoints: []\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTable(t, content)); err == nil {
				t.Error("Load accepted an invalid table; the table must fail closed")
			}
		})
	}
}

func TestForRemoteIsTheAllowlist(t *testing.T) {
	tab, err := Load(writeTable(t, validTable))
	if err != nil {
		t.Fatal(err)
	}
	e, err := tab.ForRemote("https://github.com/example/repo.git")
	if err != nil || e.Name != "github.com" {
		t.Errorf("ForRemote(github repo) = %v, %v", e, err)
	}
	// The wire prefix matches too: a provisioned clone may carry either.
	if e, err := tab.ForRemote("http://gitea-relay:3000/jeff/vivarium.git"); err != nil || e.Name != "gitea" {
		t.Errorf("ForRemote(gitea wire) = %v, %v", e, err)
	}
	for _, bad := range []string{
		"file:///workspace/.git",
		"ssh://git@github.com/example/repo.git",
		"git@github.com:example/repo.git",
		"https://evil.example/github.com/",
		"https://github.com.evil.example/x",
		"/some/bare/path",
	} {
		if _, err := tab.ForRemote(bad); err == nil {
			t.Errorf("ForRemote(%q) resolved; it must refuse anything outside the table", bad)
		}
	}
}

func TestToken(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "token")
	if err := os.WriteFile(cred, []byte("  s3cret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Endpoint{Name: "github.com", CredentialFile: cred}
	tok, err := e.Token()
	if err != nil || tok != "s3cret-token" {
		t.Errorf("Token() = %q, %v; want the trimmed secret", tok, err)
	}

	if err := os.WriteFile(cred, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Token(); err == nil {
		t.Error("an empty credential file must be an error, not an anonymous fallback")
	}

	e.CredentialFile = filepath.Join(dir, "missing")
	if _, err := e.Token(); err == nil {
		t.Error("a missing credential file must be an error")
	}
}
