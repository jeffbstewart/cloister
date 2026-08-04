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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// The environment prompt has to make one hop that nothing was watching:
// from the image, where it is baked, into the agent CLI's context file,
// which lives on a NAMED VOLUME.  Docker seeds a named volume from the
// image exactly once — when the volume is created — and never again, so
// an image upgrade does not refresh it.  Something has to copy the
// prompt forward on every start, and until now that was a line in the
// entrypoint ending `2>/dev/null || true`: silent whether it worked or
// not.
//
// A stale prompt is not a visible failure.  The agent starts, answers,
// works — under last month's rules, or none.  So the session manager
// installs the prompt itself, at the moment a session begins, and SAYS
// what it did.

const (
	// stockPromptPath is the prompt as baked into the image.
	stockPromptPath = "/usr/local/share/cloister/AGENTS.md"
	// agentPromptRel is where the agent CLI reads it, under HOME.
	agentPromptRel = ".qwen/QWEN.md"
)

// promptStatus is what installPrompt found and did, for reporting.
type promptStatus struct {
	Installed bool  // the copy ran (the file was absent or stale)
	Bytes     int   // size now in place
	Err       error // nothing could be done; the agent will run without it
}

// installPrompt copies the image's prompt into the agent's context file
// when they differ, and reports what happened.  Errors are returned,
// never swallowed: an agent running without its environment prompt is
// the failure this exists to make visible.
func installPrompt(stock, dest string) promptStatus {
	want, err := os.ReadFile(stock)
	if err != nil {
		return promptStatus{Err: fmt.Errorf("reading the image's prompt (%s): %w", stock, err)}
	}
	if got, err := os.ReadFile(dest); err == nil && bytes.Equal(got, want) {
		return promptStatus{Bytes: len(want)}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return promptStatus{Err: fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)}
	}
	// Write to a temp file and rename, so a crash mid-write cannot leave
	// the agent reading half a prompt — which would be worse than none,
	// because it would still look like a prompt.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".prompt-*")
	if err != nil {
		return promptStatus{Err: fmt.Errorf("writing %s: %w", dest, err)}
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once renamed
	if _, err := tmp.Write(want); err != nil {
		tmp.Close()
		return promptStatus{Err: fmt.Errorf("writing %s: %w", dest, err)}
	}
	if err := tmp.Close(); err != nil {
		return promptStatus{Err: fmt.Errorf("writing %s: %w", dest, err)}
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return promptStatus{Err: fmt.Errorf("installing %s: %w", dest, err)}
	}
	return promptStatus{Installed: true, Bytes: len(want)}
}

// reportPrompt installs the prompt and tells the operator where it
// stands, because "the agent has its instructions" is not otherwise
// observable from outside until something goes wrong expensively.
func (m *manager) reportPrompt() {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/agent"
	}
	dest := filepath.Join(home, filepath.FromSlash(agentPromptRel))

	st := installPrompt(m.o.stockPrompt, dest)
	switch {
	case st.Err != nil:
		m.printf("\n!! the environment prompt could NOT be installed: %v\n", st.Err)
		m.println("   The agent will start WITHOUT its rules — it will not know that git is")
		m.println("   read-only here, that the archivist owns writes, or that the cell is")
		m.println("   offline.  Expect it to improvise.  Fix this before trusting the work.")
	case st.Installed:
		m.printf("environment prompt installed (%d bytes) → %s\n", st.Bytes, dest)
	default:
		m.printf("environment prompt current (%d bytes)\n", st.Bytes)
	}
	m.println("the agent should open with: cloister ready: git reads only, the archivist writes, no network")
}
