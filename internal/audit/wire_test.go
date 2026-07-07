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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

func wireRecord(t *testing.T, d Detail) Record {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	rec := New(id, "test_tool", Decision("applied"), 5*time.Millisecond)
	rec.Time = time.Now().UTC()
	rec.Detail = d
	return rec
}

func TestWireRoundTripEveryKind(t *testing.T) {
	exit := 1
	details := []Detail{
		&CommandDetail{Argv: []string{"./gradlew", "build"}, ExitCode: &exit},
		&MutationDetail{Path: "src/a.go", LinesAdded: 3},
		&ResearchDetail{Query: "how do X"},
		&SearchDetail{Query: "X", Engine: "kagi"},
		&ExtractDetail{URL: "https://example.com/doc"},
		&ReadDetail{Paths: []string{"a.env", "b.key"}},
	}
	for _, d := range details {
		t.Run(string(d.Kind()), func(t *testing.T) {
			b, err := json.Marshal(wireRecord(t, d))
			if err != nil {
				t.Fatal(err)
			}
			line := string(b)
			if !strings.Contains(line, `"kind":"`+string(d.Kind())+`"`) || !strings.Contains(line, `"detail":{`) {
				t.Fatalf("wire shape missing kind/detail: %s", line)
			}
			var back Record
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			if back.Detail == nil || back.Detail.Kind() != d.Kind() {
				t.Fatalf("round-trip kind = %v, want %v", back.Detail, d.Kind())
			}
		})
	}
}

func TestWireRoundTripPreservesFields(t *testing.T) {
	rec := wireRecord(t, &ReadDetail{Paths: []string{"one", "two"}})
	b, _ := json.Marshal(rec)
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if r := back.Read(); r == nil || len(r.Paths) != 2 || r.Paths[1] != "two" {
		t.Fatalf("Read detail = %+v", back.Read())
	}
	if back.Mutation() != nil {
		t.Error("accessor for a different kind must be nil")
	}
	if back.Tool != "test_tool" || back.Status != rec.Status {
		t.Errorf("header/status lost: %+v", back.Header)
	}
}

func TestWireHeaderStaysFlat(t *testing.T) {
	b, _ := json.Marshal(wireRecord(t, &MutationDetail{Path: "x"}))
	var flat map[string]any
	if err := json.Unmarshal(b, &flat); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ts", "runId", "tool", "decision", "duration", "kind", "detail"} {
		if _, ok := flat[key]; !ok {
			t.Errorf("wire line missing flat key %q: %s", key, b)
		}
	}
	if _, ok := flat["mutation"]; ok {
		t.Error("legacy per-kind top-level key present in new wire format")
	}
}

func TestLegacyLinesStillDecode(t *testing.T) {
	legacy := `{"ts":"2026-07-07T12:00:00Z","runId":"0197f2e6-8f2a-7c3b-9d4e-1a2b3c4d5e6f","tool":"apply_diff","decision":"applied","duration":"12ms","mutation":{"path":"src/a.go","linesAdded":2}}`
	var rec Record
	if err := json.Unmarshal([]byte(legacy), &rec); err != nil {
		t.Fatal(err)
	}
	if m := rec.Mutation(); m == nil || m.Path != "src/a.go" || m.LinesAdded != 2 {
		t.Fatalf("legacy decode = %+v", rec.Mutation())
	}
}

func TestUnknownKindRejected(t *testing.T) {
	bad := `{"ts":"2026-07-07T12:00:00Z","runId":"0197f2e6-8f2a-7c3b-9d4e-1a2b3c4d5e6f","tool":"x","decision":"y","duration":"1ms","kind":"surprise","detail":{}}`
	var rec Record
	if err := json.Unmarshal([]byte(bad), &rec); err == nil {
		t.Error("unknown kind accepted; want fail-closed rejection")
	}
}
