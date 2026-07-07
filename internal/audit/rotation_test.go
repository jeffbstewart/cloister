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

package audit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

func TestAppendWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}

	id, err := runid.Parse("0197f2e6-8f2a-7c3b-9d4e-1a2b3c4d5e6f")
	if err != nil {
		t.Fatal(err)
	}
	exit := 1
	rec := New(id, "test", DecisionRun, 81237*time.Millisecond)
	rec.Status = "failed"
	rec.Command = &CommandDetail{
		Params:   map[string]string{"filter": "TranscodeMatcherServiceTest"},
		Argv:     []string{"./gradlew", "--offline", "test", "--tests", "TranscodeMatcherServiceTest"},
		ExitCode: &exit,
		LogPath:  "/state/logs/" + id.String() + ".log",
		LogBytes: 481223,
	}
	if err := l.Append(rec); err != nil {
		t.Fatal(err)
	}
	// A record with no detail is fine, but the Header (incl. RunID) is required,
	// so a rejection carries its own event id.
	if err := l.Append(New(mustRunID(t), "test", DecisionRejectedParam, 0)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	recs := readRecords(t, path)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	r := recs[0]
	if r.Time.IsZero() {
		t.Error("ts not stamped")
	}
	if r.RunID != id {
		t.Errorf("runId round-trip: got %s, want %s", r.RunID, id)
	}
	if r.Decision != DecisionRun || r.Duration.Std() != 81237*time.Millisecond || r.Status != "failed" {
		t.Errorf("header round-trip mismatch: %+v", r.Header)
	}
	if r.Command == nil || r.Command.ExitCode == nil || *r.Command.ExitCode != 1 {
		t.Errorf("action round-trip mismatch: %+v", r.Command)
	}
	if r.Command == nil || len(r.Command.Argv) != 5 || r.Command.Argv[0] != "./gradlew" || r.Command.Argv[4] != "TranscodeMatcherServiceTest" {
		t.Errorf("argv round-trip mismatch: %+v", r.Command)
	}
	if recs[1].Decision != DecisionRejectedParam {
		t.Errorf("rejected record decision = %q", recs[1].Decision)
	}
	if recs[1].Command != nil {
		t.Error("rejected record must have no command detail")
	}
	if recs[1].RunID.IsZero() {
		t.Error("every record carries a runId now (its event id)")
	}
}

// TestAppendStampsTime: the writer's clock is authoritative — a caller-supplied
// timestamp is overwritten, so the trail can never be backdated.
func TestAppendStampsTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec := New(mustRunID(t), "build", DecisionRun, 0)
	rec.Time = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC) // attempted backdate
	before := time.Now().UTC().Add(-time.Minute)
	if err := l.Append(rec); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, path)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Time.Before(before) {
		t.Errorf("caller-supplied timestamp survived: %s", recs[0].Time)
	}
}

// TestReopenAppends: restarting the server must never truncate the audit
// trail — it is append-only.
func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	l1, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Append(New(mustRunID(t), "build", DecisionRun, 0)); err != nil {
		t.Fatal(err)
	}
	l1.Close()

	l2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Append(New(mustRunID(t), "test", DecisionRun, 0)); err != nil {
		t.Fatal(err)
	}
	l2.Close()

	recs := readRecords(t, path)
	if len(recs) != 2 {
		t.Fatalf("got %d records after reopen, want 2 (file was truncated?)", len(recs))
	}
	if recs[0].Tool != "build" || recs[1].Tool != "test" {
		t.Errorf("order wrong: %+v", recs)
	}
}

// TestRotation: past MaxBytes the current file shifts into numbered
// generations, the oldest generation drops, and no record is ever lost
// inside the retained window.
func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	l, err := Open(path, Options{MaxBytes: 300, Generations: 2})
	if err != nil {
		t.Fatal(err)
	}
	const total = 20
	for i := 0; i < total; i++ {
		if err := l.Append(New(mustRunID(t), "build", DecisionRun, 0)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	if fi, err := os.Stat(path); err != nil || fi.Size() >= 300+200 {
		t.Errorf("current file not rotated (err=%v size=%d)", err, fi.Size())
	}
	for _, gen := range []string{path + ".1", path + ".2"} {
		recs := readRecords(t, gen)
		if len(recs) == 0 {
			t.Errorf("%s missing or empty; want a retained generation", filepath.Base(gen))
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("generation .3 exists; oldest must be dropped at Generations=2")
	}

	// Everything still on disk is valid JSONL and sums to <= total.
	kept := len(readRecords(t, path)) + len(readRecords(t, path+".1")) + len(readRecords(t, path+".2"))
	if kept == 0 || kept > total {
		t.Errorf("kept %d records of %d written", kept, total)
	}
}

// TestApplyDefaults: zero or negative fields take the package defaults;
// explicit values survive.
func TestApplyDefaults(t *testing.T) {
	var o Options
	o.ApplyDefaults()
	if o.MaxBytes != DefaultMaxBytes || o.Generations != DefaultGenerations {
		t.Errorf("zero Options = %+v, want defaults", o)
	}
	o = Options{MaxBytes: 42, Generations: 3}
	o.ApplyDefaults()
	if o.MaxBytes != 42 || o.Generations != 3 {
		t.Errorf("explicit Options overwritten: %+v", o)
	}
}
