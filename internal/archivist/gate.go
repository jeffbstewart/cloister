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

package archivist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forgelint"
)

// ErrNotGrangeReady reports a repository the archivist refuses to provision
// because it is not set up for grange service — an unsupported forge, or a
// forge-lint config that is missing, malformed, names another repository,
// or points at a different API host.  It is a refusal (the fix is the
// lock-down runbook), distinct from the forge being unreadable (an infra
// error) or its protections failing (a GateRefusedError).
var ErrNotGrangeReady = errors.New("archivist: repository is not set up for grange service")

// ForgeGate is the provision gate (docs/grange.md, "Provision-time
// verification"): it reads the repository's OWN forge-lint config from the
// freshly-cloned staging tree — the operator-approved default-branch copy,
// a real file read by the strict loader — and runs forgelint's
// bot-credential check against the live forge.  It is archive's
// ProvisionGate: provision calls Verify and refuses the grange on a
// non-nil error.
//
// GitHub is M1's only backend.  A non-github endpoint is refused
// fail-closed rather than passed silently — the gitea gate follows the
// pilot (grange.md), and until then a gitea grange leans on the
// archivist's client-side discipline, not on this gate pretending to have
// checked.
type ForgeGate struct {
	// Dial builds the guarded HTTP client for an endpoint's API: it dials
	// the relay's cell address while TLS still verifies the real API host's
	// certificate.  Injected so tests point it at an httptest server.
	Dial func(apiBase, apiRelay string) (*http.Client, error)
}

// forgeLintPath is where a grange-ready repository pins its expectations,
// governed by the operator via CODEOWNERS (the lock-down runbook).
const forgeLintPath = ".github/forge-lint.yaml"

// Verify implements archive.ProvisionGate, returning the repository's
// declared agent-branch namespace so branch creation can be refused
// locally rather than by the forge three steps later.
func (g ForgeGate) Verify(ctx context.Context, ep endpoint.Endpoint, repo, stagingTree string) (string, error) {
	if ep.Forge != endpoint.ForgeGitHub {
		return "", fmt.Errorf("archivist: the provision gate supports only github endpoints in M1; endpoint %s is %q: %w", ep.Name, ep.Forge, ErrNotGrangeReady)
	}
	cfgPath := filepath.Join(stagingTree, filepath.FromSlash(forgeLintPath))
	cfg, err := forgelint.LoadConfig(cfgPath)
	if err != nil {
		return "", fmt.Errorf("archivist: provision gate: %v — bring the repo up to %s: %w", err, forgelint.HardeningRunbook, ErrNotGrangeReady)
	}
	// The pinned config must govern the repository it ships in, and name
	// the API host TLS will verify — otherwise a repo could carry another
	// project's (weaker) expectations.
	if cfg.Repo != repo {
		return "", fmt.Errorf("archivist: provision gate: %s pins repo %q but the workspace is %q: %w", forgeLintPath, cfg.Repo, repo, ErrNotGrangeReady)
	}
	if err := sameAPIHost(cfg.APIBase, ep.API); err != nil {
		return "", fmt.Errorf("archivist: provision gate: %v: %w", err, ErrNotGrangeReady)
	}
	token, err := ep.Token()
	if err != nil {
		return "", fmt.Errorf("archivist: provision gate: reading the endpoint credential: %w", err)
	}
	client, err := g.Dial(cfg.APIBase, ep.APIRelay)
	if err != nil {
		return "", fmt.Errorf("archivist: provision gate: building the forge transport: %w", err)
	}
	snap, err := forgelint.NewGitHub(cfg, token, client).ProvisionSnapshot(ctx)
	if err != nil {
		return "", fmt.Errorf("archivist: provision gate: reading %s protections: %w", repo, err)
	}
	if r := forgelint.Gate(forgelint.Check(cfg, snap)); !r.OK {
		return "", &GateRefusedError{Repo: repo, Blocking: r.Blocking}
	}
	// The repo's own declared namespace travels back so branch creation
	// can be refused here rather than by the forge, three verbs later.
	return cfg.AgentNamespace, nil
}

// sameAPIHost checks that the forge-lint config's apiBase and the
// endpoint's api name the same host — the host whose certificate the
// guarded transport verifies.
func sameAPIHost(configAPI, endpointAPI string) error {
	ca, err := url.Parse(configAPI)
	if err != nil || ca.Host == "" {
		return fmt.Errorf("forge-lint apiBase %q is not a URL", configAPI)
	}
	ea, err := url.Parse(endpointAPI)
	if err != nil || ea.Host == "" {
		return fmt.Errorf("endpoint api %q is not a URL", endpointAPI)
	}
	if !strings.EqualFold(ca.Host, ea.Host) {
		return fmt.Errorf("forge-lint apiBase host %q does not match the endpoint's api host %q", ca.Host, ea.Host)
	}
	return nil
}

// GateRefusedError reports a provision refused because the repository's
// forge protections do not meet grange service (as opposed to the config
// being missing or the forge unreadable).  Its first requirement names the
// failure in the lifecycle audit record.
type GateRefusedError struct {
	Repo     string
	Blocking []forgelint.Verdict
}

func (e *GateRefusedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "provision refused: %s does not meet the forge protections grange service requires:", e.Repo)
	for _, v := range e.Blocking {
		fmt.Fprintf(&b, "\n  - %s: %s", v.Req, v.Detail)
	}
	fmt.Fprintf(&b, "\nbring it up to spec: %s", forgelint.HardeningRunbook)
	return b.String()
}

// Requirement names the first failing requirement, for the audit record.
func (e *GateRefusedError) Requirement() string {
	if len(e.Blocking) > 0 {
		return e.Blocking[0].Req
	}
	return ""
}
