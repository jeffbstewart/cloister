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

package digest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeffbstewart/cloister/internal/runid"
)

// loadLog reads a captured build log from testdata/ (shared by the
// per-parser test files).
func loadLog(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGet(t *testing.T) {
	for _, name := range []string{"", "gradle", "gotest", "generic"} {
		p, ok := Get(name)
		if !ok || p == nil {
			t.Errorf("Get(%q): parser not found", name)
		}
	}
	if _, ok := Get("maven"); ok {
		t.Error("Get(\"maven\") should be unknown")
	}
}

func TestKnown(t *testing.T) {
	if !Known("") {
		t.Error("Known(\"\") = false; empty should default to generic")
	}
	if !Known("gotest") {
		t.Error("Known(\"gotest\") = false")
	}
	if Known("maven") {
		t.Error("Known(\"maven\") = true; want false")
	}
}

func TestHint(t *testing.T) {
	runID, err := runid.Parse("0197f2e6-8f2a-7c3b-9d4e-1a2b3c4d5e6f")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		f    Findings
		want string
	}{
		{
			name: "clean run points at get_log",
			f:    Findings{},
			want: `Full log via get_log(runId="0197f2e6-8f2a-7c3b-9d4e-1a2b3c4d5e6f").`,
		},
		{
			name: "parsed vs reported test counts",
			f:    Findings{FailedTests: []FailedTest{{Test: "a"}, {Test: "b"}}, TestsReported: 5},
			want: "2 failed tests parsed of 5 reported",
		},
		{
			name: "reported never below parsed",
			f:    Findings{FailedTests: []FailedTest{{Test: "a"}, {Test: "b"}}},
			want: "2 failed tests parsed of 2 reported",
		},
		{
			name: "compile error count",
			f:    Findings{CompileErrors: []CompileError{{File: "a.kt"}, {File: "b.kt"}, {File: "c.kt"}}},
			want: "3 compile errors parsed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.f.Hint(runID)
			if !strings.Contains(got, tt.want) {
				t.Errorf("Hint = %q, want containing %q", got, tt.want)
			}
		})
	}
}

func TestCleanPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"file:///workspace/src/main/kotlin/Foo.kt", "src/main/kotlin/Foo.kt"},
		{"/workspace/src/Foo.java", "src/Foo.java"},
		{"./matcher.go", "matcher.go"},
		{"src/Foo.kt", "src/Foo.kt"},
	}
	for _, tt := range tests {
		if got := cleanPath(tt.in); got != tt.want {
			t.Errorf("cleanPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFirstIndented(t *testing.T) {
	lines := []string{"Header FAILED", "", "    the assertion message", "unindented"}
	if got := firstIndented(lines, 1); got != "the assertion message" {
		t.Errorf("firstIndented = %q", got)
	}
	// An unindented line before any indented one means "no message block".
	if got := firstIndented([]string{"Header FAILED", "BUILD FAILED"}, 1); got != "" {
		t.Errorf("firstIndented = %q, want empty", got)
	}
	if got := firstIndented(nil, 0); got != "" {
		t.Errorf("firstIndented(nil) = %q, want empty", got)
	}
}
