package workspace

import (
	"errors"
	"fmt"
	"strings"
)

// apply_diff engine.  Pure function: given a file's
// bytes and a unified-diff string, produce the edited bytes — located by CONTENT,
// not line numbers, tolerant of the sloppy diffs LLMs produce.  Never shells out
// to patch(1).  This is the mechanism; workspace.Unified is only for display.

var (
	ErrNoHunks        = errors.New("apply_diff: diff has no hunks")
	ErrMalformedDiff  = errors.New("apply_diff: malformed diff")
	ErrMultiFile      = errors.New("apply_diff: multi-file diff not supported (one file per call)")
	ErrHunkNotFound   = errors.New("apply_diff: a hunk's context was not found in the file")
	ErrAmbiguous      = errors.New("apply_diff: a hunk's context matches more than one location; add more surrounding context")
	ErrAlreadyApplied = errors.New("apply_diff: diff already applied; file unchanged")
)

// ApplyDiff applies a content-located unified diff to old, returning the edited
// content.  Line numbers in `@@` headers are ignored; hunks are located by their
// context+removed lines.  Returns ErrAlreadyApplied when the file already matches
// the post-image, and the other sentinel errors for the failure
// modes above.
func ApplyDiff(old []byte, diff string) ([]byte, error) {
	hunks, err := parseHunks(diff)
	if err != nil {
		return nil, err
	}
	shape := splitFile(old)
	lines, changed, err := applyHunks(shape.lines, hunks)
	if err != nil {
		return nil, err
	}
	if !changed {
		return nil, ErrAlreadyApplied
	}
	return shape.join(lines), nil
}

// --- file shape: split into lines while remembering EOL style + trailing NL ---

type fileShape struct {
	lines           []string
	crlf            bool // dominant EOL is CRLF
	trailingNewline bool
}

func splitFile(b []byte) fileShape {
	if len(b) == 0 {
		return fileShape{}
	}
	s := string(b)
	crlf := strings.Count(s, "\r\n")
	lf := strings.Count(s, "\n") - crlf
	s = strings.ReplaceAll(s, "\r\n", "\n")
	trailing := strings.HasSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	if trailing {
		lines = lines[:len(lines)-1] // drop the empty element after the final newline
	}
	return fileShape{lines: lines, crlf: crlf > lf, trailingNewline: trailing}
}

// join reassembles lines in the file's original EOL style, preserving the
// original trailing-newline state.
func (fs fileShape) join(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	eol := "\n"
	if fs.crlf {
		eol = "\r\n"
	}
	out := strings.Join(lines, eol)
	if fs.trailingNewline {
		out += eol
	}
	return []byte(out)
}

// --- diff parsing ---

type diffKind uint8

const (
	dctx diffKind = iota // ' ' context
	ddel                 // '-' removed
	dadd                 // '+' added
	dnl                  // '\' no-newline marker (parsed, then ignored in v1)
)

type dline struct {
	kind diffKind
	text string
}

type hunk struct {
	lines []dline
}

// blocks returns the old side (context + removed) and new side (context + added).
func (h hunk) blocks() (oldB, newB []string) {
	for _, dl := range h.lines {
		switch dl.kind {
		case dctx:
			oldB = append(oldB, dl.text)
			newB = append(newB, dl.text)
		case ddel:
			oldB = append(oldB, dl.text)
		case dadd:
			newB = append(newB, dl.text)
		}
	}
	return
}

func parseHunks(diff string) ([]hunk, error) {
	raw := strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n")
	if n := len(raw); n > 0 && raw[n-1] == "" {
		raw = raw[:n-1]
	}
	var hunks []hunk
	var cur []dline
	inHunk := false
	fileHeaders := 0
	flush := func() {
		if inHunk && len(cur) > 0 {
			hunks = append(hunks, hunk{lines: cur})
		}
		cur = nil
	}
	for _, line := range raw {
		switch {
		case strings.HasPrefix(line, "@@"):
			flush()
			inHunk = true // ignore the numbers; the body is what matters
		case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ "):
			if strings.HasPrefix(line, "--- ") {
				fileHeaders++
			}
			flush()
			inHunk = false
		case isExtHeader(line):
			flush()
			inHunk = false
		default:
			if !inHunk {
				if line == "" {
					continue // tolerate stray blank lines between headers
				}
				return nil, fmt.Errorf("%w: line outside a hunk: %q", ErrMalformedDiff, line)
			}
			k, text, ok := classifyBodyLine(line)
			if !ok {
				return nil, fmt.Errorf("%w: bad hunk line prefix: %q", ErrMalformedDiff, line)
			}
			if k == dnl {
				continue // v1: trailing-newline is preserved from the file, not the marker
			}
			cur = append(cur, dline{k, text})
		}
	}
	flush()
	if fileHeaders > 1 {
		return nil, ErrMultiFile
	}
	if len(hunks) == 0 {
		return nil, ErrNoHunks
	}
	return hunks, nil
}

