package scholar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/egress"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// Audit decision values, as audit.Decision consts.
const (
	decSearched         audit.Decision = "searched"
	decExtracted        audit.Decision = "extracted"
	decPending          audit.Decision = "pending_approval"
	decRejected         audit.Decision = "rejected"
	decRejectedDenied   audit.Decision = "rejected_denied"
	decRejectedInternal audit.Decision = "rejected_internal"
	decRejectedCap      audit.Decision = "rejected_cap"
	decRejectedUnknown  audit.Decision = "rejected_unknown_tool"
	decRejectedBadInput audit.Decision = "rejected_bad_input"
	decAnswered         audit.Decision = "answered"
	decRejectedTimeout  audit.Decision = "rejected_timeout"
	decError            audit.Decision = "error"
)

// provenance values for an extract's Via.
const (
	viaSearchResult = "search_result"
	viaRawURL       = "raw_url"
)

// maxSearchNudges bounds how many times the loop pushes the model to search
// before it gives up: a research answer must be grounded in at least one
// web_search — structural, not prompt discipline.  A model that answers
// from its own weights is nudged; if it still refuses, the call fails closed
// rather than return an ungrounded answer.
const maxSearchNudges = 2

// maxEgressErrors trips the backend breaker: this many back-to-back egress
// backend errors (e.g. a 401) end the loop, since retrying a broken backend
// just wastes the search budget.
const maxEgressErrors = 2

// refusal is a terminal, agent-visible refusal carrying its audit decision and,
// when a bound tripped, the specific limit.
type refusal struct {
	decision audit.Decision
	limit    audit.Limit
	msg      string
}

func (r *refusal) Error() string { return r.msg }

// researchResult is what research returns: the answer (empty on a refusal), the
// URLs-only transcript built along the way, and the loop stats for the audit.
type researchResult struct {
	answer     Answer
	transcript string
	truncated  bool
	searches   int
	extracts   int
	tokens     int
}

// runctx bundles the per-request loop state, passed to the dispatch helpers so
// none of it lives on the Server (request-scoped).
type runctx struct {
	opID      runid.ID
	sess      *egress.Session
	tr        *transcript
	messages  *[]Message
	consulted map[string]bool
	// grounded is set once the loop has actually RETRIEVED something usable — a
	// search that returned results, or a successful extract.  A mere attempt (a
	// search that errored or returned nothing) does NOT ground: the answer must
	// rest on real retrieved information, never the model's weights.
	grounded bool
	// consecutiveErrs counts back-to-back egress BACKEND errors (decError — a
	// 401, TLS/dial failure, …), reset by any non-error outcome.  A persistent
	// backend error will not fix itself on retry, so the loop trips a breaker
	// rather than let the model burn its whole search budget. lastErr is the most
	// recent such error (scrubbed) for the refusal message.
	consecutiveErrs int
	lastErr         string
}

// noteEgress updates the consecutive-error breaker: a backend error advances it,
// any other outcome (results, deny, cap) resets it — those mean the backend is
// reachable and answering.
func (rc *runctx) noteEgress(dec audit.Decision, err error) {
	if dec == decError {
		rc.consecutiveErrs++
		if err != nil {
			rc.lastErr = err.Error()
		}
		return
	}
	rc.consecutiveErrs = 0
}

