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

// Record is one audit line: the required Header envelope (embedded, so its
// fields stay flat and greppable), an optional human-readable Status, and at
// most one typed detail body for the record's kind.  Exactly one worker type
// owns each detail; a reader switches on the non-nil one (or on Tool).
type Record struct {
	Header
	Status   string          `json:"status,omitempty"`
	Command  *CommandDetail  `json:"command,omitempty"`
	Mutation *MutationDetail `json:"mutation,omitempty"`
	Research *ResearchDetail `json:"research,omitempty"`
	Search   *SearchDetail   `json:"search,omitempty"`
	Extract  *ExtractDetail  `json:"extract,omitempty"`
}

// New builds a Record with its required Header.  Time is left zero here and
// stamped by Append: the writer's clock is authoritative.  Callers attach at
// most one detail (Command/Mutation/Research/Search/Extract) and may set Status.
func New(runID runid.ID, tool string, decision Decision, dur time.Duration) Record {
	return Record{Header: Header{RunID: runID, Tool: tool, Decision: decision, Duration: Duration(dur)}}
}
