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

package codename

import (
	"regexp"
	"strings"
	"testing"
)

// wordShape is what makes a codename safe everywhere it is used: a git
// branch segment, a path component, a URL, a log line — no escaping, no
// case folding surprises, nothing a shell would look at twice.
var wordShape = regexp.MustCompile(`^[a-z]{3,10}$`)

// TestVocabularyIsWellFormed guards the lists themselves: the sizes the
// Combinations constant claims, the charset every consumer relies on,
// no duplicates within a list, and no word shared BETWEEN the lists —
// which is what keeps "swift-swift" impossible.
func TestVocabularyIsWellFormed(t *testing.T) {
	if len(adjectives) != 50 || len(animals) != 50 {
		t.Fatalf("lists are %d adjectives and %d animals, want 50 and 50 (Combinations claims %d)",
			len(adjectives), len(animals), Combinations)
	}
	seen := map[string]string{}
	for list, words := range map[string][]string{"adjectives": adjectives, "animals": animals} {
		for _, w := range words {
			if !wordShape.MatchString(w) {
				t.Errorf("%s: %q is not 3-10 lowercase letters — it must be safe as a branch and path segment", list, w)
			}
			if where, dup := seen[w]; dup {
				t.Errorf("%q appears in both %s and %s; a pair could stutter", w, where, list)
			}
			seen[w] = list
		}
	}
}

// TestPickIsDeterministicAndCovers: the chooser is injected, so a test
// can name the exact pair — and the extremes must be reachable, which
// catches an off-by-one in the index arithmetic.
func TestPickIsDeterministicAndCovers(t *testing.T) {
	first := Pick(func(int) int { return 0 })
	if want := adjectives[0] + "-" + animals[0]; first != want {
		t.Errorf("Pick(0) = %q, want %q", first, want)
	}
	last := Pick(func(n int) int { return n - 1 })
	if want := adjectives[49] + "-" + animals[49]; last != want {
		t.Errorf("Pick(n-1) = %q, want %q — the last entry of each list must be reachable", last, want)
	}
}

// TestNewIsShapedAndVaried: real draws are well formed, and over enough
// of them the generator is not returning one constant (a chooser wired
// to a dead source would still pass every shape check).
func TestNewIsShapedAndVaried(t *testing.T) {
	shape := regexp.MustCompile(`^[a-z]{3,10}-[a-z]{3,10}$`)
	distinct := map[string]bool{}
	for i := 0; i < 200; i++ {
		got := New()
		if !shape.MatchString(got) {
			t.Fatalf("New() = %q, want adjective-animal", got)
		}
		if strings.Count(got, "-") != 1 {
			t.Fatalf("New() = %q, want exactly one hyphen", got)
		}
		distinct[got] = true
	}
	if len(distinct) < 50 {
		t.Errorf("200 draws produced only %d distinct codenames — the source looks stuck", len(distinct))
	}
}
