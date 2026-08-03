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

// Package operator is the client for the archivist's OPERATOR surface
// (docs/archivist.md, "Two surfaces"): the workspace's boundary events,
// which the coding agent is deliberately not connected to.  The
// workbench session manager (cmd/workbench) is its only caller.
//
// The verbs return typed results rather than the raw MCP envelope, and
// a tool-level refusal — a gate violation, unpublished work, a corrupt
// workspace — arrives as a *RefusedError, not as a success carrying an
// error string.  The distinction matters at the call site: a refusal is
// something to show the operator and offer a next move for, while a
// transport failure means the archivist is unreachable.
package operator

import (
	"context"
	"fmt"
	"time"

	"github.com/jeffbstewart/cloister/internal/mcpclient"
)

// State is the workspace's disk-derived condition, mirroring
// archive.LifecycleState across the wire.
type State string

const (
	StateEmpty       State = "empty"
	StateProvisioned State = "provisioned"
	StateCorrupt     State = "corrupt"
)

// Status is what workspace_state answers.
type Status struct {
	State  State  `json:"state"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// ProvisionedAt is the marker's write time, epoch seconds.
	ProvisionedAt int64 `json:"provisioned_at"`
}

// Provisioned reports the provenance in one line for display.
func (s Status) Provisioned() string {
	if s.Branch == "" {
		return s.Repo
	}
	return s.Repo + " on " + s.Branch
}

// RefusedError is a tool-level refusal — the archivist answered, and
// the answer was no.  Aliased from the shared client core so callers
// can errors.As against either name and mean the same thing.
type RefusedError = mcpclient.RefusedError

// Client speaks to one archivist's operator surface.
type Client struct {
	c *mcpclient.Client
}

// Config wires a Client.
type Config struct {
	// URL is the operator surface, e.g.
	// http://archivist:9600/operator/mcp.
	URL string
	// Name identifies this client in the MCP handshake; it shows up in
	// the archivist's logs, so make it say who is calling.
	Name string
	// Version is this client's version, for the same handshake.
	Version string
}

// Dial connects to the operator surface.  The caller closes the Client.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("operator: no archivist URL")
	}
	if cfg.Name == "" {
		cfg.Name = "cloister-operator"
	}
	c, err := mcpclient.Dial(ctx, mcpclient.Config{URL: cfg.URL, Name: cfg.Name, Version: cfg.Version})
	if err != nil {
		return nil, fmt.Errorf("operator: %w", err)
	}
	return &Client{c: c}, nil
}

// Close ends the session.
func (c *Client) Close() error { return c.c.Close() }

// Status reports the workspace's condition.  Never acts.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.call(ctx, "workspace_state", nil, &st)
	return st, err
}

// ProvisionResult is what a successful provision reports.
type ProvisionResult struct {
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Endpoint string `json:"endpoint"`
}

// Provision brings an EMPTY workspace into being.  branch may be empty
// (the default branch); a refusal — an unknown host, a forge that does
// not meet grange service, a non-empty workspace — is a *RefusedError.
//
// The clone runs through the relays under the gate, so this is slow by
// nature: give it a context with minutes, not seconds.
func (c *Client) Provision(ctx context.Context, repo, branch string) (ProvisionResult, error) {
	args := map[string]any{"repo": repo}
	if branch != "" {
		args["branch"] = branch
	}
	var res ProvisionResult
	err := c.call(ctx, "provision", args, &res)
	return res, err
}

// DisposeResult is what dispose reports.
type DisposeResult struct {
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	Disposed     bool   `json:"disposed"`
	AlreadyEmpty bool   `json:"already_empty"`
}

// Dispose returns the workspace to EMPTY.  Without force it refuses
// while unpublished work exists; force discards it.  A workspace with
// no provenance marker is refused at ANY force — that rail is what
// keeps dispose structurally unable to wipe a host tree, so a
// *RefusedError from a corrupt workspace is not something to retry
// harder.
func (c *Client) Dispose(ctx context.Context, force bool) (DisposeResult, error) {
	args := map[string]any{}
	if force {
		args["force"] = true
	}
	var res DisposeResult
	err := c.call(ctx, "dispose", args, &res)
	return res, err
}

// call invokes one tool and unmarshals its JSON answer.
func (c *Client) call(ctx context.Context, name string, args map[string]any, out any) error {
	return c.c.Call(ctx, name, args, out)
}

// ProvisionTimeout bounds a provision: a full clone through the relays
// plus the forge-lint gate.  Generous — a big repository over a
// two-hop relay is minutes — but bounded, so a wedged relay surfaces as
// a timeout the operator can act on instead of a session that never
// returns.
const ProvisionTimeout = 15 * time.Minute

// DisposeTimeout bounds a dispose: local filesystem work plus the
// unpublished-work check, which asks the endpoint whether checkpoints
// landed.
const DisposeTimeout = 2 * time.Minute
