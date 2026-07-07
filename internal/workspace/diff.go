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

package workspace

import (
	"fmt"
	"strings"
)

// DefaultContext is the number of unchanged lines shown around each change.
const DefaultContext = 3

// lcsGuard bounds the O(n*m) LCS table; beyond it we fall back to a coarse
// (non-minimal) whole-block diff rather than allocate unboundedly.
const lcsGuard = 4_000_000

// Unified renders a line-level unified diff between old and new content with
// `context` surrounding lines.  It is a DISPLAY/AUDIT artifact — what dryRun
// returns and what the audit stores.  It is NOT
// the mechanism apply_diff uses to edit a file (that is the content-matching
// engine).  Line endings are normalized to `\n` for display and the
// no-final-newline distinction is not shown; the applier handles those exactly.
func Unified(oldName, newName string, old, new []byte, context int) string {
	if context < 0 {
		context = DefaultContext
	}
	a := splitLines(old)
	b := splitLines(new)
	ops := lcsDiff(a, b)
	return format(oldName, newName, ops, context)
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	// A trailing newline yields a final empty element — drop it so "a\nb\n" and
	// "a\nb" both split to ["a","b"] (the no-newline distinction is not shown).
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

type opKind uint8

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type editOp struct {
	kind opKind
	line string
}

// lcsDiff returns the edit script transforming a into b as equal/delete/insert
// ops, via a longest-common-subsequence DP. Correct and simple; O(n*m) time and
// space, guarded for pathological inputs.
func lcsDiff(a, b []string) []editOp {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n*m > lcsGuard {
		return coarse(a, b)
	}
	// dp[i][j] = LCS length of a[i:] and b[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []editOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, editOp{opEqual, a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, editOp{opDelete, a[i]})
			i++
		default:
			ops = append(ops, editOp{opInsert, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, editOp{opDelete, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, editOp{opInsert, b[j]})
	}
	return ops
}

func coarse(a, b []string) []editOp {
	ops := make([]editOp, 0, len(a)+len(b))
	for _, l := range a {
		ops = append(ops, editOp{opDelete, l})
	}
	for _, l := range b {
		ops = append(ops, editOp{opInsert, l})
	}
	return ops
}

// lineOp is an editOp annotated with its 1-based old/new line numbers (0 = not
// present on that side).
type lineOp struct {
	editOp
	oldNo, newNo int
}

func format(oldName, newName string, ops []editOp, context int) string {
	if len(ops) == 0 {
		return ""
	}
	los := make([]lineOp, len(ops))
	oldNo, newNo := 0, 0
	for i, op := range ops {
		switch op.kind {
		case opEqual:
			oldNo++
			newNo++
			los[i] = lineOp{op, oldNo, newNo}
		case opDelete:
			oldNo++
			los[i] = lineOp{op, oldNo, 0}
		case opInsert:
			newNo++
			los[i] = lineOp{op, 0, newNo}
		}
	}

	n := len(los)
	changed := func(i int) bool { return los[i].kind != opEqual }

	// Mark every op that is a change or within `context` lines of one; unions of
	// nearby changes merge automatically into a single hunk.
	include := make([]bool, n)
	any := false
	for i := 0; i < n; i++ {
		if !changed(i) {
			continue
		}
		any = true
		lo, hi := i-context, i+context
		if lo < 0 {
			lo = 0
		}
		if hi > n-1 {
			hi = n - 1
		}
		for k := lo; k <= hi; k++ {
			include[k] = true
		}
	}
	if !any {
		return "" // no changes
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	for i := 0; i < n; {
		if !include[i] {
			i++
			continue
		}
		j := i
		for j < n && include[j] {
			j++
		}
		writeHunk(&out, los[i:j])
		i = j
	}
	return out.String()
}

func writeHunk(out *strings.Builder, hunk []lineOp) {
	var oldStart, oldCount, newStart, newCount int
	for _, l := range hunk {
		if l.oldNo != 0 {
			if oldStart == 0 {
				oldStart = l.oldNo
			}
			oldCount++
		}
		if l.newNo != 0 {
			if newStart == 0 {
				newStart = l.newNo
			}
			newCount++
		}
	}
	fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	for _, l := range hunk {
		switch l.kind {
		case opEqual:
			out.WriteByte(' ')
		case opDelete:
			out.WriteByte('-')
		case opInsert:
			out.WriteByte('+')
		}
		out.WriteString(l.line)
		out.WriteByte('\n')
	}
}