// research runs the fixed loop for one query: fresh context, drive the model,
// dispatch its tool calls to the request-scoped egress session, build the
// transcript, and finish when the model calls respond or a cap trips.
func (s *Server) research(ctx context.Context, opID runid.ID, query string) (researchResult, error) {
	c := s.cfg.Caps
	rc := &runctx{opID: opID, sess: s.cfg.Egress.NewSession(),
		tr: newTranscript(c.MaxTranscriptBytes, query), consulted: map[string]bool{}}
	log.Printf("scholar: op=%s research start: %q", opID, clip(query, 200))
	if len(query) > c.MaxQueryBytes {
		return rc.result(0, 0, 0), &refusal{decRejectedCap, audit.LimitQuerySize, fmt.Sprintf("query exceeds the %d-byte limit", c.MaxQueryBytes)}
	}
	ctx, cancel := context.WithTimeout(ctx, c.WallClock)
	defer cancel()

	messages := []Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: query}}
	rc.messages = &messages
	var searches, extracts, tokens, searchNudges int

	// acceptOrNudge gates finishing: an answer is accepted only once at least one
	// web_search has run (grounded).  Otherwise it pushes the model to search, and
	// after maxSearchNudges gives up and fails closed rather than return an
	// answer from the model's own weights. done=false means "nudged, keep looping".
	acceptOrNudge := func(ans Answer) (researchResult, error, bool) {
		if rc.grounded {
			return rc.finish(ans, searches, extracts, tokens), nil, true
		}
		if searchNudges < maxSearchNudges {
			searchNudges++
			messages = append(messages, Message{Role: "user",
				Content: "You have no usable results yet — your searches returned nothing or errored. Obtain real results from web_search (try different terms, or read a result) before answering. Never answer from your own knowledge."})
			return researchResult{}, nil, false
		}
		return rc.result(searches, extracts, tokens),
			&refusal{decError, "", "no web search returned usable results, so no grounded answer is available"}, true
	}

	for turn := 0; ; turn++ {
		if turn >= c.MaxTurns {
			return rc.result(searches, extracts, tokens), &refusal{decRejectedCap, audit.LimitSteps, "reached the step limit without an answer"}
		}
		reply, used, err := s.cfg.Model.Complete(ctx, messages, toolDefs)
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("scholar: op=%s timed out at turn %d", opID, turn)
				return rc.result(searches, extracts, tokens), &refusal{decRejectedTimeout, "", "research timed out"}
			}
			log.Printf("scholar: op=%s model call failed at turn %d: %v", opID, turn, err)
			return rc.result(searches, extracts, tokens), err
		}
		tokens += used
		if c.MaxTokens > 0 && tokens > c.MaxTokens {
			return rc.result(searches, extracts, tokens), &refusal{decRejectedCap, audit.LimitTokens, "token budget exhausted before an answer"}
		}
		messages = append(messages, reply)
		if txt := strings.TrimSpace(reply.Content); txt != "" {
			rc.tr.line("model: %s", oneLine(txt))
		}

		if len(reply.ToolCalls) == 0 {
			if txt := strings.TrimSpace(reply.Content); txt != "" {
				ans := Answer{Answer: capStr(txt, c.MaxAnswerBytes), Sources: sortedKeys(rc.consulted)}
				if res, err, done := acceptOrNudge(ans); done {
					return res, err
				}
				continue
			}
			messages = append(messages, Message{Role: "user", Content: "Use a tool, or call respond with your answer."})
			continue
		}

		nudged := false
		for _, tc := range reply.ToolCalls {
			switch tc.Function.Name {
			case "respond":
				ans := s.parseRespond(tc.Function.Arguments, rc.consulted, c.MaxAnswerBytes)
				if res, err, done := acceptOrNudge(ans); done {
					return res, err
				}
				nudged = true
			case "web_search":
				if searches >= c.MaxSearches {
					s.refuseTool(rc, tc, "web_search", audit.LimitSearchesPerInvocation, "refused: per-invocation search limit reached; answer with what you have")
					continue
				}
				searches++
				s.doSearch(ctx, rc, tc)
			case "extract_url_as_markdown":
				if extracts >= c.MaxExtracts {
					s.refuseTool(rc, tc, "extract_url_as_markdown", audit.LimitExtractsPerInvocation, "refused: per-invocation read limit reached")
					continue
				}
				extracts++
				s.doExtract(ctx, rc, tc)
			default:
				s.toolResult(rc.messages, tc.ID, "error: unknown tool "+tc.Function.Name)
				rc.tr.line("unknown tool: %s", tc.Function.Name)
				s.audit(audit.New(rc.opID, tc.Function.Name, decRejectedUnknown, 0))
			}
			if nudged {
				break // a respond attempt was rejected for want of a search; re-loop
			}
		}
		if rc.consecutiveErrs >= maxEgressErrors {
			return rc.result(searches, extracts, tokens),
				&refusal{decError, "", "web search is unavailable (repeated backend errors): " + clip(rc.lastErr, 200)}
		}
		if ctx.Err() != nil {
			return rc.result(searches, extracts, tokens), &refusal{decRejectedTimeout, "", "research timed out"}
		}
	}
}

