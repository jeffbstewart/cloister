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

// Package audit appends one JSONL record per action call — including
// rejected ones — to /state/audit.jsonl.
//
// The vocabulary in this file is worker-type agnostic: the required Header
// envelope, the Record line, and the Decision, Limit, and Duration types.
// Each worker type declares its own decisions, limits, and detail bodies in
// its own file (builder.go, scribe.go, scholar.go); the self-rotating writer
// lives in rotation.go.
package audit

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

// Decision is the terminal disposition of one audited op.  It is a named string
// type — an OPEN enum: each worker type declares its own decisions as
// audit.Decision consts in its own file.  That keeps callers type-safe without
// the shared envelope having to enumerate every worker's outcomes.
type Decision string

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
// companion to a cap decision.  Empty unless a limit was the cause.  It keeps
// the Decision enum coarse while still letting a reader tell a persistent
// daily quota ("the cell is out of budget") from a per-invocation ceiling:
// grep the limit, not a proliferation of decisions.  Like decisions, Limit
// consts are declared in the owning worker's file.
type Limit string

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

// Kind names a detail body's type — the wire discriminator.
type Kind string

// The detail kinds, one per owning worker file.
const (
	KindCommand  Kind = "command"
	KindMutation Kind = "mutation"
	KindResearch Kind = "research"
	KindSearch   Kind = "search"
	KindExtract  Kind = "extract"
	KindRead     Kind = "read"
)

// Detail is one record's typed body.  Each worker's file declares its
// detail types; the single Detail field makes "at most one" true by
// construction rather than by convention.
type Detail interface {
	Kind() Kind
}

// decodeDetail is the explicit decode table — deliberately a visible
// package-local switch, not a registry: reading this function answers
// "what kinds exist" completely.
func decodeDetail(k Kind) (Detail, error) {
	switch k {
	case KindCommand:
		return &CommandDetail{}, nil
	case KindMutation:
		return &MutationDetail{}, nil
	case KindResearch:
		return &ResearchDetail{}, nil
	case KindSearch:
		return &SearchDetail{}, nil
	case KindExtract:
		return &ExtractDetail{}, nil
	case KindRead:
		return &ReadDetail{}, nil
	}
	return nil, fmt.Errorf("audit: unknown detail kind %q", k)
}

// Record is one audit line: the required Header envelope (flat and
// greppable on the wire), an optional human-readable Status, and at most
// one typed Detail — enforced structurally by the single field.  On the
// wire the detail nests under "detail" with its "kind" alongside:
//
//	{"ts":…,"runId":…,"tool":"apply_diff","decision":"applied","duration":"12ms",
//	 "kind":"mutation","detail":{"path":"src/a.go",…}}
//
// Readers use the typed accessors (Mutation(), Command(), …), which
// html/template resolves exactly like the former struct fields.
type Record struct {
	Header
	Status string
	Detail Detail
}

// Typed accessors: the detail if it is that kind, else nil.  Returning
// the pointer keeps producer ergonomics — a handler may set the detail
// once and enrich it later through the accessor.
func (r Record) Command() *CommandDetail   { d, _ := r.Detail.(*CommandDetail); return d }
func (r Record) Mutation() *MutationDetail { d, _ := r.Detail.(*MutationDetail); return d }
func (r Record) Research() *ResearchDetail { d, _ := r.Detail.(*ResearchDetail); return d }
func (r Record) Search() *SearchDetail     { d, _ := r.Detail.(*SearchDetail); return d }
func (r Record) Extract() *ExtractDetail   { d, _ := r.Detail.(*ExtractDetail); return d }
func (r Record) Read() *ReadDetail         { d, _ := r.Detail.(*ReadDetail); return d }

// recordWire is the JSON shape: flat header + status + kind + detail.
type recordWire struct {
	Header
	Status string          `json:"status,omitempty"`
	Kind   Kind            `json:"kind,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

// MarshalJSON writes the flat envelope with the kind-discriminated detail.
func (r Record) MarshalJSON() ([]byte, error) {
	w := recordWire{Header: r.Header, Status: r.Status}
	if r.Detail != nil {
		w.Kind = r.Detail.Kind()
		b, err := json.Marshal(r.Detail)
		if err != nil {
			return nil, err
		}
		w.Detail = b
	}
	return json.Marshal(w)
}

// UnmarshalJSON decodes via the kind table.  Lines from before the
// tagged-union format (per-kind top-level keys) still decode — the
// deployed cells' ledgers predate it.
func (r *Record) UnmarshalJSON(b []byte) error {
	var w recordWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	r.Header, r.Status, r.Detail = w.Header, w.Status, nil
	if w.Kind != "" {
		d, err := decodeDetail(w.Kind)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(w.Detail, d); err != nil {
			return err
		}
		r.Detail = d
		return nil
	}
	return r.unmarshalLegacy(b)
}

// unmarshalLegacy reads the pre-union shape: one optional top-level key
// per kind.  Keep until no deployed ledger predates the union format.
func (r *Record) unmarshalLegacy(b []byte) error {
	var old struct {
		Command  *CommandDetail  `json:"command"`
		Mutation *MutationDetail `json:"mutation"`
		Research *ResearchDetail `json:"research"`
		Search   *SearchDetail   `json:"search"`
		Extract  *ExtractDetail  `json:"extract"`
		Read     *ReadDetail     `json:"read"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		return err
	}
	switch {
	case old.Command != nil:
		r.Detail = old.Command
	case old.Mutation != nil:
		r.Detail = old.Mutation
	case old.Research != nil:
		r.Detail = old.Research
	case old.Search != nil:
		r.Detail = old.Search
	case old.Extract != nil:
		r.Detail = old.Extract
	case old.Read != nil:
		r.Detail = old.Read
	}
	return nil
}

// New builds a Record with its required Header.  Time is left zero here and
// stamped by Append: the writer's clock is authoritative.  Callers set
// Detail (at most one, structurally) and may set Status.
func New(runID runid.ID, tool string, decision Decision, dur time.Duration) Record {
	return Record{Header: Header{RunID: runID, Tool: tool, Decision: decision, Duration: Duration(dur)}}
}
