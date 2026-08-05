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

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/archivist"
	"github.com/jeffbstewart/cloister/internal/disclosure"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forge"
	"github.com/jeffbstewart/cloister/internal/status/sink"
)

// archivistRole parses the archivist's flag set and returns its bootstrap.
func archivistRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("archivist", flag.ContinueOnError)
	common := registerCommon(fs, ":9600")
	grangeRoot := fs.String("grange", envOr("GRANGE_ROOT", "/grange"),
		"the grange volume root; the archivist manages tree/ (the exported checkout) and staging/ under it")
	endpoints := fs.String("endpoints", envOr("ENDPOINTS_FILE", "/etc/cloister/endpoints.yaml"),
		"the endpoint table (read-only mount): identities, credential paths, and the remote allowlist")
	stateURL := fs.String("state-url", envOr("STATE_URL", ""), "base URL of the state service (remote ops audit there)")
	defBranch := fs.String("default-branch", "",
		"override default-branch detection (hand-built checkouts only; a grange clone's origin/HEAD is authoritative)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return common.runOrProbe(func() {
		runArchivist(archivistOptions{
			Addr: *common.addr, GrangeRoot: *grangeRoot, Endpoints: *endpoints,
			StateURL: *stateURL, DefaultBranch: *defBranch,
		})
	}), nil
}

// archivistOptions carries the archivist's bootstrap inputs.
type archivistOptions struct {
	Addr          string
	GrangeRoot    string
	Endpoints     string
	StateURL      string
	DefaultBranch string
}

func runArchivist(o archivistOptions) {
	// The endpoint table is the instance binding (docs/archivist.md):
	// identity, credentials, and the remote allowlist all follow from
	// it, so there is no archivist without one.
	table, err := endpoint.Load(o.Endpoints)
	if err != nil {
		log.Fatalf("archivist: %v", err)
	}
	token := envOr("STATE_TOKEN", "")
	if o.StateURL == "" || token == "" {
		log.Fatalf("archivist: STATE_URL and STATE_TOKEN are required: remote operations are audited")
	}

	// The grange owns the workspace lifecycle: provision brings a checkout
	// into being (there is none at boot — an EMPTY volume is the norm), so
	// the archivist no longer opens one here.  Identity, credentials, and
	// the allowlist all derive from the table.
	grange, err := archive.NewGrange(archive.GrangeConfig{
		Root:  o.GrangeRoot,
		Table: table,
		Gate:  provisionGate(),
		// The per-repository disclosure acknowledgment
		// (docs/JAILED_CLAUDE.md).  Inert unless the cell is armed —
		// CLOISTER_DISCLOSURE_REQUIRED names where source would go, and
		// only docker/cell-claude.yaml sets it.  The archivist is where
		// this belongs because it is the one component that
		// authoritatively knows the repository (it does the clone), and
		// provision is the once-per-workspace moment.
		Disclosure: func(repo string) error {
			return disclosure.Check(repo, os.LookupEnv)
		},
		OpenForge:     func(ep endpoint.Endpoint) (forge.Client, error) { return buildForge(&ep) },
		DefaultBranch: o.DefaultBranch,
	})
	if err != nil {
		log.Fatalf("archivist: %v", err)
	}

	// A clone can run for minutes and await_review blocks for up to an
	// hour, so this role drains slowly — kept under the compose
	// stop_grace_period, with await_review winding itself up on the
	// drain signal rather than being waited out.
	drainTimeout = 4 * time.Minute
	srv := archivist.New(archivist.Config{
		Version:  version,
		Grange:   grange,
		Audit:    sink.NewClient(sink.ClientConfig{BaseURL: o.StateURL, Token: token, Origin: auditOrigin()}),
		Draining: Draining(),
	})
	// Close before any fatal exit, not deferred past one: log.Fatalf skips
	// defers, and a crash-looping container would leak a hooks dir per
	// restart.
	serveErr := serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("archivist (grange %s)", o.GrangeRoot))
	grange.Close()
	if serveErr != nil {
		log.Fatalf("serve: %v", serveErr)
	}
}

// provisionGate builds the forge-lint provision gate, dialing each
// endpoint's API through its relay the same way buildForge does for the PR
// verbs (the relay address holds the connection; TLS still verifies the
// real API host).
func provisionGate() archivist.ForgeGate {
	return archivist.ForgeGate{
		Dial: func(apiBase, apiRelay string) (*http.Client, error) {
			u, err := url.Parse(apiBase)
			if err != nil || u.Hostname() == "" {
				return nil, fmt.Errorf("api %q is not a URL", apiBase)
			}
			return wire.NewGuardedClient(map[string]string{u.Hostname(): apiRelay}, forgeTimeout)
		},
	}
}

// forgeTimeout bounds a single forge API call — long enough for a slow
// PR fetch, short enough that a stuck relay does not wedge the verb.
const forgeTimeout = 60 * time.Second

// buildForge wires the PR-verb client for the workspace's endpoint.
// The transport is a guarded client that dials ONLY the api relay (by
// its cell address) while TLS verifies the real API host's certificate:
// the bytes ride the jail, the relay never sees plaintext, and a stray
// HTTP(S)_PROXY cannot tunnel around it.
func buildForge(ep *endpoint.Endpoint) (forge.Client, error) {
	u, err := url.Parse(ep.API)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("endpoint %s api %q is not a URL", ep.Name, ep.API)
	}
	// The guarded client keys on the request host (the real API name, so
	// SNI and cert verification are unchanged) and redirects the dial to
	// the relay address.
	client, err := wire.NewGuardedClient(map[string]string{u.Hostname(): ep.APIRelay}, forgeTimeout)
	if err != nil {
		return nil, fmt.Errorf("building the forge transport: %w", err)
	}
	switch ep.Forge {
	case endpoint.ForgeGitHub:
		return forge.NewGitHub(ep.API, ep.Token, client)
	default:
		// The Gitea adapter follows the pilot (docs/archivist.md); until
		// then a gitea endpoint serves the git verbs but has no PR verbs.
		log.Printf("archivist: endpoint %s forge %q has no PR-verb adapter yet; propose/check_progress/read_reviews/reply_to_review are not served", ep.Name, ep.Forge)
		return nil, nil
	}
}
