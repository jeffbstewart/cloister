package scholar

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/egress"
	"github.com/jeffbstewart/cloister/internal/egress/policy"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// --- shared scaffolding -----------------------------------------------------

func mustRunID(t *testing.T) runid.ID {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatalf("runid.New() failed: %v", err)
	}
	return id
}

type stubSearcher struct {
	hits  []egress.Hit
	err   error
	calls int
}

func (s *stubSearcher) Name() string { return "kagi" }
func (s *stubSearcher) Search(_ context.Context, _ string, _ int) ([]egress.Hit, error) {
	s.calls++
	return s.hits, s.err
}

type stubRetriever struct {
	md    string
	calls int
}

func (r *stubRetriever) Name() string { return "kagi" }
func (r *stubRetriever) Fetch(_ context.Context, u string) (egress.Extracted, error) {
	r.calls++
	return egress.Extracted{Markdown: r.md, FinalURL: u}, nil
}

type recAuditor struct{ recs []audit.Record }

func (a *recAuditor) Append(r audit.Record) error { a.recs = append(a.recs, r); return nil }
func (a *recAuditor) decisions(tool string) []audit.Decision {
	var out []audit.Decision
	for _, r := range a.recs {
		if r.Tool == tool {
			out = append(out, r.Decision)
		}
	}
	return out
}

