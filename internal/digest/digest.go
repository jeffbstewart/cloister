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

// Package digest turns raw build logs into small, structured results the
// agent can consume without destroying its ~64k-token context.
// An action tool call returns a Digest — parsed compile errors, failed tests,
// failed tasks, and a short tail — never the raw log; the full log stays on
// disk and is paged via get_log.
//
// Parsers are pure functions ([]byte → Findings) selected by name in the
// manifest, covered by table-driven tests over captured logs in testdata/.
package digest

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// CompileError is one compiler diagnostic parsed from the log.
type CompileError struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// FailedTest is one test failure parsed from the log.
type FailedTest struct {
	Class   string `json:"class,omitempty"`
	Test    string `json:"test"`
	Message string `json:"message,omitempty"`
}

// Findings is what a parser extracts from a raw log.
type Findings struct {
	FailedTasks   []string
	CompileErrors []CompileError
	FailedTests   []FailedTest
	// TestsReported is the failure count the log itself claims (e.g. the
	// Gradle "N tests completed, M failed" line).  Compared against the
	// parsed count in the digest hint so the agent knows if the parser
	// missed failures.
	TestsReported int
}

// Digest is the JSON an action tool call returns instead of the log.
type Digest struct {
	RunID    runid.ID `json:"runId"`
	Status   string   `json:"status"`
	ExitCode int      `json:"exitCode"`
	// Duration is a real duration in memory and a readable string on the
	// wire ("1.5s"), reusing audit's serialization.
	Duration audit.Duration `json:"duration"`
	// Error is the runner's failure description.  A string, not a Go error:
	// Digest is a wire type — error values do not survive a JSON round-trip,
	// and the consumer is the agent reading text, not Go code branching on
	// error identity.
	Error         string         `json:"error,omitempty"`
	FailedTasks   []string       `json:"failedTasks,omitempty"`
	CompileErrors []CompileError `json:"compileErrors,omitempty"`
	FailedTests   []FailedTest   `json:"failedTests,omitempty"`
	LogTotalLines int            `json:"logTotalLines"`
	LogTail       []string       `json:"logTail"`
	Hint          string         `json:"hint,omitempty"`
}

// ParseFunc is the parser contract: a pure function over the full log bytes.
type ParseFunc func(log []byte) Findings

var parsers = map[string]ParseFunc{
	"gradle":  Gradle,
	"gotest":  GoTest,
	"generic": Generic,
}

// Get returns the parser for name; the empty name means "generic".
func Get(name string) (ParseFunc, bool) {
	if name == "" {
		name = "generic"
	}
	p, ok := parsers[name]
	return p, ok
}

// Known reports whether name selects a parser (empty = generic = known).
// The manifest loader uses this so an unknown parser is a startup error.
func Known(name string) bool {
	_, ok := Get(name)
	return ok
}

// Hint builds the digest hint line pointing the agent at get_log and
// flagging any gap between parsed and reported failure counts.
func (f Findings) Hint(runID runid.ID) string {
	base := fmt.Sprintf("Full log via get_log(runId=%q).", runID)
	if n := len(f.FailedTests); n > 0 {
		reported := f.TestsReported
		if reported < n {
			reported = n
		}
		return fmt.Sprintf("%s %d failed tests parsed of %d reported.", base, n, reported)
	}
	if n := len(f.CompileErrors); n > 0 {
		return fmt.Sprintf("%s %d compile errors parsed.", base, n)
	}
	return base
}

// --- helpers shared by the parsers ---

func splitLines(b []byte) []string {
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

// cleanPath strips URI and workspace prefixes so digests carry repo-relative
// paths the agent can open directly.
func cleanPath(p string) string {
	p = strings.TrimPrefix(p, "file://")
	p = strings.TrimPrefix(p, "/workspace/")
	p = strings.TrimPrefix(p, "./")
	return p
}

func addCompileError(f *Findings, seen map[string]bool, file, lineNo, msg string) {
	file = cleanPath(file)
	key := file + ":" + lineNo + ":" + msg
	if seen[key] {
		return
	}
	seen[key] = true
	n, _ := strconv.Atoi(lineNo)
	f.CompileErrors = append(f.CompileErrors, CompileError{File: file, Line: n, Message: msg})
}

// firstIndented returns the first non-blank indented line at or after i,
// trimmed — the assertion/exception line under a failure header.
func firstIndented(lines []string, i int) string {
	for ; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		if l[0] != ' ' && l[0] != '\t' {
			return ""
		}
		return strings.TrimSpace(l)
	}
	return ""
}
