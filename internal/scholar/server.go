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

// Package scholar is the quarantined web-research agent:
// the agent-builder binary's -scholar mode.  It serves one MCP tool, research(query),
// and behind it a fixed Go loop drives a model (OpenAI-compatible) with exactly
// three tools — web_search, extract_url_as_markdown, respond — dispatched to the
// in-process egress subsystem.  It holds no workspace and, verified by a boot
// self-check (selfcheck.go), no route to the arbitrary internet.
package scholar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/egress"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// Auditor records one line per research call and per search/extract.  The
// state-service client satisfies it; tests pass a collector.
type Auditor interface {
	Append(audit.Record) error
}

// TranscriptStore persists a research call's URLs-only transcript, keyed by
// opId, for later review at /research/<opId>. The state-service client satisfies
// it; a nil store simply means no transcript is kept.
type TranscriptStore interface {
	PutTranscript(opID runid.ID, payload []byte) error
}

// Answer is what research returns to the coding agent.
type Answer struct {
	Answer  string   `json:"answer"`
	Sources []string `json:"sources"`
}

// Caps bound one research call.  The three timeouts must sum to ≤ the
// agent's MCP client timeout: query 15m + loop 10m + answer 10m ≤ 40m.
type Caps struct {
	MaxQueryBytes      int
	MaxAnswerBytes     int
	MaxExtractBytes    int // per-extract markdown fed to the model (context budget, not the answer)
	MaxSearches        int
	MaxExtracts        int
	MaxTurns           int // hard ceiling on model round-trips; guarantees termination
	MaxTokens          int // 0 = disabled
	MaxTranscriptBytes int // transcript size cap (truncate + flag beyond)
	WallClock          time.Duration
	QueryApproval      time.Duration // query-gate deadline
	AnswerApproval     time.Duration // answer-gate deadline
}

// DefaultCaps are the shipped defaults.
func DefaultCaps() Caps {
	return Caps{
		MaxQueryBytes:      4 << 10,
		MaxAnswerBytes:     16 << 10,
		MaxExtractBytes:    24 << 10, // keep one page from dominating the ~32K-token context
		MaxSearches:        10,
		MaxExtracts:        20,
		MaxTurns:           40,
		MaxTokens:          0,
		MaxTranscriptBytes: 1 << 20, // 1 MiB
		WallClock:          10 * time.Minute,
		QueryApproval:      15 * time.Minute,
		AnswerApproval:     10 * time.Minute,
	}
}

// Config wires the scholar server.
type Config struct {
	Version     string
	Egress      *egress.Subsystem
	Model       Completer
	Audit       Auditor         // may be nil (tests)
	Approvals   ApprovalClient  // gates fail-closed if nil; production always sets it
	Transcripts TranscriptStore // may be nil (no transcript kept)
	AnswerGate  bool            // gate the answer before returning (default on in prod)
	Caps        Caps
}

// Server owns the research MCP surface.
type Server struct {
	cfg Config
	mcp *mcp.Server
}

// New builds the research tool surface.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "scholar", Version: cfg.Version}, nil)
	s.mcp.AddTool(
		&mcp.Tool{
			Name:        "research",
			Description: "Answer a self-contained question using web search and page retrieval. Returns {answer, sources}. Blocks until answered, refused, or timed out.",
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"query": {Type: "string", Description: "a self-contained question; the scholar has no other context and no project access"},
				},
				Required: []string{"query"},
			},
		},
		s.handleResearch,
	)
	return s
}

// Handler serves MCP at /mcp and a liveness probe at /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcp }, nil))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	return mux
}