func testEgress(t *testing.T, s egress.Searcher, r egress.Retriever) *egress.Subsystem {
	t.Helper()
	on := true
	p := &policy.Policy{}
	p.Search.Engine = policy.EngineKagi
	p.Search.DailyCap = 100
	p.Search.DenySearchEnginePages = &on
	p.Extract.DailyCap = 100
	p.Extract.Deny = []policy.DenyEntry{{Host: "pastebin.com"}}
	p.Limits.MaxResponseBytes = 1 << 20
	p.Limits.Timeout = policy.Duration(10 * time.Second)
	dir := t.TempDir()
	now := time.Now()
	sl, err := egress.OpenLedger(filepath.Join(dir, "s"), 48*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	el, err := egress.OpenLedger(filepath.Join(dir, "e"), 48*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := egress.NewSubsystem(egress.Config{
		Policy: p, Searcher: s, Retriever: r, SearchLedger: sl, ExtractLedger: el,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func toolCallMsg(name, args string) Message {
	var tc ToolCall
	tc.ID = "call_" + name
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return Message{Role: "assistant", ToolCalls: []ToolCall{tc}}
}

// scriptModel replays replies in order, repeating the last once exhausted.
type scriptModel struct {
	replies []Message
	calls   int
}

func (m *scriptModel) Complete(_ context.Context, _ []Message, _ []Tool) (Message, int, error) {
	i := m.calls
	m.calls++
	if i >= len(m.replies) {
		i = len(m.replies) - 1
	}
	return m.replies[i], 1, nil
}

// flowModel does a realistic search→extract→respond, reading the handle it must
// extract out of the search result message (a scripted model can't guess it).
type flowModel struct{ calls int }

func (m *flowModel) Complete(_ context.Context, messages []Message, _ []Tool) (Message, int, error) {
	m.calls++
	switch m.calls {
	case 1:
		return toolCallMsg("web_search", `{"query":"gradle toolchains"}`), 1, nil
	case 2:
		return toolCallMsg("extract_url_as_markdown", `{"target":"`+firstHandle(messages)+`"}`), 1, nil
	default:
		return toolCallMsg("respond", `{"answer":"Gradle resolves JDKs via toolchains.","sources":["https://a.example/1"]}`), 1, nil
	}
}

func firstHandle(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "tool" {
			continue
		}
		var rs []struct {
			Handle string `json:"handle"`
		}
		if json.Unmarshal([]byte(messages[i].Content), &rs) == nil && len(rs) > 0 {
			return rs[0].Handle
		}
	}
	return ""
}

// --- tests ------------------------------------------------------------------

func TestResearchHappyPath(t *testing.T) {
	s := &stubSearcher{hits: []egress.Hit{{Title: "T", URL: "https://a.example/1", Snippet: "S"}}}
	r := &stubRetriever{md: "# Gradle toolchains\nDetail."}
	aud := &recAuditor{}
	srv := New(Config{Egress: testEgress(t, s, r), Model: &flowModel{}, Audit: aud, Caps: DefaultCaps()})

	ans, err := srv.research(context.Background(), mustRunID(t), "how do gradle toolchains resolve JDKs?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans.answer.Answer, "toolchains") {
		t.Errorf("answer = %q", ans.answer.Answer)
	}
	if len(ans.answer.Sources) != 1 || ans.answer.Sources[0] != "https://a.example/1" {
		t.Errorf("sources = %v, want the consulted URL", ans.answer.Sources)
	}
	if s.calls != 1 || r.calls != 1 {
		t.Errorf("searcher=%d retriever=%d, want 1/1", s.calls, r.calls)
	}
	if got := aud.decisions("web_search"); len(got) != 1 || got[0] != decSearched {
		t.Errorf("web_search audit = %v", got)
	}
	if got := aud.decisions("extract_url_as_markdown"); len(got) != 1 || got[0] != decExtracted {
		t.Errorf("extract audit = %v", got)
	}
}

func TestUnknownToolIsAuditedNotDispatched(t *testing.T) {
	s := &stubSearcher{}
	r := &stubRetriever{}
	aud := &recAuditor{}
	model := &scriptModel{replies: []Message{
		toolCallMsg("delete_everything", `{}`),
		toolCallMsg("respond", `{"answer":"done"}`),
	}}
	srv := New(Config{Egress: testEgress(t, s, r), Model: model, Audit: aud, Caps: DefaultCaps()})

	// The model never searches, so the call fails closed (search-guard) — but the
	// unknown tool was still audited and never dispatched (the point of this test).
	_, err := srv.research(context.Background(), mustRunID(t), "q")
	var ref *refusal
	if err != nil && !errors.As(err, &ref) {
		t.Fatalf("unexpected non-refusal error: %v", err)
	}
	if s.calls != 0 || r.calls != 0 {
		t.Errorf("unknown tool dispatched: search=%d extract=%d", s.calls, r.calls)
	}
	if got := aud.decisions("delete_everything"); len(got) != 1 || got[0] != decRejectedUnknown {
		t.Errorf("unknown-tool audit = %v, want [%s]", got, decRejectedUnknown)
	}
}

// TestRefusesUngroundedAnswer: a model that answers without ever searching is
// nudged, then refused — the research tool never returns an answer from the
// model's own weights — structural, not prompt-only.
func TestRefusesUngroundedAnswer(t *testing.T) {
	model := &scriptModel{replies: []Message{toolCallMsg("respond", `{"answer":"from my weights"}`)}}
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{}, &stubRetriever{}),
		Model:  model, Audit: &recAuditor{}, Caps: DefaultCaps(),
	})
	_, err := srv.research(context.Background(), mustRunID(t), "q")
	var ref *refusal
	if !errors.As(err, &ref) {
		t.Fatalf("want a refusal for an ungrounded answer, got %v", err)
	}
	if model.calls != maxSearchNudges+1 {
		t.Errorf("model called %d times, want %d (initial + %d nudges)", model.calls, maxSearchNudges+1, maxSearchNudges)
	}
}

// TestSearchWithoutResultsIsNotGrounded closes the gap the smoke test exposed: a
// search that returns nothing (or errors) is an ATTEMPT, not grounding — a model
// that "searched" but retrieved nothing, then answered, is refused.
func TestSearchWithoutResultsIsNotGrounded(t *testing.T) {
	s := &stubSearcher{} // no hits: the search succeeds but returns nothing
	model := &scriptModel{replies: []Message{
		toolCallMsg("web_search", `{"query":"x"}`),
		toolCallMsg("respond", `{"answer":"from my weights"}`),
	}}
	srv := New(Config{Egress: testEgress(t, s, &stubRetriever{}), Model: model, Audit: &recAuditor{}, Caps: DefaultCaps()})

	_, err := srv.research(context.Background(), mustRunID(t), "q")
	var ref *refusal
	if !errors.As(err, &ref) {
		t.Fatalf("a search that returned nothing must not ground an answer; got %v", err)
	}
	if s.calls == 0 {
		t.Error("expected the model to have searched at least once")
	}
}

// TestBackendErrorsTripBreaker: repeated egress backend errors (e.g. a 401) end
// the loop fast instead of burning the whole search budget on retries.
func TestBackendErrorsTripBreaker(t *testing.T) {
	s := &stubSearcher{err: errors.New("upstream kagi.com: 401 Unauthorized")}
	model := &scriptModel{replies: []Message{toolCallMsg("web_search", `{"query":"x"}`)}} // keeps searching
	srv := New(Config{Egress: testEgress(t, s, &stubRetriever{}), Model: model, Audit: &recAuditor{}, Caps: DefaultCaps()})

	_, err := srv.research(context.Background(), mustRunID(t), "q")
	var ref *refusal
	if !errors.As(err, &ref) {
		t.Fatalf("repeated backend errors should trip the breaker; got %v", err)
	}
	if s.calls != maxEgressErrors {
		t.Errorf("searcher called %d times, want %d (breaker, not MaxSearches)", s.calls, maxEgressErrors)
	}
}

func TestNeverRespondHitsCap(t *testing.T) {
	caps := DefaultCaps()
	caps.MaxTurns = 3
	model := &scriptModel{replies: []Message{toolCallMsg("web_search", `{"query":"x"}`)}} // repeats forever
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{hits: []egress.Hit{{URL: "https://a.example/1"}}}, &stubRetriever{}),
		Model:  model, Audit: &recAuditor{}, Caps: caps,
	})

	_, err := srv.research(context.Background(), mustRunID(t), "q")
	var ref *refusal
	if !errors.As(err, &ref) || ref.decision != decRejectedCap {
		t.Fatalf("err = %v, want a rejected_cap refusal", err)
	}
	if model.calls != caps.MaxTurns {
		t.Errorf("model called %d times, want exactly MaxTurns=%d", model.calls, caps.MaxTurns)
	}
}

