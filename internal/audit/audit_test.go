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
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

func mustRunID(t *testing.T) runid.ID {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatalf("runid.New() failed: %v", err)
	}
	return id
}

func readRecords(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d is not valid JSON: %v (%q)", len(out)+1, err, sc.Text())
		}
		out = append(out, r)
	}
	return out
}

// TestAppendRejectsIncompleteHeader: the required core is enforced at Append.
func TestAppendRejectsIncompleteHeader(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "audit.jsonl"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Append(New(mustRunID(t), "", DecisionRun, 0)); err == nil {
		t.Error("append with empty tool should fail Validate")
	}
	if err := l.Append(New(mustRunID(t), "test", "", 0)); err == nil {
		t.Error("append with empty decision should fail Validate")
	}
}

// TestDurationJSON: a Duration is a time.Duration in memory, a readable string on
// the wire.
func TestDurationJSON(t *testing.T) {
	b, err := json.Marshal(Duration(1500 * time.Millisecond))
	if err != nil || string(b) != `"1.5s"` {
		t.Errorf("duration marshal = %s (%v), want \"1.5s\"", b, err)
	}
	var d Duration
	if err := json.Unmarshal([]byte(`"412ms"`), &d); err != nil || d.Std() != 412*time.Millisecond {
		t.Errorf("duration round-trip = %v (%v), want 412ms", d.Std(), err)
	}
}
