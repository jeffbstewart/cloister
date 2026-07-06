// Package audit appends one JSONL record per action call — including
// rejected ones — to /state/audit.jsonl.  This record shape is the seed of
// the ecosystem-wide audit layer; the future internet-gateway service will
// emit the same shape.
//
// The current file rotates at Options.MaxBytes into numbered generations
// (audit.jsonl.1 … .N, oldest dropped), so history is bounded without ever
// truncating a file in place.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

// Decision is the terminal disposition of one audited op.  It is a named string
// type — an OPEN enum: the audit package defines the builder decisions here, and
// each subsystem (scribe, scholar) declares its own decisions as audit.Decision
// consts.  That keeps callers type-safe without the audit package having to
// enumerate every subsystem's outcomes.
type Decision string

// Builder-action decisions.  Scribe/scholar decisions live in those packages.
const (
	DecisionRun                Decision = "run"
	DecisionRejectedParam      Decision = "rejected_param"
	DecisionRejectedBusy       Decision = "rejected_busy"
	DecisionRejectedNoManifest Decision = "rejected_no_manifest"
)

// Duration is a time.Duration that serializes as its readable string form
// ("412ms", "1.5s") rather than a raw count — self-describing in the JSONL, a
// proper duration in memory.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Std returns the standard-library time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Limit names the bound whose exhaustion caused a rejection — the optional
// companion to a cap decision.  Empty unless a limit was the cause.  It keeps the
// Decision enum coarse (one rejected_cap) while still letting a reader tell a
// persistent daily quota ("the cell is out of budget") from a per-invocation
// ceiling: grep the limit, not a proliferation of decisions.
type Limit string

const (
	LimitSearchesPerDay        Limit = "searches_per_day"        // daily egress quota (persistent)
	LimitExtractsPerDay        Limit = "extracts_per_day"        // daily egress quota (persistent)
	LimitSearchesPerInvocation Limit = "searches_per_invocation" // one research call's ceiling
	LimitExtractsPerInvocation Limit = "extracts_per_invocation" // one research call's ceiling
	LimitQuerySize             Limit = "query_size"
	LimitTokens                Limit = "tokens"
	LimitSteps                 Limit = "steps"
)

// Rotation defaults.
const (
	DefaultMaxBytes    = 10 << 20 // rotate the current file past 10 MB
	DefaultGenerations = 5        // audit.jsonl.1 … .5 are kept
)

// Options tunes rotation; the zero value selects the defaults.
type Options struct {
	MaxBytes    int64 // rotate when the current file reaches this; 0 → DefaultMaxBytes
	Generations int   // rotated generations kept; 0 → DefaultGenerations
}

// Header is the required envelope on every audit record: when (Time), which op
// (RunID), what (Tool), the outcome (Decision), and how long it took (Duration).
// Every field is required — no omitempty, and Validate rejects an unset one — so
// the timeline backbone is always present and greppable.
type Header struct {
	Time     time.Time `json:"ts"`
	RunID    runid.ID  `json:"runId"`
	Tool     string    `json:"tool"`
	Decision Decision  `json:"decision"`
	Duration Duration  `json:"duration"`
	// Limit is the one optional envelope field: which bound was exhausted, set
	// only alongside a cap decision.  The required core above is what Validate
	// enforces.
	Limit Limit `json:"limit,omitempty"`
}

// Validate reports the first unset required Header field (Duration may be zero —
// an instantaneous op is legitimate).
func (h Header) Validate() error {
	switch {
	case h.Tool == "":
		return fmt.Errorf("audit: header.tool is required")
	case h.Decision == "":
		return fmt.Errorf("audit: header.decision is required")
	case h.RunID.IsZero():
		return fmt.Errorf("audit: header.runId is required")
	case h.Time.IsZero():
		return fmt.Errorf("audit: header.ts is required")
	}
	return nil
}

// CommandDetail is a builder command's record body: a manifest action
// invocation — build, test, coverage, lint, etc.
type CommandDetail struct {
	Params   map[string]string `json:"params,omitempty"` // the agent-suppliable inputs
	Argv     []string          `json:"argv,omitempty"`   // the fully resolved command run
	ExitCode *int              `json:"exitCode,omitempty"`
	LogPath  string            `json:"logPath,omitempty"`
	LogBytes int64             `json:"logBytes,omitempty"`
}

