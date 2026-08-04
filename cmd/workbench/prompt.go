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

// How the environment prompt reaches the model, and how we know.
//
// It is delivered by /usr/local/bin/qwen-cloister, which passes the
// image's AGENTS.md to the agent CLI through --append-system-prompt —
// a real system prompt, appended to the CLI's built-in one so its own
// tool-calling scaffolding survives.
//
// It used to be copied into ~/.qwen/QWEN.md instead, and that was the
// wrong file in two ways.  It is the agent's own USER MEMORY: loaded
// into conversation context rather than given as instruction, and
// managed by the CLI's auto-memory, auto-skill and auto-dream features
// — so the cell's rules arrived as a note-to-self, in a store the agent
// rewrites.  That matches what we saw: rules ignored, MCP tools tried
// as shell commands, correction that did not stick.
//
// Two things are worth checking before every session, because both
// failures are invisible from outside until they are expensive: that
// the prompt the wrapper will read actually exists, and that the old
// copy is no longer squatting in memory.

const (
	// stockPromptPath is the prompt as baked into the image — the file
	// qwen-cloister reads.
	stockPromptPath = "/usr/local/share/cloister/AGENTS.md"
	// legacyMemoryRel is where the prompt used to be copied.  Still
	// checked, so a cell upgraded from an older image does not carry a
	// stale duplicate in the agent's memory forever.
	legacyMemoryRel = ".qwen/QWEN.md"
)

// promptCheck is what the pre-flight found.
type promptCheck struct {
	Bytes int   // size of the prompt the wrapper will deliver
	Err   error // the prompt is unusable; the agent must not start
	// EvictedLegacy is true when a copy of OUR prompt was found in the
	// agent's memory file and removed.
	EvictedLegacy bool
	// LegacyDiffers is true when the memory file holds something that is
	// NOT our prompt — genuine agent memory, left strictly alone.
	LegacyDiffers bool
}

// checkPrompt verifies the prompt the wrapper will deliver, and clears
// the legacy copy out of the agent's memory when it is unmistakably
// ours.
//
// "Unmistakably ours" means byte-identical to the image's prompt.
// Anything else is the agent's own memory and is never touched — the
// cost of deleting a model's accumulated notes is far higher than the
// cost of leaving a stale duplicate, so the test is exact equality and
// nothing looser.
func checkPrompt(stock, legacy string) promptCheck {
	want, err := os.ReadFile(stock)
	if err != nil {
		return promptCheck{Err: fmt.Errorf("the environment prompt (%s) cannot be read: %w", stock, err)}
	}
	if len(bytes.TrimSpace(want)) == 0 {
		return promptCheck{Err: fmt.Errorf("the environment prompt (%s) is empty", stock)}
	}
	c := promptCheck{Bytes: len(want)}

	got, err := os.ReadFile(legacy)
	switch {
	case err != nil:
		return c // nothing there, which is the wanted state
	case bytes.Equal(got, want):
		if err := os.Remove(legacy); err == nil {
			c.EvictedLegacy = true
		}
	default:
		c.LegacyDiffers = true
	}
	return c
}

// reportPrompt runs the pre-flight and tells the operator where things
// stand, because "the agent has its rules" is not otherwise observable
// until something goes wrong.  Returns false when the agent must not
// start.
func (m *manager) reportPrompt() bool {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/agent"
	}
	c := checkPrompt(m.o.stockPrompt, filepath.Join(home, filepath.FromSlash(legacyMemoryRel)))

	if c.Err != nil {
		m.printf("\n!! %v\n", c.Err)
		m.println("   The agent would start without its rules — not knowing that git is")
		m.println("   read-only here, that the archivist owns writes, or that this cell is")
		m.println("   offline.  Refusing to start it.  The image needs repair.")
		return false
	}
	m.printf("environment prompt: %d bytes, delivered as a system prompt\n", c.Bytes)
	if c.EvictedLegacy {
		m.println("removed a stale copy of it from the agent's memory file (it belongs in the")
		m.println("system prompt, not in ~/.qwen/QWEN.md, which the agent rewrites itself)")
	}
	if c.LegacyDiffers {
		m.println("note: ~/.qwen/QWEN.md holds the agent's own memory; left untouched")
	}
	m.println("the agent should open with: cloister ready: git reads only, the archivist writes, no network")
	return true
}
