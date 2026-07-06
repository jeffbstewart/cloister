package workspace

import (
	"strings"
	"testing"
)

func TestUnifiedSingleChange(t *testing.T) {
	old := "line1\nline2\nline3\n"
	neu := "line1\nCHANGED\nline3\n"
	want := "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n line1\n-line2\n+CHANGED\n line3\n"
	got := Unified("a/f", "b/f", []byte(old), []byte(neu), DefaultContext)
	if got != want {
		t.Errorf("Unified mismatch:\n got:\n%q\nwant:\n%q", got, want)
	}
}

func TestUnifiedNoChange(t *testing.T) {
	same := []byte("a\nb\nc\n")
	if got := Unified("a", "b", same, same, DefaultContext); got != "" {
		t.Errorf("identical content produced a diff: %q", got)
	}
}

func TestUnifiedPureInsertAndDelete(t *testing.T) {
	// Insert a line.
	got := Unified("a/f", "b/f", []byte("a\nb\n"), []byte("a\nX\nb\n"), DefaultContext)
	if !strings.Contains(got, "+X") || strings.Contains(got, "-") && !strings.Contains(got, "--- ") {
		t.Errorf("insert diff unexpected:\n%s", got)
	}
	// Delete a line.
	got = Unified("a/f", "b/f", []byte("a\nb\nc\n"), []byte("a\nc\n"), DefaultContext)
	if !strings.Contains(got, "-b") {
		t.Errorf("delete diff should remove b:\n%s", got)
	}
}

func TestUnifiedSeparateHunks(t *testing.T) {
	// Two changes far apart (with zero context) produce two hunks.
	old := "1\n2\n3\n4\n5\n6\n7\n8\n9\n"
	neu := "X\n2\n3\n4\n5\n6\n7\n8\nY\n"
	got := Unified("a", "b", []byte(old), []byte(neu), 0)
	if n := strings.Count(got, "@@ "); n != 2 {
		t.Errorf("want 2 hunks for two distant changes, got %d:\n%s", n, got)
	}
}

func TestUnifiedContextMergesNearbyChanges(t *testing.T) {
	// Two changes 1 line apart, with context 3, merge into one hunk.
	old := "1\n2\n3\n4\n5\n"
	neu := "X\n2\n3\n4\nY\n"
	got := Unified("a", "b", []byte(old), []byte(neu), 3)
	if n := strings.Count(got, "@@ "); n != 1 {
		t.Errorf("want 1 merged hunk, got %d:\n%s", n, got)
	}
}

func TestUnifiedNormalizesCRLF(t *testing.T) {
	// CRLF vs LF of the same logical lines is not a difference for the display
	// formatter (the applier handles EOLs exactly; this is display only).
	got := Unified("a", "b", []byte("a\r\nb\r\n"), []byte("a\nb\n"), DefaultContext)
	if got != "" {
		t.Errorf("CRLF/LF-only difference should not show in the display diff:\n%s", got)
	}
}