// capturingModel records the messages it sees each turn.  It searches on the
// fresh first turn (to ground the answer), then responds — so a research call
// completes in two turns and each first turn can be checked for request-scoping.
type capturingModel struct{ seen [][]Message }

func (m *capturingModel) Complete(_ context.Context, messages []Message, _ []Tool) (Message, int, error) {
	cp := make([]Message, len(messages))
	copy(cp, messages)
	m.seen = append(m.seen, cp)
	if len(messages) == 2 { // a fresh [system, user]: the first turn of a research call
		return toolCallMsg("web_search", `{"query":"x"}`), 1, nil
	}
	return toolCallMsg("respond", `{"answer":"ok"}`), 1, nil
}

func TestResearchContextIsRequestScoped(t *testing.T) {
	cap := &capturingModel{}
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{hits: []egress.Hit{{URL: "https://x.example/1"}}}, &stubRetriever{}),
		Model:  cap, Caps: DefaultCaps(),
	})

	if _, err := srv.research(context.Background(), mustRunID(t), "first query"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.research(context.Background(), mustRunID(t), "second query"); err != nil {
		t.Fatal(err)
	}
	// Two turns per research (search, respond); the FIRST turn of each must see a
	// fresh [system, user:query] — no state bleeds between calls.
	if len(cap.seen) != 4 {
		t.Fatalf("model called %d times, want 4 (2 turns x 2 research calls)", len(cap.seen))
	}
	for i, want := range []string{"first query", "second query"} {
		msgs := cap.seen[i*2]
		if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" || msgs[1].Content != want {
			t.Errorf("research %d first turn saw %+v, want a fresh [system, user:%q]", i, msgs, want)
		}
	}
}
