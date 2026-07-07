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

package approval

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

func TestResolved(t *testing.T) {
	cases := []struct {
		d    Decision
		want bool
	}{
		{Pending, false},
		{"", false}, // zero value: not yet registered, still not final
		{Approved, true},
		{Rejected, true},
		{Timeout, true},
	}
	for _, c := range cases {
		if got := c.d.Resolved(); got != c.want {
			t.Errorf("Decision(%q).Resolved() = %v, want %v", c.d, got, c.want)
		}
	}
}

// TestRecordJSON: times are time.Time in memory; an undecided op omits
// decidedAt on the wire (omitzero) and round-trips intact.
func TestRecordJSON(t *testing.T) {
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 7, 6, 15, 4, 5, 0, time.UTC)
	pending := Record{OpID: id, Tool: "apply_diff", Path: "src/main.go", CreatedAt: created, Decision: Pending}

	b, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "decidedAt") {
		t.Errorf("undecided record must omit decidedAt: %s", b)
	}

	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != pending {
		t.Errorf("round trip: %+v != %+v", back, pending)
	}

	decided := pending
	decided.Decision = Approved
	decided.DecidedAt = created.Add(90 * time.Second)
	b, err = json.Marshal(decided)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "decidedAt") {
		t.Errorf("decided record must carry decidedAt: %s", b)
	}
}
