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
	"errors"
	"fmt"
	"net"
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

// Validate vets a bot identity: it lands in `-c user.name=` and a
// commit's ident line, where a newline or an angle bracket would
// corrupt the object git writes.  This is the one authority for the
// rule; archive delegates to it.
func (i Identity) Validate() error {
	for _, f := range []struct{ what, val string }{{"name", i.Name}, {"email", i.Email}} {
		if f.val == "" {
			return fmt.Errorf("identity %s is required", f.what)
		}
		if len(f.val) > 200 {
			return fmt.Errorf("identity %s is over 200 bytes", f.what)
		}
		for j := 0; j < len(f.val); j++ {
			if c := f.val[j]; c < 0x20 || c > 0x7e {
				return fmt.Errorf("identity %s byte %d (%#x) is not printable ASCII", f.what, j, c)
			}
		}
		if strings.ContainsAny(f.val, "<>") {
			return fmt.Errorf("identity %s may not contain '<' or '>'", f.what)
		}
	}
	if !strings.Contains(i.Email, "@") {
		return fmt.Errorf("identity email %q is not an address", i.Email)
	}
	return nil
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
	// API is the forge API root ("https://api.github.com/").  It is the
	// name TLS verifies against; the bytes are dialed to APIRelay.
	API string `yaml:"api"`
	// APIRelay is the "host:port" the forge API client actually dials —
	// the api relay's service name on the cell network.  The API
	// hostname is NOT aliased (so the relay resolves the real forge
	// without a self-referential loop); the guarded client redirects the
	// connection here while TLS still verifies API's certificate.
	APIRelay string `yaml:"apiRelay"`
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
	if _, _, err := net.SplitHostPort(e.APIRelay); err != nil {
		return fmt.Errorf("apiRelay is required as host:port (the api relay's cell address): %w", err)
	}
	if e.CredentialFile == "" {
		return fmt.Errorf("credentialFile is required (a read-only mounted token file path)")
	}
	// Vet the identity at load, so a bad table fails at boot, not at the
	// first checkpoint.
	if err := e.Bot.Validate(); err != nil {
		return fmt.Errorf("bot %w", err)
	}
	return nil
}

// ErrNotAllowed reports a remote URL that matches no endpoint — the
// allowlist refusal, as a sentinel callers can test with errors.Is.
var ErrNotAllowed = errors.New("endpoint: remote matches no endpoint; the table is the allowlist and this workspace's remote is outside it")

// ForRemote resolves the endpoint a remote URL belongs to by canonical
// (or wire) prefix.  An unmatched URL is refused — this lookup IS the
// allowlist, and it runs before git does.  The endpoint is returned by
// value: the table's validated entries stay unmutable behind its
// unexported field.
func (t *Table) ForRemote(remoteURL string) (Endpoint, error) {
	for _, e := range t.endpoints {
		if strings.HasPrefix(remoteURL, e.Canonical) || strings.HasPrefix(remoteURL, e.Wire) {
			return e, nil
		}
	}
	return Endpoint{}, fmt.Errorf("%w: %q", ErrNotAllowed, remoteURL)
}

// Token reads the endpoint's credential file.  Read per call, never
// cached: the mounted file is the rotation point.
func (e Endpoint) Token() (string, error) {
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
