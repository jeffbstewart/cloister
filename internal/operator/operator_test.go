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

package operator

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/mcpserve"
)

// stub serves canned answers on the operator path, so the client's own
// behavior — decoding, and the refusal/transport split — is what's under
// test.  The wire contract against the REAL archivist is pinned by
// internal/archivist's own operator-client test.
func stub(t *testing.T, answers map[string]func() *mcp.CallToolResult) *Client {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "stub", Version: "0"}, nil)
	for name, answer := range answers {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: name},
			func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
				return answer(), nil, nil
			})
	}
	ts := httptest.NewServer(mcpserve.HandlerAt(map[string]*mcp.Server{"/operator/mcp": srv}))
	t.Cleanup(ts.Close)

	c, err := Dial(context.Background(), Config{URL: ts.URL + "/operator/mcp", Name: "test", Version: "0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func answer(text string) func() *mcp.CallToolResult {
	return func() *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
	}
}

func refuse(text string) func() *mcp.CallToolResult {
	return func() *mcp.CallToolResult {
		r := answer(text)()
		r.IsError = true
		return r
	}
}

func TestStatusDecodesEachCondition(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Status
	}{
		{"empty", `{"state":"empty"}`, Status{State: StateEmpty}},
		{"corrupt", `{"state":"corrupt"}`, Status{State: StateCorrupt}},
		{"provisioned", `{"state":"provisioned","repo":"op/repo","branch":"agent/brisk-otter","provisioned_at":1753000000}`,
			Status{State: StateProvisioned, Repo: "op/repo", Branch: "agent/brisk-otter", ProvisionedAt: 1753000000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := stub(t, map[string]func() *mcp.CallToolResult{"workspace_state": answer(tc.body)})
			got, err := c.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Status = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProvisionedLine(t *testing.T) {
	if got := (Status{Repo: "op/repo"}).Provisioned(); got != "op/repo" {
		t.Errorf("default-branch line = %q, want just the repo", got)
	}
	if got := (Status{Repo: "op/repo", Branch: "agent/x"}).Provisioned(); got != "op/repo on agent/x" {
		t.Errorf("line = %q", got)
	}
}

// TestRefusalIsTypedNotSwallowed: the archivist's refusals arrive as
// tool-level errors carrying the reason.  They must surface as
// *RefusedError with the message intact — a caller that showed
// "provision failed" instead of the archivist's own "R2: stale
// approvals survive" would strip exactly the part the operator needs.
func TestRefusalIsTypedNotSwallowed(t *testing.T) {
	c := stub(t, map[string]func() *mcp.CallToolResult{
		"provision": refuse("archivist: op/repo fails R2: stale approvals survive"),
	})
	_, err := c.Provision(context.Background(), "https://github.com/op/repo", "")
	var ref *RefusedError
	if !errors.As(err, &ref) {
		t.Fatalf("provision refusal = %v (%T), want a *RefusedError", err, err)
	}
	if ref.Verb != "provision" || ref.Message != "archivist: op/repo fails R2: stale approvals survive" {
		t.Errorf("refusal = %+v, want the archivist's own message verbatim", ref)
	}
}

// TestUnknownToolIsNotARefusal: a verb the surface does not have fails
// at the protocol level, and must NOT be reported as the archivist
// refusing — that would read as "the workspace said no" when the truth
// is "this archivist has no such verb".
func TestUnknownToolIsNotARefusal(t *testing.T) {
	c := stub(t, map[string]func() *mcp.CallToolResult{"workspace_state": answer(`{"state":"empty"}`)})
	_, err := c.Dispose(context.Background(), false)
	var ref *RefusedError
	if err == nil || errors.As(err, &ref) {
		t.Fatalf("missing verb = %v, want a transport-level error, not a refusal", err)
	}
}

func TestDisposeReportsAlreadyEmpty(t *testing.T) {
	c := stub(t, map[string]func() *mcp.CallToolResult{
		"dispose": answer(`{"disposed":false,"already_empty":true}`),
	})
	res, err := c.Dispose(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Disposed || !res.AlreadyEmpty {
		t.Errorf("dispose of an empty workspace = %+v, want a no-op, not an error", res)
	}
}
