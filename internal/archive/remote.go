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

package archive

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forge"
)

// ErrNoEndpoints reports a remote verb invoked on an Archive with no
// endpoint table: local-only mode (tests, hand-built checkouts) has no
// credentials and no allowlist, so it has no remote verbs.
var ErrNoEndpoints = errors.New("archive: no endpoint table configured; remote verbs are unavailable")

// WithEndpoints installs the endpoint table (docs/archivist.md,
// "Endpoints, identity, and credential").  The table is the remote
// allowlist and the credential source: without it every remote verb
// refuses, and fetches run bare (the local test rigs' mode).
func WithEndpoints(t *endpoint.Table) Option {
	return func(c *config) { c.endpoints = t }
}

// remoteContext resolves the workspace's origin URL against the
// endpoint table and builds the per-invocation overlay.  It runs per
// verb, against the CURRENT origin URL — the repo-local config is
// agent-writable post-M3, so the allowlist must hold even if the
// remote was rewritten between calls.
func (a *Archive) remoteContext(ctx context.Context) (*endpoint.Endpoint, overlay, error) {
	if a.table == nil {
		return nil, overlay{}, ErrNoEndpoints
	}
	url, err := a.run.out(ctx, "remote", "get-url", originRemote)
	if err != nil {
		return nil, overlay{}, err
	}
	ep, err := a.table.ForRemote(url)
	if err != nil {
		return nil, overlay{}, err
	}
	tok, err := ep.Token()
	if err != nil {
		return nil, overlay{}, err
	}
	o := overlay{env: authEnv(tok)}
	if ep.Wire != ep.Canonical {
		// The wire mapping rides per invocation; guardConfig refuses
		// url..insteadOf in the repo-local config, so this -c is the only
		// place designations get rewritten.
		o.cfg = []string{"-c", "url." + ep.Wire + ".insteadOf=" + ep.Canonical}
	}
	return ep, o, nil
}

// authEnv carries the endpoint's credential into one git invocation as
// configuration via the environment (GIT_CONFIG_COUNT/KEY/VALUE) —
// never argv, which is world-readable in /proc.  The Basic form with
// the x-access-token user is what GitHub's smart-HTTP endpoint expects
// for token auth; Gitea accepts the same shape.
func authEnv(token string) []string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

// fetchOverlay is the overlay for fetch-only verbs: with a table, the
// full remote context (allowlist + credential); without one, bare —
// the local rigs fetch from path remotes no table could ever admit.
func (a *Archive) fetchOverlay(ctx context.Context) (overlay, error) {
	if a.table == nil {
		return overlay{}, nil
	}
	_, o, err := a.remoteContext(ctx)
	return o, err
}

// RemoteInfo resolves the workspace's endpoint and its "owner/name"
// repository from the CURRENT origin URL — the wiring the forge verbs
// need, re-resolved per call for the same reason remoteContext is.
func (a *Archive) RemoteInfo(ctx context.Context) (*endpoint.Endpoint, string, error) {
	if a.table == nil {
		return nil, "", ErrNoEndpoints
	}
	url, err := a.run.out(ctx, "remote", "get-url", originRemote)
	if err != nil {
		return nil, "", err
	}
	ep, err := a.table.ForRemote(url)
	if err != nil {
		return nil, "", err
	}
	repo, err := forge.RepoFromRemote(ep.Canonical, url)
	if err != nil {
		// A provisioned clone may carry the wire form instead.
		repo, err = forge.RepoFromRemote(ep.Wire, url)
	}
	if err != nil {
		return nil, "", err
	}
	return ep, repo, nil
}

// PublishInfo says what Publish did.
type PublishInfo struct {
	Branch   string
	Endpoint string // the endpoint name the branch went to
}

// Publish pushes the current line of work to its endpoint and records
// the upstream, flipping the branch to published (which is what turns
// restore and sync from history rewrite to forward motion).
//
// Client-side refusals, belt-and-braces under the forge's ruleset: the
// default branch is never pushed (its only motion is
// sync_from_upstream), a detached HEAD has nothing to publish, and
// force-push and tag deletion are structurally absent — no argument to
// this verb set can express them.
func (a *Archive) Publish(ctx context.Context) (PublishInfo, error) {
	if err := a.guardConfig(ctx); err != nil {
		return PublishInfo{}, err
	}
	branch, err := a.currentBranch(ctx)
	if err != nil {
		return PublishInfo{}, err
	}
	if branch == "" {
		return PublishInfo{}, fmt.Errorf("archive: publish: not on a branch")
	}
	if branch == a.def.String() {
		return PublishInfo{}, fmt.Errorf("%w: publish", ErrDefaultBranch)
	}
	ep, o, err := a.remoteContext(ctx)
	if err != nil {
		return PublishInfo{}, err
	}
	if _, err := a.run.outWith(ctx, o, "push", "-u", originRemote, branch); err != nil {
		return PublishInfo{}, err
	}
	return PublishInfo{Branch: branch, Endpoint: ep.Name}, nil
}

// deleteRemoteBranch removes name's published counterpart — the remote
// half of abandon_work.  A branch that was never published has nothing
// to delete, and saying so is not an error.
func (a *Archive) deleteRemoteBranch(ctx context.Context, name BranchName) error {
	if name.String() == a.def.String() {
		return fmt.Errorf("%w: delete the remote default branch", ErrDefaultBranch)
	}
	_, o, err := a.remoteContext(ctx)
	if err != nil {
		return err
	}
	_, err = a.run.outWith(ctx, o, "push", originRemote, "--delete", name.String())
	return err
}
