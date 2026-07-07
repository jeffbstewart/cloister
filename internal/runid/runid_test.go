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

package runid

import (
	"encoding/json"
	"testing"
	"time"
)

const fixture = "0197f2e6-8f2a-7c3b-9d4e-1a2b3c4d5e6f"

func mustNew(t *testing.T) ID {
	t.Helper()
	id, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return id
}

func TestNewProducesValidIDs(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := mustNew(t)
		if _, err := Parse(id.String()); err != nil {
			t.Fatalf("New() produced an ID its own Parse rejects: %v", err)
		}
	}
}

func TestNewIsCollisionless(t *testing.T) {
	const n = 100_000
	seen := make(map[ID]bool, n)
	for i := 0; i < n; i++ {
		id := mustNew(t)
		if seen[id] {
			t.Fatalf("collision after %d ids: %s", i, id)
		}
		seen[id] = true
	}
}

// TestShellSafeAlphabet: canonical form must contain only [0-9a-f-] — no
// shell metacharacters, spaces, or path separators.
func TestShellSafeAlphabet(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := mustNew(t).String()
		if len(id) != 36 {
			t.Fatalf("length %d, want 36: %q", len(id), id)
		}
		for _, c := range id {
			if !(c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Fatalf("unsafe character %q in id %q", c, id)
			}
		}
	}
}

// TestSortableByTime: UUIDv7's leading timestamp makes later ids compare
// greater as plain strings.
func TestSortableByTime(t *testing.T) {
	a := mustNew(t)
	time.Sleep(5 * time.Millisecond)
	b := mustNew(t)
	if !(a.String() < b.String()) {
		t.Errorf("ids not time-ordered: %s !< %s", a, b)
	}
}

func TestZeroValue(t *testing.T) {
	var zero ID
	if !zero.IsZero() {
		t.Error("zero value must report IsZero")
	}
	if zero.String() != "" {
		t.Errorf("zero String() = %q, want empty", zero.String())
	}
	if mustNew(t).IsZero() {
		t.Error("New() must never be zero")
	}
}

func TestParseRejectsUntrustedInput(t *testing.T) {
	bad := []string{
		"",
		"not-a-uuid",
		"../../etc/passwd",
		fixture + "/../x",
		"0197F2E6-8F2A-7C3B-9D4E-1A2B3C4D5E6F", // uppercase
		"0197f2e6-8f2a-4c3b-9d4e-1a2b3c4d5e6f", // v4, not v7
		"0197f2e6-8f2a-7c3b-1d4e-1a2b3c4d5e6f", // bad variant nibble
		fixture + " ",                          // trailing space
		"$(rm -rf /)",
		fixture + ".log", // suffix smuggling
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted; want rejection", s)
		}
	}
	id, err := Parse(fixture)
	if err != nil || id.String() != fixture {
		t.Errorf("Parse(%q) = %q, %v", fixture, id, err)
	}
}

func TestShard(t *testing.T) {
	var zero ID
	if got := zero.Shard(); got != "00" {
		t.Errorf("zero ID Shard() = %q, want %q", got, "00")
	}
	id, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if got := id.Shard(); got != fixture[len(fixture)-2:] {
		t.Errorf("Shard() = %q, want last two chars %q", got, fixture[len(fixture)-2:])
	}
}

func TestJSONRoundTrip(t *testing.T) {
	orig := mustNew(t)
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back ID
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != orig {
		t.Errorf("round trip: %s != %s", back, orig)
	}

	var rejected ID
	if err := json.Unmarshal([]byte(`"not-a-uuid"`), &rejected); err == nil {
		t.Error("unmarshal accepted an invalid id")
	}

	var zero ID
	if err := json.Unmarshal([]byte(`""`), &zero); err != nil || !zero.IsZero() {
		t.Errorf("empty string must decode to the zero ID (err=%v)", err)
	}
}
