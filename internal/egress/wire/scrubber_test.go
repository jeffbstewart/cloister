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

package wire

import "testing"

func TestScrubRedactsAllSecrets(t *testing.T) {
	s := NewScrubber("sekret-kagi", "sekret-brave")
	in := `upstream 401: {"error":"bad token sekret-kagi"} (also sekret-brave)`
	got := s.Scrub(in)
	if got != `upstream 401: {"error":"bad token [redacted]"} (also [redacted])` {
		t.Errorf("Scrub() = %q", got)
	}
}

// TestScrubIgnoresEmptySecrets: a cell with no Brave key must not redact
// every empty string.
func TestScrubIgnoresEmptySecrets(t *testing.T) {
	s := NewScrubber("", "only-key", "")
	if got := s.Scrub("abc"); got != "abc" {
		t.Errorf("empty secrets corrupted text: %q", got)
	}
	if got := s.Scrub("x only-key y"); got != "x [redacted] y" {
		t.Errorf("real secret survived: %q", got)
	}
}

// TestScrubNilReceiver: a nil scrubber is a documented no-op so callers need
// not special-case it.
func TestScrubNilReceiver(t *testing.T) {
	var s *Scrubber
	if got := s.Scrub("text"); got != "text" {
		t.Errorf("nil scrubber altered text: %q", got)
	}
}