func (rc *runctx) result(searches, extracts, tokens int) researchResult {
	return researchResult{transcript: rc.tr.String(), truncated: rc.tr.truncated,
		searches: searches, extracts: extracts, tokens: tokens}
}

func (rc *runctx) finish(ans Answer, searches, extracts, tokens int) researchResult {
	rc.tr.line("answer: %s", oneLine(ans.Answer))
	r := rc.result(searches, extracts, tokens)
	r.answer = ans
	return r
}

func (s *Server) parseRespond(argsJSON string, consulted map[string]bool, maxAnswer int) Answer {
	var a struct {
		Answer  string   `json:"answer"`
		Sources []string `json:"sources"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &a)
	for _, src := range a.Sources {
		if src != "" {
			consulted[src] = true
		}
	}
	return Answer{Answer: capStr(a.Answer, maxAnswer), Sources: sortedKeys(consulted)}
}

// refuseTool audits a per-invocation cap refusal, attaching the tool's identity
// (query for search; provenance+URL for extract) and the limit that tripped.
func (s *Server) refuseTool(rc *runctx, tc ToolCall, tool string, limit audit.Limit, note string) {
	s.toolResult(rc.messages, tc.ID, note)
	rc.tr.line("%s refused: %s", tool, limit)
	rec := audit.New(rc.opID, tool, decRejectedCap, 0)
	rec.Limit = limit
	if tool == "web_search" {
		rec.Search = &audit.SearchDetail{Query: argString(tc.Function.Arguments, "query")}
	} else {
		via, url := s.identify(rc, argString(tc.Function.Arguments, "target"))
		rec.Extract = &audit.ExtractDetail{Via: via, URL: url}
	}
	s.audit(rec)
}

func (s *Server) doSearch(ctx context.Context, rc *runctx, tc ToolCall) {
	start := time.Now()
	query := argString(tc.Function.Arguments, "query")
	if query == "" {
		s.toolResult(rc.messages, tc.ID, "error: web_search needs a non-empty query")
		s.audit(audit.New(rc.opID, "web_search", decRejectedBadInput, time.Since(start)))
		return
	}
	count := argInt(tc.Function.Arguments, "count")
	if count == 0 {
		count = 5
	}
	res, err := rc.sess.Search(ctx, query, count)
	if err != nil {
		note, dec, lim := mapEgressErr(err, "search")
		rc.noteEgress(dec, err)
		s.toolResult(rc.messages, tc.ID, note)
		rc.tr.line("search %q: %s", query, dec)
		rec := audit.New(rc.opID, "web_search", dec, time.Since(start))
		rec.Limit = lim
		rec.Status = clip(err.Error(), 300) // scrubbed by the egress subsystem
		rec.Search = &audit.SearchDetail{Query: query}
		s.audit(rec)
		return
	}
	rc.consecutiveErrs = 0 // the backend answered (even if 0 results)
	type modelResult struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
		Handle  string `json:"handle"`
	}
	out := make([]modelResult, len(res))
	urls := make([]string, len(res))
	for i, r := range res {
		// Clip snippets: they accumulate across many searches and Kagi's can be
		// long.  The model uses them to pick what to extract, not as the answer.
		out[i] = modelResult{Title: r.Title, URL: r.URL, Snippet: clip(r.Snippet, 300), Handle: r.Handle.String()}
		urls[i] = r.URL
	}
	if len(res) > 0 {
		rc.grounded = true // real results retrieved — the answer can rest on them
	}
	b, _ := json.Marshal(out)
	s.toolResult(rc.messages, tc.ID, string(b))
	rc.tr.line("search %q -> %d results: %s", query, len(urls), strings.Join(urls, " "))
	rec := audit.New(rc.opID, "web_search", decSearched, time.Since(start))
	rec.Search = &audit.SearchDetail{Query: query, Engine: s.cfg.Egress.Engine(), ResultURLs: urls}
	s.audit(rec)
}

func (s *Server) doExtract(ctx context.Context, rc *runctx, tc ToolCall) {
	start := time.Now()
	target := argString(tc.Function.Arguments, "target")
	if target == "" {
		s.toolResult(rc.messages, tc.ID, "error: extract_url_as_markdown needs a target (a handle or an https URL)")
		s.audit(audit.New(rc.opID, "extract_url_as_markdown", decRejectedBadInput, time.Since(start)))
		return
	}
	via, url := s.identify(rc, target)
	ext, err := rc.sess.Extract(ctx, target)
	if err == nil {
		s.recordExtract(rc, tc, via, url, ext, decExtracted, start)
		return
	}
	if errors.Is(err, egress.ErrNeedsApproval) {
		s.gateRawExtract(ctx, rc, tc, url, start) // a raw model URL: url == target
		return
	}
	note, dec, lim := mapEgressErr(err, "read")
	rc.noteEgress(dec, err)
	s.toolResult(rc.messages, tc.ID, note)
	rc.tr.line("read[%s] %s: %s", via, url, dec)
	rec := audit.New(rc.opID, "extract_url_as_markdown", dec, time.Since(start))
	rec.Limit = lim
	rec.Status = clip(err.Error(), 300) // scrubbed by the egress subsystem
	rec.Extract = &audit.ExtractDetail{Via: via, URL: url}
	s.audit(rec)
}

// gateRawExtract registers a raw, model-built URL for per-retrieval operator
// approval and, on approval, extracts it (all egress gates still apply).
func (s *Server) gateRawExtract(ctx context.Context, rc *runctx, tc ToolCall, rawURL string, start time.Time) {
	rc.tr.line("read[raw_url] %s: pending approval", rawURL)
	pend := audit.New(rc.opID, "extract_url", decPending, 0)
	pend.Extract = &audit.ExtractDetail{Via: viaRawURL, URL: rawURL}
	s.audit(pend)

	switch s.gate(ctx, "extract_url", rawURL, s.cfg.Caps.WallClock) {
	case approval.Approved:
		ext, err := rc.sess.ExtractApprovedURL(ctx, rawURL)
		if err != nil {
			note, dec, lim := mapEgressErr(err, "read")
			rc.noteEgress(dec, err)
			s.toolResult(rc.messages, tc.ID, note)
			rc.tr.line("read[raw_url] %s: %s", rawURL, dec)
			rec := audit.New(rc.opID, "extract_url_as_markdown", dec, time.Since(start))
			rec.Limit = lim
			rec.Status = clip(err.Error(), 300)
			rec.Extract = &audit.ExtractDetail{Via: viaRawURL, URL: rawURL}
			s.audit(rec)
			return
		}
		s.recordExtract(rc, tc, viaRawURL, rawURL, ext, decExtracted, start)
	case approval.Timeout:
		s.toolResult(rc.messages, tc.ID, "the operator did not approve reading that URL in time; note it unavailable and continue")
		rc.tr.line("read[raw_url] %s: rejected_timeout", rawURL)
		s.auditRawRefusal(rc.opID, rawURL, decRejectedTimeout, start)
	default:
		s.toolResult(rc.messages, tc.ID, "the operator declined reading that URL; note it unavailable and continue")
		rc.tr.line("read[raw_url] %s: rejected", rawURL)
		s.auditRawRefusal(rc.opID, rawURL, decRejected, start)
	}
}

func (s *Server) auditRawRefusal(opID runid.ID, rawURL string, dec audit.Decision, start time.Time) {
	rec := audit.New(opID, "extract_url_as_markdown", dec, time.Since(start))
	rec.Extract = &audit.ExtractDetail{Via: viaRawURL, URL: rawURL}
	s.audit(rec)
}

// recordExtract handles a successful extraction: the model result, the
// consulted-source set, the transcript line, and the audit record.
func (s *Server) recordExtract(rc *runctx, tc ToolCall, via, url string, ext egress.Extracted, decision audit.Decision, start time.Time) {
	rc.grounded = true     // a page was actually read — the answer can rest on it
	rc.consecutiveErrs = 0 // the backend answered
	if ext.FinalURL != "" {
		rc.consulted[ext.FinalURL] = true
	}
	// Cap the markdown fed into the model context (a page can be huge); the model
	// re-reads sparingly and the full byte count is still recorded below.
	s.toolResult(rc.messages, tc.ID, capStr(ext.Markdown, s.cfg.Caps.MaxExtractBytes))
	rc.tr.line("read[%s] %s (%d bytes)", via, ext.FinalURL, len(ext.Markdown))
	rec := audit.New(rc.opID, "extract_url_as_markdown", decision, time.Since(start))
	rec.Extract = &audit.ExtractDetail{Via: via, URL: url, Provider: s.cfg.Egress.Provider(), FinalURL: ext.FinalURL}
	s.audit(rec)
}

// identify classifies an extract target for the audit: a session handle resolves
// to its URL (provenance search_result, the opaque handle elided); a raw https
// URL is itself; anything else is an unknown token we decline to log.
func (s *Server) identify(rc *runctx, target string) (via, url string) {
	if u, ok := rc.sess.URLFor(target); ok {
		return viaSearchResult, u
	}
	if strings.HasPrefix(strings.ToLower(target), "https://") {
		return viaRawURL, target
	}
	return "", ""
}

// mapEgressErr turns an egress sentinel into an audit decision (+ limit, for
// caps) and a note the model can act on.  The provider-error text is already
// scrubbed by egress.
func mapEgressErr(err error, kind string) (note string, decision audit.Decision, limit audit.Limit) {
	switch {
	case errors.Is(err, egress.ErrDenied):
		return "refused: that host is on the deny list; note it as unavailable and continue", decRejectedDenied, ""
	case errors.Is(err, egress.ErrInternalHost):
		return "refused: that target is an internal address; continue with other sources", decRejectedInternal, ""
	case errors.Is(err, egress.ErrSearchCap):
		return "refused: daily search budget reached; answer with what you have", decRejectedCap, audit.LimitSearchesPerDay
	case errors.Is(err, egress.ErrExtractCap):
		return "refused: daily read budget reached; answer with what you have", decRejectedCap, audit.LimitExtractsPerDay
	case errors.Is(err, egress.ErrUnknownHandle):
		return "refused: unknown handle; use a handle returned by a recent web_search", decRejectedUnknown, ""
	case errors.Is(err, egress.ErrNotHTTPS):
		return "refused: only https targets are allowed", decRejectedBadInput, ""
	default:
		return "error: " + err.Error(), decError, ""
	}
}

func (s *Server) toolResult(messages *[]Message, callID, text string) {
	*messages = append(*messages, Message{Role: "tool", ToolCallID: callID, Content: text})
}

func capStr(s string, maxBytes int) string {
	if maxBytes > 0 && len(s) > maxBytes {
		return s[:maxBytes] + "\n…[truncated]"
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func argString(argsJSON, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(argsJSON), &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func argInt(argsJSON, key string) int {
	var m map[string]any
	if json.Unmarshal([]byte(argsJSON), &m) != nil {
		return 0
	}
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

// clip shortens a string for a log line without dumping a whole query/answer.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
