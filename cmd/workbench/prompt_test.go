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

func TestInstallPromptCopiesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	stock := filepath.Join(dir, "AGENTS.md")
	dest := filepath.Join(dir, "home", ".qwen", "QWEN.md")
	if err := os.WriteFile(stock, []byte("the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := installPrompt(stock, dest)
	if st.Err != nil || !st.Installed {
		t.Fatalf("install = %+v, want a clean install", st)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "the rules\n" {
		t.Errorf("dest = %q, %v", got, err)
	}
}

// TestInstallPromptRefreshesAStaleFile is the case that matters: the
// agent's context file lives on a named volume, which docker seeds from
// the image once and never again.  An image upgrade leaves last
// month's rules in place, and nothing about that looks wrong from
// outside.
func TestInstallPromptRefreshesAStaleFile(t *testing.T) {
	dir := t.TempDir()
	stock := filepath.Join(dir, "AGENTS.md")
	dest := filepath.Join(dir, "QWEN.md")
	if err := os.WriteFile(stock, []byte("new rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("last month's rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := installPrompt(stock, dest)
	if st.Err != nil || !st.Installed {
		t.Fatalf("install = %+v, want a refresh", st)
	}
	if got, _ := os.ReadFile(dest); string(got) != "new rules\n" {
		t.Errorf("stale content survived: %q", got)
	}
}

func TestInstallPromptLeavesACurrentFileAlone(t *testing.T) {
	dir := t.TempDir()
	stock := filepath.Join(dir, "AGENTS.md")
	dest := filepath.Join(dir, "QWEN.md")
	for _, p := range []string{stock, dest} {
		if err := os.WriteFile(p, []byte("same\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := installPrompt(stock, dest)
	if st.Err != nil || st.Installed {
		t.Errorf("install = %+v, want no write and no error", st)
	}
	if st.Bytes != len("same\n") {
		t.Errorf("bytes = %d", st.Bytes)
	}
}

// TestInstallPromptReportsFailureRatherThanSwallowingIt: the previous
// mechanism was `cp … 2>/dev/null || true`, which is why a missing
// prompt was invisible until an agent improvised its way through a
// task.
func TestInstallPromptReportsFailureRatherThanSwallowingIt(t *testing.T) {
	dir := t.TempDir()
	st := installPrompt(filepath.Join(dir, "no-such-file"), filepath.Join(dir, "QWEN.md"))
	if st.Err == nil {
		t.Fatal("a missing stock prompt reported success")
	}
	if !strings.Contains(st.Err.Error(), "no-such-file") {
		t.Errorf("error %q does not name the missing file", st.Err)
	}
	if st.Installed {
		t.Error("reported an install that did not happen")
	}
}

// TestReportPromptSaysSomethingUseableInEveryCase — the operator reads
// this line to decide whether to trust the session.
func TestReportPromptSaysSomethingUseableInEveryCase(t *testing.T) {
	dir := t.TempDir()
	stock := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(stock, []byte("the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(dir, "home"))

	arc := &fakeArchivist{}
	m, out, _ := rig(t, arc, "")
	m.o.stockPrompt = stock

	m.reportPrompt()
	if s := out.String(); !strings.Contains(s, "installed") || !strings.Contains(s, "cloister ready") {
		t.Errorf("first install said %q; want the install and the line to watch for", s)
	}

	out.Reset()
	m.reportPrompt()
	if s := out.String(); !strings.Contains(s, "current") {
		t.Errorf("second call said %q; want it to report the prompt already current", s)
	}

	// And the loud case.
	out.Reset()
	m.o.stockPrompt = filepath.Join(dir, "gone")
	m.reportPrompt()
	if s := out.String(); !strings.Contains(s, "could NOT be installed") || !strings.Contains(s, "improvise") {
		t.Errorf("failure said %q; want it unmissable and to say what follows", s)
	}
}
