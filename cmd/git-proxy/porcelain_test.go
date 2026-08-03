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

package main

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// The allow-list in classify.go is now the whole safety property, and
// it was written against one git version by reading documentation.
// This test asks the REAL git what commands it has, so a version bump
// announces itself here instead of in a failed build inside a cell.
//
// It does not assert a verdict per command — that would just restate
// the table.  It asserts which commands fall through to the GENERIC
// refusal, i.e. the ones nobody has looked at.  A new porcelain command
// then arrives as a one-line diff naming it, which is the review
// prompt: read what it does, then either allow it, give it a specific
// refusal, or leave it here deliberately.

// unexamined is every mainporcelain command that reaches the
// catch-all.  Keep sorted; the failure message prints a ready-made
// replacement.
var unexamined = []string{
	"archive",
	"citool",
	"format-patch",
	"gitk",
	"gui",
	"maintenance",
	"range-diff",
	"scalar",
}

func TestPorcelainCoverage(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed; this check needs the real binary")
	}
	out, err := exec.Command(git, "--list-cmds=list-mainporcelain").Output()
	if err != nil {
		// --list-cmds is undocumented-but-stable; if a future git drops
		// it, say so rather than silently passing.
		t.Skipf("git --list-cmds is unavailable (%v); this check needs it", err)
	}

	want := map[string]bool{}
	for _, c := range unexamined {
		want[c] = true
	}

	var got []string
	for _, cmd := range strings.Fields(string(out)) {
		p := classify([]string{cmd})
		// The catch-all is identifiable by its opening words; every
		// deliberate refusal has its own text.
		if p.verdict == refuse && strings.HasPrefix(p.reason, "this git command is not on the read-only list") {
			got = append(got, cmd)
		}
	}
	sort.Strings(got)

	var missing, extra []string
	for _, c := range got {
		if !want[c] {
			extra = append(extra, c)
		}
	}
	seen := map[string]bool{}
	for _, c := range got {
		seen[c] = true
	}
	for _, c := range unexamined {
		if !seen[c] {
			missing = append(missing, c)
		}
	}

	if len(extra) > 0 {
		v, _ := exec.Command(git, "--version").Output()
		t.Errorf("this git (%s) has porcelain commands nobody has classified: %s\n"+
			"  Read what each does, then either add it to `reads`, give it an entry in `refusals`,\n"+
			"  or add it to `unexamined` to record that it was considered and left to the catch-all.",
			strings.TrimSpace(string(v)), strings.Join(extra, ", "))
	}
	if len(missing) > 0 {
		// A LOG, not a failure.  Three gits are in play and they differ:
		// CI runs whatever is current, a developer runs whatever their
		// machine has, and the image runs bookworm's.  An entry this git
		// lacks means only that the command arrived or departed in some
		// other version — harmless, since the classification is
		// refuse-by-default either way.  Failing here would make the
		// test unrunnable on anything but one exact git.
		t.Logf("`unexamined` lists commands this git does not have (version skew, not a problem): %s",
			strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Logf("current set:\nvar unexamined = []string{\n\t\"%s\",\n}", strings.Join(got, "\",\n\t\""))
	}
}
