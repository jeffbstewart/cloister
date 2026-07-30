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
	"time"

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/archivist"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forge"
	"github.com/jeffbstewart/cloister/internal/status/sink"
)

// archivistRole parses the archivist's flag set and returns its bootstrap.
func archivistRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("archivist", flag.ContinueOnError)
	common := registerCommon(fs, ":9600")
	workspace := fs.String("workspace", "/workspace", "the provisioned checkout the archivist drives")
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
			Addr: *common.addr, Workspace: *workspace, Endpoints: *endpoints,
			StateURL: *stateURL, DefaultBranch: *defBranch,
		})
	}), nil
}

// archivistOptions carries the archivist's bootstrap inputs.
type archivistOptions struct {
	Addr          string
	Workspace     string
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

	opts := []archive.Option{archive.WithEndpoints(table)}
	if o.DefaultBranch != "" {
		opts = append(opts, archive.WithDefaultBranch(o.DefaultBranch))
	}
	// Identity is resolved from the table by the engine; the argument
	// here is deliberately zero.
	a, err := archive.New(o.Workspace, archive.Identity{}, opts...)
	if err != nil {
		log.Fatalf("archivist: %v", err)
	}

	srv := archivist.New(archivist.Config{
		Version: version,
		Archive: a,
		Forge:   buildForge(a.Endpoint()),
		Audit:   sink.NewClient(o.StateURL, token),
	})
	// Close before any fatal exit, not deferred past one: log.Fatalf
	// skips defers, and a crash-looping container would leak one hooks
	// dir per restart.
	serveErr := serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("archivist (workspace %s, endpoint %s, default branch %s)",
			o.Workspace, a.Endpoint().Name, a.DefaultBranch()))
	a.Close()
	if serveErr != nil {
		log.Fatalf("serve: %v", serveErr)
	}
}

// buildForge wires the PR-verb client for the workspace's endpoint.
// The transport is a guarded client that can dial ONLY the forge's API
// host — in the cell that name is a network alias on the api relay, so
// the bytes ride the jail while TLS verifies end-to-end — and a stray
// HTTP(S)_PROXY cannot tunnel around it.
func buildForge(ep *endpoint.Endpoint) forge.Client {
	u, err := url.Parse(ep.API)
	if err != nil || u.Host == "" {
		log.Fatalf("archivist: endpoint %s api %q is not a URL", ep.Name, ep.API)
	}
	host := u.Host
	if u.Port() == "" {
		port := "443"
		if u.Scheme == "http" {
			port = "80"
		}
		host = u.Host + ":" + port
	}
	client, err := wire.NewGuardedClient(map[string]string{u.Hostname(): host}, 60*time.Second)
	if err != nil {
		log.Fatalf("archivist: building the forge transport: %v", err)
	}
	switch ep.Forge {
	case endpoint.ForgeGitHub:
		g, err := forge.NewGitHub(ep.API, ep.Token, client)
		if err != nil {
			log.Fatalf("archivist: %v", err)
		}
		return g
	default:
		// The Gitea adapter follows the pilot (docs/archivist.md); until
		// then a gitea endpoint serves the git verbs but has no PR verbs.
		log.Printf("archivist: endpoint %s forge %q has no PR-verb adapter yet; propose/check_progress/read_reviews/reply_to_review are not served", ep.Name, ep.Forge)
		return nil
	}
}