// MutationDetail is a scribe workspace-mutation's record body.  A
// single-target op sets Path; a move/copy sets From and To.
type MutationDetail struct {
	Path          string `json:"path,omitempty"` // workspace-relative target
	From          string `json:"from,omitempty"` // move/copy source
	To            string `json:"to,omitempty"`   // move/copy destination
	BytesBefore   int64  `json:"bytesBefore,omitempty"`
	BytesAfter    int64  `json:"bytesAfter,omitempty"`
	FilesTouched  int    `json:"filesTouched,omitempty"`
	LinesAdded    int    `json:"linesAdded,omitempty"`
	LinesRemoved  int    `json:"linesRemoved,omitempty"`
	SHA256After   string `json:"sha256After,omitempty"`
	HasDiff       bool   `json:"hasDiff,omitempty"`       // a diff payload is stored for this opId
	DiffTruncated bool   `json:"diffTruncated,omitempty"` // that payload was capped for size
}

// ResearchDetail is a scholar research-call's record body.  URLs and counts
// only — never page bodies or result content.
type ResearchDetail struct {
	Query            string `json:"query,omitempty"`
	Searches         int    `json:"searches,omitempty"`
	Extracts         int    `json:"extracts,omitempty"`
	Tokens           int    `json:"tokens,omitempty"`
	AnswerBytes      int    `json:"answerBytes,omitempty"`
	AnswerSHA256     string `json:"answerSha256,omitempty"`
	TranscriptStored bool   `json:"transcriptStored,omitempty"`
}

// SearchDetail is a scholar web_search record body.  The query and hit URLs
// — never the result snippets or page content.
type SearchDetail struct {
	Query      string   `json:"query"`
	Engine     string   `json:"engine,omitempty"`     // search backend
	ResultURLs []string `json:"resultUrls,omitempty"` // hit URLs
}

// ExtractDetail is a scholar extract_url_as_markdown record body: which URL
// was consulted and how it was targeted — never the fetched page body.  The
// opaque search handle is elided; a handle is resolved to its URL for the log.
type ExtractDetail struct {
	Via      string `json:"via,omitempty"`      // provenance: "search_result" | "raw_url"
	URL      string `json:"url,omitempty"`      // the URI retrieved (identity; present on success and failure)
	Provider string `json:"provider,omitempty"` // extract backend (success only)
	FinalURL string `json:"finalUrl,omitempty"` // resolved URL after redirects (success only)
}

// Record is one audit line: the required Header envelope (embedded, so its
// fields stay flat and greppable), an optional human-readable Status, and at
// most one typed detail body for the record's kind.  Exactly one subsystem owns
// each detail; a reader switches on the non-nil one (or on Tool).
type Record struct {
	Header
	Status   string          `json:"status,omitempty"`
	Command  *CommandDetail  `json:"command,omitempty"`
	Mutation *MutationDetail `json:"mutation,omitempty"`
	Research *ResearchDetail `json:"research,omitempty"`
	Search   *SearchDetail   `json:"search,omitempty"`
	Extract  *ExtractDetail  `json:"extract,omitempty"`
}

// New builds a Record with its required Header.  Time is left zero and stamped
// at Append (the state service's clock is authoritative).  Callers attach at most
// one detail (Command/Mutation/Research/Search/Extract) and may set Status.
func New(runID runid.ID, tool string, decision Decision, dur time.Duration) Record {
	return Record{Header: Header{RunID: runID, Tool: tool, Decision: decision, Duration: Duration(dur)}}
}

// Log is an append-only, self-rotating JSONL writer, safe for concurrent use.
type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	max  int64
	gens int
}

// Open opens (or creates) the audit file in append-only mode.
func Open(path string, opts Options) (*Log, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.Generations <= 0 {
		opts.Generations = DefaultGenerations
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Log{path: path, f: f, size: fi.Size(), max: opts.MaxBytes, gens: opts.Generations}, nil
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// Append writes one record as a single JSON line, stamping Time if unset and
// rejecting a record whose required Header is incomplete, then rotates if the
// file has reached the size threshold — a record is never split across
// generations.
func (l *Log) Append(r Record) error {
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	if err := r.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	n, werr := l.f.Write(b)
	l.size += int64(n)
	if werr != nil {
		return werr
	}
	if l.size >= l.max {
		return l.rotate()
	}
	return nil
}

// rotate shifts audit.jsonl → .1 → … → .N (dropping the oldest) and starts
// a fresh current file.  Callers hold l.mu.
func (l *Log) rotate() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.gens))
	for i := l.gens - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.path, i)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, fmt.Sprintf("%s.%d", l.path, i+1))
		}
	}
	renameErr := os.Rename(l.path, l.path+".1")
	f, err := openAppend(l.path)
	if err != nil {
		return errors.Join(renameErr, err)
	}
	l.f = f
	if renameErr != nil {
		// The old file could not be moved (e.g. held open elsewhere on
		// Windows): keep appending to it rather than losing records.
		fi, statErr := f.Stat()
		if statErr == nil {
			l.size = fi.Size()
		}
		return renameErr
	}
	l.size = 0
	return nil
}

// Close closes the underlying file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
