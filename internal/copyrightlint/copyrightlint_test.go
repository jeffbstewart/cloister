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

package copyrightlint

import (
	"strings"
	"testing"
	"testing/fstest"
)

const testOwner = "Jeffrey B. Stewart"

// header renders an owner header with the given year field, in the shape
// PR #26 stamped across the repo.
func header(years string) string {
	return "// Copyright " + years + " " + testOwner + "\n//\n// Licensed under the Apache License, Version 2.0 (the \"License\");\n// you may not use this file except in compliance with the License.\n\npackage x\n"
}

func testConfig() Config {
	return Config{
		Owner:      testOwner,
		Exclude:    []string{"**/*.md", "**/testdata/**", "go.mod"},
		ThirdParty: []string{"third_party/**"},
	}
}

// check runs Check over a MapFS with a fixed submission year of 2027.
func check(t *testing.T, cfg Config, files map[string]string, changed ...string) []Finding {
	t.Helper()
	fsys := fstest.MapFS{}
	var paths []string
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
		paths = append(paths, name)
	}
	ch := make(map[string]bool, len(changed))
	for _, c := range changed {
		ch[c] = true
	}
	got, err := Check(cfg, fsys, paths, ch, 2027)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return got
}

func problemsOf(fs []Finding) map[string]Problem {
	m := make(map[string]Problem, len(fs))
	for _, f := range fs {
		m[f.Path] = f.Problem
	}
	return m
}

func TestCleanFilePasses(t *testing.T) {
	got := check(t, testConfig(), map[string]string{
		"a.go": header("2026"),
	})
	if len(got) != 0 {
		t.Fatalf("unexpected findings: %v", got)
	}
}

func TestMissingHeader(t *testing.T) {
	got := check(t, testConfig(), map[string]string{
		"a.go": "package x\n",
	})
	if p := problemsOf(got)["a.go"]; p != ProblemMissingHeader {
		t.Fatalf("a.go problem = %q, want %q (findings: %v)", p, ProblemMissingHeader, got)
	}
}

func TestMissingLicenseReference(t *testing.T) {
	got := check(t, testConfig(), map[string]string{
		"a.go": "// Copyright 2026 " + testOwner + "\npackage x\n",
	})
	if p := problemsOf(got)["a.go"]; p != ProblemMissingLicense {
		t.Fatalf("a.go problem = %q, want %q (findings: %v)", p, ProblemMissingLicense, got)
	}
}

func TestChangedFileYearCurrency(t *testing.T) {
	files := map[string]string{
		"stale.go":     header("2026"),
		"current.go":   header("2027"),
		"range.go":     header("2026-2027"),
		"untouched.go": header("2026"),
	}
	got := check(t, testConfig(), files, "stale.go", "current.go", "range.go")
	problems := problemsOf(got)
	if p := problems["stale.go"]; p != ProblemStaleYear {
		t.Errorf("stale.go problem = %q, want %q", p, ProblemStaleYear)
	}
	for _, ok := range []string{"current.go", "range.go", "untouched.go"} {
		if p, found := problems[ok]; found {
			t.Errorf("%s flagged %q; want clean", ok, p)
		}
	}
}

func TestMalformedYears(t *testing.T) {
	for name, years := range map[string]string{
		"list":       "2026, 2027",
		"descending": "2027-2026",
		"flat-range": "2026-2026",
		"two-digit":  "26",
	} {
		t.Run(name, func(t *testing.T) {
			got := check(t, testConfig(), map[string]string{"a.go": header(years)})
			if p := problemsOf(got)["a.go"]; p != ProblemMalformedYear {
				t.Fatalf("years %q: problem = %q, want %q (findings: %v)", years, p, ProblemMalformedYear, got)
			}
		})
	}
}

func TestForeignCopyright(t *testing.T) {
	foreign := "// Copyright 2020 Example Industries\n// Licensed under the MIT license.\ncode\n"
	got := check(t, testConfig(), map[string]string{
		"lib.go":                "" + foreign,
		"third_party/vendor.go": foreign,
	}, "lib.go", "third_party/vendor.go")
	problems := problemsOf(got)
	if p := problems["lib.go"]; p != ProblemForeignHolder {
		t.Errorf("lib.go problem = %q, want %q", p, ProblemForeignHolder)
	}
	// Allowlisted: presence satisfied by the foreign line, never year-checked
	// even though it is in the changed set, and no Apache reference required.
	if p, found := problems["third_party/vendor.go"]; found {
		t.Errorf("third_party/vendor.go flagged %q; want clean", p)
	}
}

func TestExcludedBinaryAndEmptySkipped(t *testing.T) {
	got := check(t, testConfig(), map[string]string{
		"README.md":           "no header here",
		"pkg/testdata/f.log":  "fixture",
		"go.mod":              "module example",
		"img.png":             "\x89PNG\x00\x00",
		"empty.go":            "",
		"docs/inner/notes.md": "also excluded by **/*.md",
	})
	if len(got) != 0 {
		t.Fatalf("unexpected findings: %v", got)
	}
}

func TestShebangAndHashHeaders(t *testing.T) {
	sh := "#!/bin/sh\n# Copyright 2026-2027 " + testOwner + "\n#\n# Licensed under the Apache License, Version 2.0 (the \"License\");\ncmd\n"
	got := check(t, testConfig(), map[string]string{"tool.sh": sh}, "tool.sh")
	if len(got) != 0 {
		t.Fatalf("unexpected findings: %v", got)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/deep/notes.md", true},
		{"*.md", "docs/notes.md", false}, // anchored: no ** means root only
		{"docs/**", "docs/a/b.txt", true},
		{"docs/**", "docs", true}, // ** matches zero segments
		{"docs/**", "src/docs/a", false},
		{"**/testdata/**", "internal/digest/testdata/x.log", true},
		{"**/testdata/**", "internal/digest/data/x.log", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte("owner: Jane Doe\nexclude:\n  - \"**/*.md\"\nthird_party: []\n"))
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.Owner != "Jane Doe" || len(cfg.Exclude) != 1 {
		t.Errorf("config = %+v", cfg)
	}

	if _, err := ParseConfig([]byte("owner: Jane Doe\nsurprise: true\n")); err == nil {
		t.Error("unknown key accepted; want fail-closed rejection")
	}
	if _, err := ParseConfig([]byte("exclude: []\n")); err == nil {
		t.Error("missing owner accepted; want rejection")
	}
	if _, err := ParseConfig([]byte("owner: Jane Doe\nexclude:\n  - \"[bad\"\n")); err == nil {
		t.Error("malformed glob accepted; want rejection")
	}
}

func TestFindingsSortedByPath(t *testing.T) {
	got := check(t, testConfig(), map[string]string{
		"z.go": "package x\n",
		"a.go": "package x\n",
		"m.go": "package x\n",
	})
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "a.go,m.go,z.go" {
		t.Errorf("findings order = %v, want sorted by path", paths)
	}
}
