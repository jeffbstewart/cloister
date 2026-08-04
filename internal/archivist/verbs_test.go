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

package archivist

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jeffbstewart/cloister/internal/verbs"
)

// The archivist's tool names are spoken in three places a compiler
// cannot connect on its own: here (registration), cmd/git-proxy
// (refusals telling an agent what to call instead), and the stock
// environment prompt the agent reads.  internal/verbs makes the two Go
// sites a compile-time relationship.  These tests close the rest of the
// loop — that the constants match what is actually registered, and that
// the prompt names tools that exist.

// toolNames reads a surface's registry through an in-memory session.
func toolNames(t *testing.T, operator bool) map[string]bool {
	t.Helper()
	srv := New(Config{Version: "test"})
	s := dial(t, srv)
	if operator {
		s = dialOperator(t, srv)
	}
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	return got
}

// TestEveryRegisteredToolHasAConstant is the guard internal/verbs
// exists for.  It fails in both directions: a tool registered under a
// name the package does not hold, and a constant naming a tool nobody
// registers.  An ADDITION fails it too, on purpose — a new tool belongs
// in internal/verbs before it belongs anywhere else, because that is
// what makes the git proxy and the prompt able to reference it safely.
func TestEveryRegisteredToolHasAConstant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator bool
		want     []string
	}{
		{"agent surface", false, verbs.Agent},
		{"operator surface", true, verbs.Operator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toolNames(t, tc.operator)
			want := map[string]bool{}
			for _, v := range tc.want {
				want[v] = true
			}
			for name := range got {
				if !want[name] {
					t.Errorf("%s registers %q, which internal/verbs does not name — add the constant", tc.name, name)
				}
			}
			for name := range want {
				if !got[name] {
					t.Errorf("internal/verbs names %q, which %s does not register — the constant is stale", name, tc.name)
				}
			}
		})
	}
}

// promptPath is the stock environment prompt baked into the agent
// image.  Reached by relative path because it is not Go source; the
// test fails loudly rather than skipping if it moves, since a silently
// skipped guard is worse than none.
var promptPath = filepath.Join("..", "..", "docker", "workbench", "AGENTS.md")

// TestAgentsPromptNamesRealTools is the one check no compiler can make.
// The prompt is where the agent LEARNS these names, so a stale entry
// there is worse than a stale refusal in the proxy: the agent reads it
// before it ever runs a command, and will call a tool that does not
// exist.
func TestAgentsPromptNamesRealTools(t *testing.T) {
	body, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("reading the stock prompt: %v (if it moved, update promptPath)", err)
	}
	agent := toolNames(t, false)

	// Two places name tools, and they need different readings.
	//
	// The translation table's right-hand column is a list of tool names
	// by construction, so every backticked token there is a claim that
	// the tool exists — INCLUDING single words like `checkpoint`, which
	// a shape-based scan of the whole file would miss precisely because
	// they look like ordinary prose.
	//
	// Elsewhere, only lower_snake_case is safely distinguishable from
	// English, so prose mentions are checked by shape.
	// Match ANY backticked span and normalize afterwards, rather than
	// trying to spell the decoration into the pattern.  The decoration
	// changes: tool names are rendered `"start_work"` now, quoted so
	// they cannot be read as shell commands, and a pattern that encoded
	// the old shape silently stopped checking — twice, once for the
	// table and once for prose.  A guard that quietly matches nothing is
	// worse than no guard, so this one cannot be blinded by punctuation.
	span := regexp.MustCompile("`([^`]+)`")
	isName := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	isSnake := regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)+$`)
	clean := func(s string) string { return strings.Trim(strings.TrimSpace(s), `"'`) }
	// Snake-case things in the prompt that are not tools.
	notTools := map[string]bool{"agent_home": true, "cloister_grange": true}

	var unknown []string
	seen := map[string]bool{}
	note := func(name string) {
		if seen[name] || notTools[name] {
			return
		}
		seen[name] = true
		if !agent[name] {
			unknown = append(unknown, name)
		}
	}

	inTable := false
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") {
			cells := strings.Split(strings.Trim(trimmed, "|"), "|")
			if len(cells) >= 2 {
				last := cells[len(cells)-1]
				if strings.Contains(last, "---") { // the header separator
					inTable = true
					continue
				}
				if inTable {
					// Inside the table's right column every entry IS a
					// tool name, so a bare word counts.
					for _, m := range span.FindAllStringSubmatch(last, -1) {
						if n := clean(m[1]); isName.MatchString(n) {
							note(n)
						}
					}
					continue
				}
			}
		} else {
			inTable = false
		}
		// In prose only lower_snake_case is safely distinguishable from
		// ordinary English and from shell words like `git`.
		for _, m := range span.FindAllStringSubmatch(line, -1) {
			if n := clean(m[1]); isSnake.MatchString(n) {
				note(n)
			}
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("the stock prompt names tools the agent surface does not have: %s\n"+
			"  (%s)\n"+
			"  Either the tool was renamed and the prompt was not, or the prompt is advertising something unbuilt.",
			strings.Join(unknown, ", "), promptPath)
	}

	// And the reverse, as a nudge rather than a failure: a tool the
	// prompt never mentions is one the agent has to discover from the
	// MCP listing alone.
	var unmentioned []string
	for name := range agent {
		if !seen[name] {
			unmentioned = append(unmentioned, name)
		}
	}
	sort.Strings(unmentioned)
	if len(unmentioned) > 0 {
		t.Logf("tools not mentioned in the stock prompt (fine if deliberate): %s", strings.Join(unmentioned, ", "))
	}
}
