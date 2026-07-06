// Package egress is the scholar's in-process path to the web.  It searches
// (Kagi default, Brave alternate) and extracts pages to markdown (Kagi,
// server-side) behind the Searcher and Retriever seams, and enforces every
// guard the design requires: a single guarded transport that can dial only
// its configured relays (wire), an operator-owned deny list and internal-URL
// hygiene refusal (policy), the opaque-handle rule that closes the extract
// exfiltration primitive, a redacting key scrubber, and a restart-surviving
// burn ledger for the daily caps.  No MCP, no model, no shell, no arbitrary
// egress.
//
// The package is split into leaves — wire (transport), policy (config),
// search and extract (providers) — with this core stitching them together.
// The seam types are re-exported here so consumers import only egress.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jeffbstewart/cloister/internal/egress/extract"
	"github.com/jeffbstewart/cloister/internal/egress/policy"
	"github.com/jeffbstewart/cloister/internal/egress/search"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
)

// Re-exported seam types, so the scholar (and any other consumer) imports
// only egress.
type (
	Hit       = search.Hit
	Searcher  = search.Searcher
	Extracted = extract.Extracted
	Retriever = extract.Retriever
)

// Result is a search hit as handed to the model: the Hit plus an opaque,
// session-scoped Handle to extract it without exposing a URL the model could
// mutate into an exfiltration channel.
type Result struct {
	Hit
	Handle Handle
}

// Subsystem is the long-lived egress engine: policy, the two providers, the two
// burn ledgers, the key scrubber, and a clock.  It holds NO per-request state —
// per-query handle maps live only in a Session.
type Subsystem struct {
	policy        *policy.Policy
	searcher      Searcher
	retriever     Retriever
	searchLedger  *Ledger
	extractLedger *Ledger
	scrubber      *wire.Scrubber
	now           func() time.Time
}

// Config assembles a Subsystem.  Tests inject stub Searcher/Retriever; prod uses
// NewProviders to build them over the guarded client.
type Config struct {
	Policy        *policy.Policy
	Searcher      Searcher
	Retriever     Retriever
	SearchLedger  *Ledger
	ExtractLedger *Ledger
	Scrubber      *wire.Scrubber   // may be nil (no keys to redact)
	Now           func() time.Time // nil → time.Now
}

// NewSubsystem validates the wiring and returns the engine.
func NewSubsystem(cfg Config) (*Subsystem, error) {
	if cfg.Policy == nil || cfg.Searcher == nil || cfg.Retriever == nil ||
		cfg.SearchLedger == nil || cfg.ExtractLedger == nil {
		return nil, fmt.Errorf("egress: NewSubsystem: policy, searcher, retriever, and both ledgers are required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Subsystem{
		policy: cfg.Policy, searcher: cfg.Searcher, retriever: cfg.Retriever,
		searchLedger: cfg.SearchLedger, extractLedger: cfg.ExtractLedger,
		scrubber: cfg.Scrubber, now: now,
	}, nil
}

// Engine reports the configured search engine name (for the audit).
func (s *Subsystem) Engine() string { return s.searcher.Name() }

// Provider reports the extract backend name (for the audit).
func (s *Subsystem) Provider() string { return s.retriever.Name() }

// Session is the request-scoped state for one research call: a fresh, empty
// handle map.  Discarded when the call ends, so it is no cross-query memory
// channel.
type Session struct {
	sub     *Subsystem
	handles map[string]string // handle → exact result URL
}

// NewSession begins a request-scoped session.
func (s *Subsystem) NewSession() *Session {
	return &Session{sub: s, handles: map[string]string{}}
}

// Search runs one query under the daily search cap, mints a handle per result,
// records the handle→URL map, and returns model-facing Results.
func (se *Session) Search(ctx context.Context, query string, count int) ([]Result, error) {
	s := se.sub
	if s.searchLedger.CountSince(startOfUTCDay(s.now())) >= s.policy.Search.DailyCap {
		return nil, ErrSearchCap
	}
	if err := s.searchLedger.Record(s.now()); err != nil { // count the billable attempt
		return nil, err
	}
	hits, err := s.searcher.Search(ctx, query, count)
	if err != nil {
		return nil, s.scrubErr(err)
	}
	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		hdl, err := newHandle()
		if err != nil {
			return nil, err
		}
		se.handles[hdl.String()] = h.URL
		out = append(out, Result{Hit: h, Handle: hdl})
	}
	return out, nil
}

