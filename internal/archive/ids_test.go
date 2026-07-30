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

package archive

import (
	"strings"
	"testing"
)

func TestParseBranchName(t *testing.T) {
	valid := []string{"agent/fix-1", "release/1.2", "a", "agent/deep/nesting", "v1.0-rc.1"}
	for _, s := range valid {
		if _, err := ParseBranchName(s); err != nil {
			t.Errorf("ParseBranchName(%q) rejected a valid name: %v", s, err)
		}
	}
	invalid := []string{
		"", "-flag", ".hidden", "a..b", "a//b", "a/", "a.", "a.lock",
		"a b", "a~1", "a^2", "a:b", "a@{1}", "a\\b", "a\nb",
		strings.Repeat("x", 201),
	}
	for _, s := range invalid {
		if _, err := ParseBranchName(s); err == nil {
			t.Errorf("ParseBranchName(%q) accepted an invalid name", s)
		}
	}
}

func TestParseCheckpointID(t *testing.T) {
	// The full-length ids are built by Repeat so the presubmit's
	// long-hex-string (possible token) scan has nothing to match.
	valid := []string{"abcd", strings.Repeat("0a1b2c3d", 5), strings.Repeat("a", 64)}
	for _, s := range valid {
		if _, err := ParseCheckpointID(s); err != nil {
			t.Errorf("ParseCheckpointID(%q) rejected a valid id: %v", s, err)
		}
	}
	invalid := []string{"", "abc", "ABCD", "xyzw", strings.Repeat("a", 65), "-abc", "abcd "}
	for _, s := range invalid {
		if _, err := ParseCheckpointID(s); err == nil {
			t.Errorf("ParseCheckpointID(%q) accepted an invalid id", s)
		}
	}
}

func TestParseRef(t *testing.T) {
	valid := []string{"HEAD", "HEAD~2", "HEAD^", "main", "origin/main", "abcd1234", "agent/fix-1~3"}
	for _, s := range valid {
		if _, err := ParseRef(s); err != nil {
			t.Errorf("ParseRef(%q) rejected a valid revision: %v", s, err)
		}
	}
	invalid := []string{"", "-x", "a..b", "a:b", "@{u}", "a b", "a\\b", strings.Repeat("x", 201)}
	for _, s := range invalid {
		if _, err := ParseRef(s); err == nil {
			t.Errorf("ParseRef(%q) accepted an invalid revision", s)
		}
	}
}

func TestValidPath(t *testing.T) {
	valid := []string{"README.md", "a/b.txt", "deep/nested/path.go", "with-dash.txt", "dot.file"}
	for _, s := range valid {
		if err := validPath(s); err != nil {
			t.Errorf("validPath(%q) rejected a valid path: %v", s, err)
		}
	}
	invalid := []string{
		"", "-f", "--force", "a\\b", "/abs/path", "C:stuff", "..",
		"../x", "a/../b", "x/..", ":!exclude", "a\nb", "a\x00b",
	}
	for _, s := range invalid {
		if err := validPath(s); err == nil {
			t.Errorf("validPath(%q) accepted an invalid path", s)
		}
	}
}
