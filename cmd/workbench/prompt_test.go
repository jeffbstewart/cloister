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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPromptAcceptsAUsablePrompt(t *testing.T) {
	dir := t.TempDir()
	stock := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(stock, []byte("the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkPrompt(stock, filepath.Join(dir, "absent"))
	if c.Err != nil || c.Bytes != len("the rules\n") {
		t.Errorf("check = %+v, want a clean result", c)
	}
	if c.EvictedLegacy || c.LegacyDiffers {
		t.Errorf("check = %+v, want nothing said about a legacy file that is not there", c)
	}
}

// TestCheckPromptRefusesAnUnusablePrompt: an agent with no rules looks
// exactly like an agent with rules until it does something expensive,
// so this is the one pre-flight that stops a session.
func TestCheckPromptRefusesAnUnusablePrompt(t *testing.T) {
	dir := t.TempDir()
	if c := checkPrompt(filepath.Join(dir, "gone"), filepath.Join(dir, "x")); c.Err == nil {
		t.Error("a missing prompt was accepted")
	}
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte("   \n\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkPrompt(empty, filepath.Join(dir, "x"))
	if c.Err == nil || !strings.Contains(c.Err.Error(), "empty") {
		t.Errorf("whitespace-only prompt = %+v, want an empty-file refusal", c)
	}
}

// TestCheckPromptEvictsOnlyOurOwnCopy is the careful half.  Older
// images copied the prompt into the agent's memory file; that copy is
// now wrong and should go.  But the same file is where the agent keeps
// its OWN memory, and deleting a model's accumulated notes costs far
// more than leaving a stale duplicate — so the test is exact equality
// and nothing looser.
func TestCheckPromptEvictsOnlyOurOwnCopy(t *testing.T) {
	dir := t.TempDir()
	stock := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(stock, []byte("the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Byte-identical: ours, and it goes.
	ours := filepath.Join(dir, "QWEN-ours.md")
	if err := os.WriteFile(ours, []byte("the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkPrompt(stock, ours)
	if !c.EvictedLegacy || c.LegacyDiffers {
		t.Errorf("identical copy = %+v, want it evicted", c)
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Error("our stale copy survived")
	}

	// Anything else is the agent's memory, and is left alone.
	for _, content := range []string{
		"the rules\nplus something the agent remembered\n",
		"the user prefers tabs\n",
		"",
	} {
		theirs := filepath.Join(dir, "QWEN-theirs.md")
		if err := os.WriteFile(theirs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		c := checkPrompt(stock, theirs)
		if c.EvictedLegacy {
			t.Errorf("deleted the agent's own memory (%q)", content)
		}
		if !c.LegacyDiffers {
			t.Errorf("content %q: want it reported as the agent's own", content)
		}
		if _, err := os.Stat(theirs); err != nil {
			t.Errorf("content %q was removed: %v", content, err)
		}
	}
}

// TestReportPromptSaysSomethingUsableInEveryCase — the operator reads
// these lines to decide whether to trust the session.
func TestReportPromptSaysSomethingUsableInEveryCase(t *testing.T) {
	dir := t.TempDir()
	stock := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(stock, []byte("the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".qwen"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	m, out, _ := rig(t, &fakeArchivist{}, "")
	m.o.stockPrompt = stock

	if !m.reportPrompt() {
		t.Fatal("a usable prompt was refused")
	}
	if s := out.String(); !strings.Contains(s, "system prompt") || !strings.Contains(s, "cloister ready") {
		t.Errorf("said %q; want how it is delivered and the line to watch for", s)
	}

	// A leftover copy from an older image is reported as it is removed.
	out.Reset()
	if err := os.WriteFile(filepath.Join(home, ".qwen", "QWEN.md"), []byte("the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.reportPrompt()
	if s := out.String(); !strings.Contains(s, "stale copy") {
		t.Errorf("said %q; want the eviction reported", s)
	}

	// And the case that stops the session.
	out.Reset()
	m.o.stockPrompt = filepath.Join(dir, "gone")
	if m.reportPrompt() {
		t.Error("started an agent with no rules")
	}
	if s := out.String(); !strings.Contains(s, "Refusing to start") {
		t.Errorf("said %q; want it unmissable and final", s)
	}
}
