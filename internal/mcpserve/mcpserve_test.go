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

package mcpserve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func req(t *testing.T, args string) *mcp.CallToolRequest {
	t.Helper()
	r := &mcp.CallToolRequest{}
	r.Params = &mcp.CallToolParamsRaw{Arguments: json.RawMessage(args)}
	return r
}

func TestDecodeRejectsUnknownKeys(t *testing.T) {
	var v struct {
		Path string `json:"path"`
	}
	if err := Decode(req(t, `{"path": "a.txt", "pth": "b.txt"}`), &v); err == nil {
		t.Error("an unknown argument key must be an error, not a silent fall-through")
	}
	if err := Decode(req(t, `{"path": "a.txt"}`), &v); err != nil || v.Path != "a.txt" {
		t.Errorf("well-formed arguments failed: %v, %+v", err, v)
	}
}

func TestDecodeEmptyArgumentsIsZeroValue(t *testing.T) {
	var v struct {
		N int `json:"n"`
	}
	if err := Decode(req(t, ""), &v); err != nil || v.N != 0 {
		t.Errorf("empty arguments = %v, %+v; want the zero value", err, v)
	}
}

func TestNoExtrasRendersAdditionalPropertiesFalse(t *testing.T) {
	b, err := json.Marshal(NoExtras())
	if err != nil {
		t.Fatal(err)
	}
	// jsonschema-go renders the Not-of-empty-schema form as the boolean
	// false schema — so on the wire the client sees exactly
	// `"additionalProperties": false`.
	if s := string(b); s != "false" {
		t.Errorf("NoExtras marshals to %s, want false", s)
	}
}

func TestErrResultSetsIsError(t *testing.T) {
	r := ErrResult("nope")
	if !r.IsError {
		t.Error("ErrResult must set IsError")
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "nope" {
		t.Errorf("content = %#v, want the message text", r.Content[0])
	}
}

func TestHandlerServesHealthz(t *testing.T) {
	h := Handler(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

// TestHandlerAtRoutesEachPathToItsOwnRegistry proves the routing over
// real HTTP, which is what the archivist's two surfaces rest on: a tool
// registered on one server is reachable at its path and NOWHERE else.
// The in-memory transports the workers' own tests use bypass this mux
// entirely, so without this the split would be untested where it is
// actually served.
func TestHandlerAtRoutesEachPathToItsOwnRegistry(t *testing.T) {
	build := func(name, tool string) *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0"}, nil)
		mcp.AddTool(s, &mcp.Tool{Name: tool, Description: tool},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
				return JSONResult(map[string]any{"tool": tool}), nil, nil
			})
		return s
	}
	ts := httptest.NewServer(HandlerAt(map[string]*mcp.Server{
		"/mcp":          build("public", "greet"),
		"/operator/mcp": build("operator", "shutdown"),
	}))
	defer ts.Close()

	tools := func(path string) map[string]bool {
		t.Helper()
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		sess, err := client.Connect(context.Background(),
			&mcp.StreamableClientTransport{Endpoint: ts.URL + path}, nil)
		if err != nil {
			t.Fatalf("connect %s: %v", path, err)
		}
		defer sess.Close()
		res, err := sess.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListTools %s: %v", path, err)
		}
		got := map[string]bool{}
		for _, tool := range res.Tools {
			got[tool.Name] = true
		}
		// The other path's tool must not merely be unlisted — it must not
		// resolve.
		if _, err := sess.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "no-such-tool"}); err == nil {
			t.Errorf("%s resolved a tool it does not have", path)
		}
		return got
	}

	if got := tools("/mcp"); len(got) != 1 || !got["greet"] {
		t.Errorf("/mcp = %v, want just greet", got)
	}
	if got := tools("/operator/mcp"); len(got) != 1 || !got["shutdown"] {
		t.Errorf("/operator/mcp = %v, want just shutdown", got)
	}
}
