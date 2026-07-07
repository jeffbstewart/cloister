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

package shield

import (
	"testing"
	"testing/fstest"
)

// load builds a Shield from a map of path → content.
func load(t *testing.T, files map[string]string) *Shield {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	s, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func TestGitignoreHidesAiignoreStrips(t *testing.T) {
	s := load(t, map[string]string{
		".gitignore": "build/\n*.tmp\n",
		".aiignore":  "secrets/\nprivate-notes.md\n",
		"src/a.go":   "code",
	})
	cases := []struct {
		rel   string
		isDir bool
		want  Visibility
	}{
		{"src/a.go", false, Visible},
		{"build", true, Hidden},
		{"build/out.jar", false, Hidden},  // ancestor exclusion
		{"scratch.tmp", false, Hidden},    // unanchored file pattern, any depth
		{"src/deep/x.tmp", false, Hidden}, //
		{"secrets", true, Stripped},       //
		{"secrets/prod.env", false, Stripped},
		{"private-notes.md", false, Stripped},
	}
	for _, c := range cases {
		if got := s.Visibility(c.rel, c.isDir); got != c.want {
			t.Errorf("Visibility(%q, dir=%v) = %v, want %v", c.rel, c.isDir, got, c.want)
		}
		wantRead := c.want == Visible
		if got := s.MayRead(c.rel, c.isDir); got != wantRead {
			t.Errorf("MayRead(%q) = %v, want %v", c.rel, got, wantRead)
		}
	}
}

func TestHiddenBeatsStrippedWhenBothMatch(t *testing.T) {
	s := load(t, map[string]string{
		".gitignore": "out/\n",
		".aiignore":  "out/\n",
	})
	if got := s.Visibility("out/gen.go", false); got != Hidden {
		t.Errorf("both-matched path = %v, want Hidden (the stronger statement)", got)
	}
}

func TestIgnoreFilesAlwaysVisibleAndGated(t *testing.T) {
	// Even patterns that would match the ignore files themselves do not
	// hide them, at any depth.
	s := load(t, map[string]string{
		".gitignore":     "*\n", // pathological: ignore everything
		".aiignore":      "*ignore\n",
		"sub/.aiignore":  "x\n",
		"sub/.gitignore": "y\n",
	})
	for _, rel := range []string{".gitignore", ".aiignore", "sub/.aiignore", "sub/.gitignore"} {
		if got := s.Visibility(rel, false); got != Visible {
			t.Errorf("Visibility(%q) = %v, want Visible always", rel, got)
		}
		if !s.MayRead(rel, false) {
			t.Errorf("MayRead(%q) = false, want true always", rel)
		}
	}
	// Writes to .aiignore gate; .gitignore and ordinary files do not.
	if !s.MustGate("sub/.aiignore") || !s.MustGate(".aiignore") {
		t.Error(".aiignore writes must gate")
	}
	if s.MustGate(".gitignore") || s.MustGate("src/a.go") {
		t.Error("only .aiignore writes gate")
	}
}

func TestNegationLastMatchWins(t *testing.T) {
	s := load(t, map[string]string{
		".gitignore": "*.log\n!keep.log\n",
	})
	if got := s.Visibility("debug.log", false); got != Hidden {
		t.Errorf("debug.log = %v, want Hidden", got)
	}
	if got := s.Visibility("keep.log", false); got != Visible {
		t.Errorf("keep.log = %v, want Visible (re-included)", got)
	}
}

func TestNoReincludeUnderExcludedDir(t *testing.T) {
	// git rule: a file cannot be re-included if a parent dir is excluded.
	s := load(t, map[string]string{
		".gitignore": "build/\n!build/keep.txt\n",
	})
	if got := s.Visibility("build/keep.txt", false); got != Hidden {
		t.Errorf("re-include under excluded dir = %v, want Hidden", got)
	}
}

func TestAnchoring(t *testing.T) {
	s := load(t, map[string]string{
		".gitignore": "/rooted.txt\ndocs/gen\nplain.txt\n",
	})
	cases := []struct {
		rel  string
		want Visibility
	}{
		{"rooted.txt", Hidden},      // leading slash: root only
		{"sub/rooted.txt", Visible}, //
		{"docs/gen", Hidden},        // middle slash: anchored to root
		{"x/docs/gen", Visible},     //
		{"plain.txt", Hidden},       // no slash: any depth
		{"a/b/plain.txt", Hidden},   //
	}
	for _, c := range cases {
		if got := s.Visibility(c.rel, false); got != c.want {
			t.Errorf("Visibility(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

func TestDirOnlyPattern(t *testing.T) {
	s := load(t, map[string]string{
		".gitignore": "cache/\n",
	})
	if got := s.Visibility("cache", true); got != Hidden {
		t.Errorf("cache dir = %v, want Hidden", got)
	}
	if got := s.Visibility("cache", false); got != Visible {
		t.Errorf("cache FILE = %v, want Visible (dir-only pattern)", got)
	}
	if got := s.Visibility("cache/entry.bin", false); got != Hidden {
		t.Errorf("cache/entry.bin = %v, want Hidden via ancestor", got)
	}
}

func TestDoubleStar(t *testing.T) {
	s := load(t, map[string]string{
		".gitignore": "**/target\nout/**\na/**/b\n",
	})
	cases := []struct {
		rel   string
		isDir bool
		want  Visibility
	}{
		{"target", true, Hidden},
		{"deep/nested/target", true, Hidden},
		{"out/x.txt", false, Hidden},
		// Deliberate divergence from git, documented: out/** hides `out`
		// itself too — a Hidden husk directory would be noise.
		{"out", true, Hidden},
		{"a/b", false, Hidden},     // ** spans zero segments
		{"a/x/y/b", false, Hidden}, // and many
		{"a/x/c", false, Visible},
	}
	for _, c := range cases {
		if got := s.Visibility(c.rel, c.isDir); got != c.want {
			t.Errorf("Visibility(%q, dir=%v) = %v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

func TestNestedIgnoreFilesDeeperWins(t *testing.T) {
	s := load(t, map[string]string{
		".gitignore":     "*.gen\n",
		"sub/.gitignore": "!special.gen\n",
	})
	if got := s.Visibility("other.gen", false); got != Hidden {
		t.Errorf("other.gen = %v, want Hidden", got)
	}
	if got := s.Visibility("sub/special.gen", false); got != Visible {
		t.Errorf("sub/special.gen = %v, want Visible (deeper file re-includes)", got)
	}
	if got := s.Visibility("special.gen", false); got != Hidden {
		t.Errorf("root special.gen = %v, want Hidden (sub's rules do not reach up)", got)
	}
}

func TestCommentsBlanksAndEscapes(t *testing.T) {
	s := load(t, map[string]string{
		".gitignore": "# a comment\n\n\\#literal\nname\\ with\\ space \n",
	})
	if got := s.Visibility("#literal", false); got != Hidden {
		t.Errorf("escaped-hash pattern = %v, want Hidden", got)
	}
	if got := s.Visibility("# a comment", false); got != Visible {
		t.Errorf("comment line must not be a pattern")
	}
}

func TestEmptyWorkspace(t *testing.T) {
	s := load(t, map[string]string{"README.md": "x"})
	if got := s.Visibility("anything/at/all.go", false); got != Visible {
		t.Errorf("no ignore files: %v, want Visible", got)
	}
}

func TestGitDirNotDescended(t *testing.T) {
	// A .gitignore inside .git must not contribute patterns.
	s := load(t, map[string]string{
		".git/info/.gitignore": "everything\n",
		"everything":           "x",
	})
	if got := s.Visibility("everything", false); got != Visible {
		t.Errorf(".git-internal ignore file leaked patterns: %v", got)
	}
}
