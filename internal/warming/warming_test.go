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

package warming

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const instructions = "Run the airlock:\n\n    bin\\update-gradle-deps.bat <PROJECT> <workspace>\n"

// newConfig returns a config whose instructions file exists iff required.
func newConfig(t *testing.T, required bool) Config {
	t.Helper()
	dir := t.TempDir()
	c := Config{
		InstructionsPath: filepath.Join(dir, "warming"),
		CacheHome:        filepath.Join(dir, "home"),
		ToolchainID:      "tc-test",
	}
	if required {
		if err := os.WriteFile(c.InstructionsPath, []byte(instructions), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestNoWarmingRequired(t *testing.T) {
	if err := newConfig(t, false).Check(); err != nil {
		t.Errorf("Check with no instructions file = %v, want nil", err)
	}
}

// TestUnwarmedRefusalCarriesInstructions is the point: the refusal must
// tell the operator exactly what to run and which marker is missing.
func TestUnwarmedRefusalCarriesInstructions(t *testing.T) {
	c := newConfig(t, true)
	err := c.Check()
	if err == nil {
		t.Fatal("Check before warming = nil, want a refusal")
	}
	for _, want := range []string{
		"tc-test",
		c.MarkerPath(),
		"update-gradle-deps.bat",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

func TestMarkLiftsTheGate(t *testing.T) {
	c := newConfig(t, true)
	p, err := c.Mark()
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if p != c.MarkerPath() {
		t.Errorf("Mark path = %q, want %q", p, c.MarkerPath())
	}
	if err := c.Check(); err != nil {
		t.Errorf("Check after Mark = %v, want nil", err)
	}
	// Idempotent: re-warming rewrites the marker without error.
	if _, err := c.Mark(); err != nil {
		t.Errorf("second Mark = %v, want nil", err)
	}
}

// TestInstructionsTemplate: the refusal interpolates the allowlisted
// deployment identifiers into a copy-paste command, degrades unset ones
// to readable placeholders, and never expands off-allowlist variables —
// even ones present in the environment (the secret-leak guard).
func TestInstructionsTemplate(t *testing.T) {
	c := newConfig(t, true)
	template := "run: update-deps.bat ${PROJECT} ${WORKSPACE}\nnever: ${STATE_TOKEN}"
	if err := os.WriteFile(c.InstructionsPath, []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"PROJECT":     "myproject",
		"STATE_TOKEN": "sekrit",
	}
	c.Getenv = func(k string) string { return env[k] }

	err := c.Check()
	if err == nil {
		t.Fatal("Check before warming = nil, want a refusal")
	}
	for _, want := range []string{
		"update-deps.bat myproject <WORKSPACE>", // set expands; unset degrades
		"never: <STATE_TOKEN>",                  // off-allowlist never expands
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "sekrit") {
		t.Errorf("refusal leaked an off-allowlist environment value: %q", err)
	}
}

// TestFailsClosed: a config that cannot name a marker refuses rather than
// passing, and only when warming is actually required.
func TestFailsClosed(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) { c.CacheHome = "" },
		func(c *Config) { c.ToolchainID = "" },
		func(c *Config) { c.ToolchainID = "evil/../tc" },
	}
	for _, mutate := range bad {
		c := newConfig(t, true)
		mutate(&c)
		if err := c.Check(); err == nil {
			t.Errorf("Check with invalid config %+v = nil, want error", c)
		}
		if _, err := c.Mark(); err == nil {
			t.Errorf("Mark with invalid config %+v = nil, want error", c)
		}
	}
}
