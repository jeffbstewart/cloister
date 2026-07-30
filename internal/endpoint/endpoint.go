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

// Package endpoint is the archivist's endpoint table
// (docs/archivist.md, "Endpoints, identity, and credential"): one entry
// per reachable git endpoint, all of them one human principal's bots.
// The table is deployment config holding credential PATHS, never
// secrets, mounted read-only.
//
// The table is also the remote allowlist: a remote URL that matches no
// entry's canonical or wire prefix is refused before git ever runs —
// which structurally refuses file://, ssh, bare paths, and therefore
// every host repository.  There is no identity parameter anywhere: the
// actor follows from (instance's human × repo's host) by lookup, so it
// cannot be chosen, and no archivist ever aggregates two humans'
// credentials.
package endpoint

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Forge names a supported backend kind for the PR-authoring verbs.
type Forge string

const (
	ForgeGitHub Forge = "github"
	ForgeGitea  Forge = "gitea"
)

// Identity is the bot identity checkpoints and PRs happen as at this
// endpoint.  The GitHub and Gitea bots are different actors, so
// identity rides with the endpoint, never the instance.
type Identity struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Endpoint is one reachable git endpoint.
type Endpoint struct {
	// Name is also the relay's name in the topology: "github.com",
	// "gitea".
	Name string `yaml:"name"`
	// Canonical is how repositories are designated everywhere:
	// "https://github.com/", the Gitea front URL.
	Canonical string `yaml:"canonical"`
	// Wire is where the bytes actually go: "https://github.com/"
	// resolved to the relay by network alias, or the plain-http Gitea
	// relay URL.  When Wire differs from Canonical the runner maps
	// designations per invocation (-c url.<wire>.insteadOf=<canonical>);
	// the repo-local config never carries the mapping.
	Wire string `yaml:"wire"`
	// Forge selects the PR-verb backend: github | gitea.
	Forge Forge `yaml:"forge"`
	// API is the forge API root ("https://api.github.com/"), dialed
	// through its own relay alias.
	API string `yaml:"api"`
	// CredentialFile is the read-only mounted token file.  The agent
	// never sees it; the table never holds its content.
	CredentialFile string `yaml:"credentialFile"`
	// Bot is the commit author identity at this endpoint.
	Bot Identity `yaml:"bot"`
}

// Table is the parsed endpoint table.
type Table struct {
	endpoints []Endpoint
}

// tableFile is the on-disk shape.
type tableFile struct {
	Endpoints []Endpoint `yaml:"endpoints"`
}

// Load reads and validates the endpoint table.  FAIL-CLOSED like every
// config in this repo: a missing file, unknown key, or invalid entry is
// an error.
func Load(path string) (*Table, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("endpoint: read table %q: %w", path, err)
	}
	var f tableFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("endpoint: parse table %q: %w", path, err)
	}
	if len(f.Endpoints) == 0 {
		return nil, fmt.Errorf("endpoint: table %q lists no endpoints", path)
	}
	seen := map[string]bool{}
	for i := range f.Endpoints {
		e := &f.Endpoints[i]
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("endpoint: table %q entry %d: %w", path, i, err)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("endpoint: table %q: duplicate endpoint name %q", path, e.Name)
		}
		seen[e.Name] = true
	}
	return &Table{endpoints: f.Endpoints}, nil
}

// urlPrefixOK vets a URL prefix field: http(s), trailing slash so
// prefix matching can never cross a path-segment boundary.
func urlPrefixOK(s string) bool {
	return (strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) &&
		strings.HasSuffix(s, "/")
}

func (e *Endpoint) validate() error {
	if e.Name == "" || strings.ContainsAny(e.Name, " \t\n") {
		return fmt.Errorf("name is required and carries no whitespace (it is also the relay's name)")
	}
	if !urlPrefixOK(e.Canonical) {
		return fmt.Errorf("canonical is required as an http(s) URL prefix ending in \"/\"")
	}
	if !urlPrefixOK(e.Wire) {
		return fmt.Errorf("wire is required as an http(s) URL prefix ending in \"/\"")
	}
	switch e.Forge {
	case ForgeGitHub, ForgeGitea:
	case "":
		return fmt.Errorf("forge is required (github or gitea)")
	default:
		return fmt.Errorf("forge %q: want github or gitea", e.Forge)
	}
	if !urlPrefixOK(e.API) {
		return fmt.Errorf("api is required as an http(s) URL prefix ending in \"/\"")
	}
	if e.CredentialFile == "" {
		return fmt.Errorf("credentialFile is required (a read-only mounted token file path)")
	}
	// The identity lands in `-c user.name=` and a commit ident line;
	// the same rule the archive engine enforces, applied at load so a
	// bad table fails at boot, not at the first checkpoint.
	for _, f := range []struct{ what, val string }{{"bot.name", e.Bot.Name}, {"bot.email", e.Bot.Email}} {
		if f.val == "" {
			return fmt.Errorf("%s is required", f.what)
		}
		if len(f.val) > 200 {
			return fmt.Errorf("%s is over 200 bytes", f.what)
		}
		for i := 0; i < len(f.val); i++ {
			if c := f.val[i]; c < 0x20 || c > 0x7e {
				return fmt.Errorf("%s byte %d (%#x) is not printable ASCII", f.what, i, c)
			}
		}
		if strings.ContainsAny(f.val, "<>") {
			return fmt.Errorf("%s may not contain '<' or '>'", f.what)
		}
	}
	if !strings.Contains(e.Bot.Email, "@") {
		return fmt.Errorf("bot.email %q is not an address", e.Bot.Email)
	}
	return nil
}

// ForRemote resolves the endpoint a remote URL belongs to by canonical
// (or wire) prefix.  An unmatched URL is refused — this lookup IS the
// allowlist, and it runs before git does.
func (t *Table) ForRemote(remoteURL string) (*Endpoint, error) {
	for i := range t.endpoints {
		e := &t.endpoints[i]
		if strings.HasPrefix(remoteURL, e.Canonical) || strings.HasPrefix(remoteURL, e.Wire) {
			return e, nil
		}
	}
	return nil, fmt.Errorf("endpoint: remote %q matches no endpoint; the table is the allowlist and this workspace's remote is outside it", remoteURL)
}

// ByName resolves an endpoint by its (relay) name.
func (t *Table) ByName(name string) (*Endpoint, bool) {
	for i := range t.endpoints {
		if t.endpoints[i].Name == name {
			return &t.endpoints[i], true
		}
	}
	return nil, false
}

// Token reads the endpoint's credential file.  Read per call, never
// cached: the mounted file is the rotation point.
func (e *Endpoint) Token() (string, error) {
	raw, err := os.ReadFile(e.CredentialFile)
	if err != nil {
		return "", fmt.Errorf("endpoint %s: read credential: %w", e.Name, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("endpoint %s: credential file %s is empty", e.Name, e.CredentialFile)
	}
	return tok, nil
}
