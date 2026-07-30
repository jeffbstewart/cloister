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