func isExtHeader(line string) bool {
	for _, p := range []string{"diff ", "index ", "new file", "deleted file", "old mode", "new mode", "similarity ", "rename ", "copy ", "GIT binary"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// classifyBodyLine maps a hunk-body line to its kind.  A fully empty line is
// treated as a blank context line (models routinely drop the leading space);
// any other prefix-less line is malformed (strict on prefixes).
func classifyBodyLine(line string) (diffKind, string, bool) {
	if line == "" {
		return dctx, "", true
	}
	switch line[0] {
	case ' ':
		return dctx, line[1:], true
	case '-':
		return ddel, line[1:], true
	case '+':
		return dadd, line[1:], true
	case '\\':
		return dnl, "", true
	}
	return 0, "", false
}

// --- application ---

func applyHunks(fileLines []string, hunks []hunk) ([]string, bool, error) {
	lines := append([]string(nil), fileLines...)
	cursor := 0
	changed := false
	for _, h := range hunks {
		oldB, newB := h.blocks()
		if len(oldB) == 0 && len(newB) == 0 {
			continue // degenerate empty hunk
		}
		// A whitespace-only change must be located whitespace-sensitively so a
		// reindent is not confused with the already-reindented result.
		exact := whitespaceOnlyChange(oldB, newB)

		idx, count := locate(lines, oldB, cursor, exact)
		switch {
		case count == 1:
			repl := writeExact(lines[idx:idx+len(oldB)], h)
			next := make([]string, 0, idx+len(repl)+len(lines)-idx-len(oldB))
			next = append(next, lines[:idx]...)
			next = append(next, repl...)
			next = append(next, lines[idx+len(oldB):]...)
			lines = next
			cursor = idx + len(repl)
			if !equalLines(oldB, newB) {
				changed = true
			}
		case count > 1:
			return nil, false, ErrAmbiguous
		default: // 0 — maybe already applied?
			nidx, ncount := locate(lines, newB, cursor, exact)
			if ncount == 1 {
				cursor = nidx + len(newB) // already applied; skip, no change
				continue
			}
			return nil, false, ErrHunkNotFound
		}
	}
	return lines, changed, nil
}

// writeExact builds the replacement: retained context lines take the FILE's
// actual bytes (matched positionally within the located region); added lines are
// verbatim from the diff.  Removed lines are consumed but not emitted.
func writeExact(region []string, h hunk) []string {
	var out []string
	fi := 0
	for _, dl := range h.lines {
		switch dl.kind {
		case dctx:
			out = append(out, region[fi])
			fi++
		case ddel:
			fi++
		case dadd:
			out = append(out, dl.text)
		}
	}
	return out
}

// locate returns the first absolute index and the count of occurrences of block
// as a contiguous run in lines[start:].
func locate(lines, block []string, start int, exact bool) (int, int) {
	if len(block) == 0 {
		return -1, 0
	}
	first, count := -1, 0
	for i := start; i+len(block) <= len(lines); i++ {
		if matchAt(lines, block, i, exact) {
			if first < 0 {
				first = i
			}
			count++
		}
	}
	return first, count
}

func matchAt(lines, block []string, i int, exact bool) bool {
	for j := range block {
		if !lineEq(lines[i+j], block[j], exact) {
			return false
		}
	}
	return true
}

func lineEq(a, b string, exact bool) bool {
	if exact {
		return a == b
	}
	return normalizeWS(a) == normalizeWS(b)
}

// normalizeWS collapses all whitespace runs to single spaces and trims, so a
// context line the model rendered with tab/space drift still matches.
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// whitespaceOnlyChange reports whether old and new differ ONLY in whitespace
// (same under normalization, but not byte-identical).
func whitespaceOnlyChange(oldB, newB []string) bool {
	if len(oldB) != len(newB) {
		return false
	}
	for i := range oldB {
		if normalizeWS(oldB[i]) != normalizeWS(newB[i]) {
			return false
		}
	}
	return !equalLines(oldB, newB)
}