// Extract turns a target into markdown.  A target that parses as an http(s)
// URL is a raw, model-constructed URL — deny/hygiene-checked, then refused
// with ErrNeedsApproval and NO upstream call; the caller routes it through
// the operator-approval flow and, on approval, ExtractApprovedURL.  Anything
// else is treated as a session handle, resolved to its exact minted URL and
// extracted with no approval.
func (se *Session) Extract(ctx context.Context, target string) (Extracted, error) {
	if u, ok := asHTTPURL(target); ok {
		return se.extractRaw(u)
	}
	return se.extractHandle(ctx, target)
}

// ExtractApprovedURL performs a raw-URL extraction that Extract refused
// pending approval.  The scholar calls this ONLY after the operator approves
// the URL — but approval bypasses nothing else: the scheme, deny-list,
// hygiene, and daily-cap gates all still apply (a human cannot approve a
// denied host).
func (se *Session) ExtractApprovedURL(ctx context.Context, rawURL string) (Extracted, error) {
	u, ok := asHTTPURL(rawURL)
	if !ok {
		return Extracted{}, ErrNotHTTPS
	}
	return se.doExtract(ctx, u)
}

// URLFor returns the exact URL a session handle was minted for, and whether the
// token is a handle this session issued.  It is for audit only — the opaque
// handle is never logged, but the URL it points at is.
func (se *Session) URLFor(handle string) (string, bool) {
	u, ok := se.handles[handle]
	return u, ok
}

func (se *Session) extractHandle(ctx context.Context, handle string) (Extracted, error) {
	rawURL, ok := se.handles[handle]
	if !ok {
		// A token we never minted (mutated, or from another session): the model
		// cannot conjure a valid handle, so this is a logic error, not a gate.
		return Extracted{}, ErrUnknownHandle
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Extracted{}, fmt.Errorf("egress: stored handle URL is invalid: %w", err)
	}
	return se.doExtract(ctx, u)
}

// extractRaw applies the same gates as a handle extract, but instead of
// calling out it refuses with ErrNeedsApproval for the operator-approval
// flow.  A denied or internal host is refused OUTRIGHT — the operator is
// never even asked about it.
func (se *Session) extractRaw(u *url.URL) (Extracted, error) {
	if err := se.sub.checkTarget(u); err != nil {
		return Extracted{}, err
	}
	return Extracted{}, ErrNeedsApproval
}

// doExtract runs the final path for an approved-by-construction target (a
// resolved handle): scheme/deny/hygiene, the daily cap, then the Kagi call.
func (se *Session) doExtract(ctx context.Context, u *url.URL) (Extracted, error) {
	s := se.sub
	if err := s.checkTarget(u); err != nil {
		return Extracted{}, err
	}
	if s.extractLedger.CountSince(startOfUTCDay(s.now())) >= s.policy.Extract.DailyCap {
		return Extracted{}, ErrExtractCap
	}
	if err := s.extractLedger.Record(s.now()); err != nil {
		return Extracted{}, err
	}
	ext, err := s.retriever.Fetch(ctx, u.String())
	if err != nil {
		return Extracted{}, s.scrubErr(err)
	}
	return ext, nil
}

// checkTarget applies the scheme, deny-list, and internal-host gates shared by
// both extract paths.
func (s *Subsystem) checkTarget(u *url.URL) error {
	if u.Scheme != "https" {
		return ErrNotHTTPS
	}
	if s.policy.Denies(u.Hostname(), u.Path) {
		return ErrDenied
	}
	if policy.IsInternalHost(u.Hostname()) {
		return ErrInternalHost
	}
	return nil
}

// scrubErr redacts key material from a provider-call error before it leaves the
// package, preserving the identity of sentinels that carry no secrets.
func (s *Subsystem) scrubErr(err error) error {
	if err == nil || errors.Is(err, ErrResponseTooBig) {
		return err
	}
	return errors.New(s.scrubber.Scrub(err.Error()))
}

// asHTTPURL reports whether s parses as an http(s) URL (a raw model URL) rather
// than a handle.  A handle like "h_ab…" has no scheme, so it lands here as false.
func asHTTPURL(s string) (*url.URL, bool) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, false
	}
	return u, true
}

// startOfUTCDay is the daily-cap window boundary.  The cap deliberately
// rolls at UTC midnight rather than the process timezone's:
//
//   - UTC has no DST, so every window is exactly 24 hours; a local-midnight
//     window is 23 or 25 hours twice a year, and "2:30am" happens twice on
//     one of those days.
//   - It matches every other timestamp in the system (audit records, cell
//     status), so the ledger, the audit trail, and the cap all agree on
//     which day an event belongs to.
//   - The cap is a spend/exfiltration bound per day-window, not a
//     scheduling feature — whose midnight it rolls at doesn't change the
//     bound, so the deterministic boundary wins.
func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
