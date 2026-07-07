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
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/egress"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// stubApprover is a controllable ApprovalClient.  A zero value auto-approves
// everything; set `always` to force one decision, `byTool` to decide per op
// tool, or `neverResolve` to keep everything pending (for timeout tests).
type stubApprover struct {
	always       approval.Decision
	byTool       map[string]approval.Decision
	neverResolve bool
	ids          map[runid.ID]string
	registered   []runid.ID
	withdrawn    []runid.ID
}

func (a *stubApprover) RegisterPending(id runid.ID, tool, _ string) error {
	if a.ids == nil {
		a.ids = map[runid.ID]string{}
	}
	a.ids[id] = tool
	a.registered = append(a.registered, id)
	return nil
}

func (a *stubApprover) PollDecision(id runid.ID) (approval.Record, error) {
	if a.neverResolve {
		return approval.Record{OpID: id, Decision: approval.Pending}, nil
	}
	d := approval.Approved
	if a.always != "" {
		d = a.always
	} else if dd, ok := a.byTool[a.ids[id]]; ok {
		d = dd
	}
	return approval.Record{OpID: id, Decision: d}, nil
}

func (a *stubApprover) Withdraw(id runid.ID) error {
	a.withdrawn = append(a.withdrawn, id)
	return nil
}

func (a *stubApprover) registeredTools() []string {
	var out []string
	for _, id := range a.registered {
		out = append(out, a.ids[id])
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// --- query & answer gates (via the MCP surface) -----------------------------

func callResearch(t *testing.T, srv *Server, query string) *mcp.CallToolResult {
	t.Helper()
	res, err := connect(t, srv).CallTool(context.Background(), &mcp.CallToolParams{
		Name: "research", Arguments: map[string]any{"query": query},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestQueryGateRejectStopsBeforeLoop(t *testing.T) {
	model := &flowModel{}
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{}, &stubRetriever{}), Model: model,
		Approvals: &stubApprover{always: approval.Rejected}, Caps: DefaultCaps(),
	})
	res := callResearch(t, srv, "how do gradle toolchains work?")
	if !res.IsError {
		t.Fatal("a rejected query must be an error result")
	}
	if model.calls != 0 {
		t.Errorf("loop ran despite a rejected query (model called %d times)", model.calls)
	}
}

func TestQueryGateTimeoutWithdraws(t *testing.T) {
	caps := DefaultCaps()
	caps.QueryApproval = 100 * time.Millisecond
	appr := &stubApprover{neverResolve: true}
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{}, &stubRetriever{}), Model: &flowModel{},
		Approvals: appr, Caps: caps,
	})
	res := callResearch(t, srv, "q")
	if !res.IsError {
		t.Fatal("a timed-out query must be an error result")
	}
	if len(appr.withdrawn) != 1 {
		t.Errorf("timed-out gate withdrew %d records, want 1", len(appr.withdrawn))
	}
}

func TestAnswerGateOnAndOff(t *testing.T) {
	build := func(gate bool, appr *stubApprover) *Server {
		return New(Config{
			Egress: testEgress(t, &stubSearcher{hits: []egress.Hit{{URL: "https://a.example/1"}}}, &stubRetriever{md: "x"}),
			Model:  &flowModel{}, Approvals: appr, AnswerGate: gate, Caps: DefaultCaps(),
		})
	}
	// Answer gate ON, operator rejects the answer → error, and a research_answer op was registered.
	appr := &stubApprover{byTool: map[string]approval.Decision{"research_answer": approval.Rejected}}
	if res := callResearch(t, build(true, appr), "q"); !res.IsError {
		t.Error("answer gate on + reject: want an error result")
	}
	if !contains(appr.registeredTools(), "research_answer") {
		t.Errorf("answer gate on: no research_answer op registered (%v)", appr.registeredTools())
	}
	// Answer gate OFF → no research_answer op even though everything else runs.
	appr2 := &stubApprover{}
	if res := callResearch(t, build(false, appr2), "q"); res.IsError {
		t.Errorf("answer gate off: unexpected error %s", resultText(res))
	}
	if contains(appr2.registeredTools(), "research_answer") {
		t.Error("answer gate off: should not register a research_answer op")
	}
}

// --- raw-URL retrieval gate (via the loop directly) -------------------------

type rawURLModel struct{ calls int }

func (m *rawURLModel) Complete(_ context.Context, _ []Message, _ []Tool) (Message, int, error) {
	m.calls++
	switch m.calls {
	case 1:
		return toolCallMsg("web_search", `{"query":"x"}`), 1, nil
	case 2:
		return toolCallMsg("extract_url_as_markdown", `{"target":"https://elsewhere.example/page"}`), 1, nil
	default:
		return toolCallMsg("respond", `{"answer":"done"}`), 1, nil
	}
}

func TestRawURLExtractIsGated(t *testing.T) {
	r := &stubRetriever{md: "# raw page"}
	appr := &stubApprover{} // approves
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{hits: []egress.Hit{{URL: "https://a.example/1"}}}, r),
		Model:  &rawURLModel{}, Approvals: appr, Audit: &recAuditor{}, Caps: DefaultCaps(),
	})
	if _, err := srv.research(context.Background(), mustRunID(t), "q"); err != nil {
		t.Fatal(err)
	}
	if !contains(appr.registeredTools(), "extract_url") {
		t.Errorf("raw-URL extract registered %v, want an extract_url op", appr.registeredTools())
	}
	if r.calls != 1 {
		t.Errorf("approved raw URL: retriever called %d times, want 1", r.calls)
	}
}

func TestRawURLExtractRejectedContinues(t *testing.T) {
	r := &stubRetriever{md: "# raw page"}
	appr := &stubApprover{always: approval.Rejected}
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{hits: []egress.Hit{{URL: "https://a.example/1"}}}, r),
		Model:  &rawURLModel{}, Approvals: appr, Audit: &recAuditor{}, Caps: DefaultCaps(),
	})
	ans, err := srv.research(context.Background(), mustRunID(t), "q") // model still calls respond on turn 3
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 {
		t.Errorf("rejected raw URL: retriever called %d times, want 0", r.calls)
	}
	if ans.answer.Answer != "done" {
		t.Errorf("loop should continue to an answer after a rejected raw URL; got %q", ans.answer.Answer)
	}
}

func TestHandleExtractCreatesNoPending(t *testing.T) {
	appr := &stubApprover{}
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{hits: []egress.Hit{{URL: "https://a.example/1"}}}, &stubRetriever{md: "x"}),
		Model:  &flowModel{}, Approvals: appr, Audit: &recAuditor{}, Caps: DefaultCaps(),
	})
	if _, err := srv.research(context.Background(), mustRunID(t), "q"); err != nil {
		t.Fatal(err)
	}
	if len(appr.registered) != 0 {
		t.Errorf("a handle extract created %d pending records, want 0 (only raw URLs gate)", len(appr.registered))
	}
}

var _ egress.Searcher = (*stubSearcher)(nil) // compile-time: stubs satisfy the egress seams
var _ egress.Retriever = (*stubRetriever)(nil)
