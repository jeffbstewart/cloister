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

package scholar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/egress"
)

func connect(t *testing.T, srv *Server) *mcp.ClientSession {
	t.Helper()
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.mcp.Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func resultText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// TestResearchOverMCP is the end-to-end acceptance: a real MCP client calls the
// research tool over the in-memory transport; a stub model drives a
// search→extract→respond flow; the answer + consulted sources come back and the
// research call is audited "answered".
func TestResearchOverMCP(t *testing.T) {
	s := &stubSearcher{hits: []egress.Hit{{Title: "T", URL: "https://a.example/1", Snippet: "S"}}}
	r := &stubRetriever{md: "# content"}
	aud := &recAuditor{}
	srv := New(Config{Version: "test", Egress: testEgress(t, s, r), Model: &flowModel{},
		Audit: aud, Approvals: &stubApprover{}, Caps: DefaultCaps()}) // approver auto-approves; AnswerGate off
	session := connect(t, srv)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "research",
		Arguments: map[string]any{"query": "how do gradle toolchains work?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("research errored: %s", resultText(res))
	}
	var ans Answer
	if err := json.Unmarshal([]byte(resultText(res)), &ans); err != nil {
		t.Fatalf("decode answer: %v (%s)", err, resultText(res))
	}
	if !strings.Contains(ans.Answer, "toolchains") || len(ans.Sources) != 1 {
		t.Errorf("answer = %+v", ans)
	}
	// Query gate registers pending, then the call resolves answered.
	if got := aud.decisions("research"); len(got) != 2 || got[0] != decPending || got[1] != decAnswered {
		t.Errorf("research audit = %v, want [pending_approval answered]", got)
	}
}

func TestResearchRejectsEmptyQuery(t *testing.T) {
	srv := New(Config{Egress: testEgress(t, &stubSearcher{}, &stubRetriever{}), Model: &flowModel{}, Caps: DefaultCaps()})
	session := connect(t, srv)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "research", Arguments: map[string]any{"query": "   "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("empty query should be an error result")
	}
}

func TestHealthz(t *testing.T) {
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{}, &stubRetriever{}),
		Model:  &scriptModel{replies: []Message{toolCallMsg("respond", `{"answer":"x"}`)}},
		Caps:   DefaultCaps(),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
}

// TestNoArbitraryEgress is the structural guard: the scholar's only
// outbound HTTP is the model client (model.go).  No file constructs bare HTTP,
// and only model.go builds an http.Client.
func TestNoArbitraryEgress(t *testing.T) {
	banned := []string{"http.DefaultClient", "http.Get(", "http.Post(", "http.Transport{"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range banned {
			if strings.Contains(string(src), b) {
				t.Errorf("%s contains %q — the scholar's only outbound HTTP is the model client", name, b)
			}
		}
		if name != "model.go" && strings.Contains(string(src), "http.Client{") {
			t.Errorf("%s constructs an http.Client; only model.go (the model endpoint) may", name)
		}
	}
}