func (s *Server) handleResearch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult("bad arguments: " + err.Error()), nil
	}
	if strings.TrimSpace(args.Query) == "" {
		return errResult("query is required"), nil
	}
	// opID correlates the research record, its search/extract records, and
	// the transcript.
	opID, err := runid.New()
	if err != nil {
		return errResult("internal: mint op id: " + err.Error()), nil
	}
	start := time.Now()

	// Query gate (always on): the operator sees the whole query — the entire
	// exfiltration budget — before the scholar spends a single call on it.
	s.auditResearch(opID, args.Query, decPending, time.Since(start))
	switch s.gate(ctx, "research", args.Query, s.cfg.Caps.QueryApproval) {
	case approval.Approved:
	case approval.Timeout:
		s.auditResearch(opID, args.Query, decRejectedTimeout, time.Since(start))
		return errResult("research query was not approved in time — STOP, do not retry"), nil
	default:
		s.auditResearch(opID, args.Query, decRejected, time.Since(start))
		return errResult("research query was declined by the operator — STOP"), nil
	}

	res, err := s.research(ctx, opID, args.Query)
	stored := s.storeTranscript(opID, res.transcript)

	// One enriched research record: loop stats + transcript flag.  Its
	// decision and duration are finalized at each exit.
	rec := audit.New(opID, "research", decError, 0)
	rec.Research = &audit.ResearchDetail{Query: args.Query,
		Searches: res.searches, Extracts: res.extracts, Tokens: res.tokens, TranscriptStored: stored}
	finalize := func(d audit.Decision) {
		rec.Decision = d
		rec.Duration = audit.Duration(time.Since(start))
		s.audit(rec)
	}

	if err != nil {
		var ref *refusal
		if errors.As(err, &ref) {
			rec.Limit = ref.limit
			finalize(ref.decision)
			return errResult(ref.msg), nil
		}
		finalize(decError)
		return errResult(err.Error()), nil
	}

	// Answer gate config knob: the answer is the one artifact crossing into the
	// code-writing context, so the operator reviews it (sources open in their own
	// browser) before it reaches the agent.
	if s.cfg.AnswerGate {
		s.auditResearch(opID, args.Query, decPending, time.Since(start))
		switch s.gate(ctx, "research_answer", answerGatePath(res.answer), s.cfg.Caps.AnswerApproval) {
		case approval.Approved:
		case approval.Timeout:
			finalize(decRejectedTimeout)
			return errResult("answer was not approved in time — STOP"), nil
		default:
			finalize(decRejected)
			return errResult("answer was declined by the operator — STOP"), nil
		}
	}

	rec.Research.AnswerBytes = len(res.answer.Answer)
	rec.Research.AnswerSHA256 = sha256Hex(res.answer.Answer)
	finalize(decAnswered)
	return jsonResult(res.answer), nil
}

// storeTranscript uploads the URLs-only transcript, returning whether it landed.
func (s *Server) storeTranscript(opID runid.ID, text string) bool {
	if s.cfg.Transcripts == nil || text == "" {
		return false
	}
	if err := s.cfg.Transcripts.PutTranscript(opID, []byte(text)); err != nil {
		log.Printf("scholar: store transcript %s: %v", opID, err)
		return false
	}
	return true
}

// answerGatePath is the reviewable summary shown on the approval page: the
// answer followed by the consulted source URLs.  The operator opens the sources
// in their own browser to verify them (no cell-side content storage).
func answerGatePath(ans Answer) string {
	var b strings.Builder
	b.WriteString(ans.Answer)
	if len(ans.Sources) > 0 {
		b.WriteString("\n\nsources:\n")
		for _, src := range ans.Sources {
			b.WriteString(src)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (s *Server) auditResearch(opID runid.ID, query string, decision audit.Decision, dur time.Duration) {
	rec := audit.New(opID, "research", decision, dur)
	rec.Research = &audit.ResearchDetail{Query: query}
	s.audit(rec)
}

func (s *Server) audit(r audit.Record) {
	logAudit(r) // mirror every event to stdout (docker logs) for live visibility
	if s.cfg.Audit == nil {
		return
	}
	if err := s.cfg.Audit.Append(r); err != nil {
		log.Printf("scholar: audit append failed: %v", err)
	}
}

// logAudit writes one concise operational line per audit event — the scholar's
// stdout trail, independent of the state-service audit sink, so a failing loop
// is legible in `docker logs`.  URLs and counts only, never page content.
func logAudit(r audit.Record) {
	st := statusTag(r)
	switch {
	case r.Search != nil:
		log.Printf("scholar: op=%s %s -> %s%s query=%q hits=%d%s",
			r.RunID, r.Tool, r.Decision, limitTag(r), clip(r.Search.Query, 80), len(r.Search.ResultURLs), st)
	case r.Extract != nil:
		u := r.Extract.URL
		if u == "" {
			u = r.Extract.FinalURL
		}
		log.Printf("scholar: op=%s %s -> %s%s via=%s url=%s%s",
			r.RunID, r.Tool, r.Decision, limitTag(r), r.Extract.Via, u, st)
	case r.Research != nil:
		log.Printf("scholar: op=%s %s -> %s%s searches=%d extracts=%d tokens=%d transcript=%v%s",
			r.RunID, r.Tool, r.Decision, limitTag(r),
			r.Research.Searches, r.Research.Extracts, r.Research.Tokens, r.Research.TranscriptStored, st)
	default:
		log.Printf("scholar: op=%s %s -> %s%s%s", r.RunID, r.Tool, r.Decision, limitTag(r), st)
	}
}

func limitTag(r audit.Record) string {
	if r.Limit == "" {
		return ""
	}
	return " limit=" + string(r.Limit)
}

func statusTag(r audit.Record) string {
	if r.Status == "" {
		return ""
	}
	return " status=" + clip(r.Status, 200)
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errResult(msg string) *mcp.CallToolResult {
	r := textResult(msg)
	r.IsError = true
	return r
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult("internal: marshal result: " + err.Error())
	}
	return textResult(string(b))
}
