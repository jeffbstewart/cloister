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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/verbs"
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

// RefusedError is a tool-level refusal: the archivist answered, and the
// answer was no.  The message is the archivist's own — it names the
// failing requirement or the unpublished work, and is meant to be shown
// to the operator verbatim rather than paraphrased.
//
// Keeping this distinct from a transport failure is the point: "the
// archivist said no, and here is why" and "the archivist is
// unreachable" call for opposite responses, and a client that flattened
// both into error would push that judgement onto every call site.
type RefusedError struct {
	Verb    string
	Message string
}

func (e *RefusedError) Error() string { return e.Verb + ": " + e.Message }

// Client speaks to one archivist's operator surface.
type Client struct {
	sess *mcp.ClientSession
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
		return nil, errors.New("operator: no archivist URL")
	}
	if cfg.Name == "" {
		cfg.Name = "cloister-operator"
	}
	c := mcp.NewClient(&mcp.Implementation{Name: cfg.Name, Version: cfg.Version}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: cfg.URL}, nil)
	if err != nil {
		return nil, fmt.Errorf("operator: dialing %s: %w", cfg.URL, err)
	}
	return &Client{sess: sess}, nil
}

// Close ends the session.
func (c *Client) Close() error { return c.sess.Close() }

// Status reports the workspace's condition.  Never acts.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.call(ctx, verbs.WorkspaceState, nil, &st)
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
	err := c.call(ctx, verbs.Provision, args, &res)
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
	err := c.call(ctx, verbs.Dispose, args, &res)
	return res, err
}

// call invokes one tool and unmarshals its JSON answer, turning a
// tool-level error into a *RefusedError.  Every verb on this surface
// answers JSON; a verb that answered prose would surface here as a
// decode error, which is why the operator surface keeps to one shape.
func (c *Client) call(ctx context.Context, name string, args map[string]any, out any) error {
	res, err := c.sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return fmt.Errorf("operator: calling %s: %w", name, err)
	}
	text, ok := firstText(res)
	if res.IsError {
		if !ok {
			// A refusal with nothing in it would print as "refused:"
			// followed by a blank line, which tells the reader nothing
			// about what to do next.
			return &RefusedError{Verb: name, Message: "refused, with no reason given"}
		}
		return &RefusedError{Verb: name, Message: text}
	}
	if out == nil {
		return nil
	}
	if !ok {
		// Distinct from a malformed payload: the difference between "the
		// archivist sent nothing" and "the archivist sent something I
		// cannot read" is the difference between a broken surface and a
		// broken contract.
		return fmt.Errorf("operator: %s returned no text content", name)
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("operator: unparseable %s answer %q: %w", name, text, err)
	}
	return nil
}

// firstText reports the result's text content, and whether there was
// any.
func firstText(res *mcp.CallToolResult) (string, bool) {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text, true
		}
	}
	return "", false
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
