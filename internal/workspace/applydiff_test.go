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
	"errors"
	"testing"
)

// Success cases: content-located application with the sloppiness LLMs produce.
func TestApplyDiffSuccess(t *testing.T) {
	tests := []struct {
		name string
		old  string
		diff string
		want string
	}{
		{
			name: "simple change",
			old:  "a\nb\nc\n",
			diff: "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n",
			want: "a\nB\nc\n",
		},
		{
			name: "wrong @@ line numbers are ignored",
			old:  "a\nb\nc\n",
			diff: "@@ -99,3 +42,7 @@\n a\n-b\n+B\n c\n",
			want: "a\nB\nc\n",
		},
		{
			name: "whitespace drift matches loosely, writes the file's actual bytes",
			old:  "\tkeep\nold\n",              // context line is tab-indented in the file
			diff: "@@ @@\n keep\n-old\n+new\n", // ...but space-rendered in the diff
			want: "\tkeep\nnew\n",              // the tab is preserved (write-exact)
		},
		{
			name: "CRLF file preserved though the diff is LF",
			old:  "a\r\nb\r\nc\r\n",
			diff: "@@ @@\n a\n-b\n+B\n c\n",
			want: "a\r\nB\r\nc\r\n",
		},
		{
			name: "no trailing newline preserved",
			old:  "a\nb",
			diff: "@@ @@\n a\n-b\n+B\n",
			want: "a\nB",
		},
		{
			name: "multiple hunks in one file",
			old:  "1\n2\n3\n4\n5\n6\n7\n",
			diff: "@@ @@\n 1\n-2\n+X\n 3\n@@ @@\n 5\n-6\n+Y\n 7\n",
			want: "1\nX\n3\n4\n5\nY\n7\n",
		},
		{
			name: "git extended headers tolerated",
			old:  "a\nb\n",
			diff: "diff --git a/f b/f\nindex 111..222 100644\n--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n+B\n",
			want: "a\nB\n",
		},
		{
			name: "pure insertion between context lines",
			old:  "a\nc\n",
			diff: "@@ @@\n a\n+b\n c\n",
			want: "a\nb\nc\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ApplyDiff([]byte(tt.old), tt.diff)
			if err != nil {
				t.Fatalf("ApplyDiff error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("ApplyDiff =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// Failure and special-status cases.
func TestApplyDiffErrors(t *testing.T) {
	tests := []struct {
		name string
		old  string
		diff string
		want error
	}{
		{
			name: "context matches more than once is ambiguous",
			old:  "dup\nmid\ndup\n",
			diff: "@@ @@\n-dup\n+DUP\n",
			want: ErrAmbiguous,
		},
		{
			name: "context not found",
			old:  "a\nb\n",
			diff: "@@ @@\n-zzz\n+q\n",
			want: ErrHunkNotFound,
		},
		{
			name: "already applied is a distinct status, not a failure",
			old:  "a\nB\nc\n",
			diff: "@@ @@\n a\n-b\n+B\n c\n",
			want: ErrAlreadyApplied,
		},
		{
			name: "malformed hunk-body prefix",
			old:  "a\nb\n",
			diff: "@@ @@\n a\nXbad\n",
			want: ErrMalformedDiff,
		},
		{
			name: "multi-file diff rejected",
			old:  "a\n",
			diff: "--- a/f1\n+++ b/f1\n@@ @@\n-a\n+b\n--- a/f2\n+++ b/f2\n@@ @@\n-c\n+d\n",
			want: ErrMultiFile,
		},
		{
			name: "no hunks",
			old:  "a\n",
			diff: "just some text\n",
			want: ErrMalformedDiff, // a line outside any hunk
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ApplyDiff([]byte(tt.old), tt.diff)
			if !errors.Is(err, tt.want) {
				t.Errorf("ApplyDiff err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestApplyDiffWhitespaceOnlyChange: a reindent (whitespace-only) is located
// whitespace-SENSITIVELY, so it applies to the un-reindented line and is not
// mistaken for already-applied.
func TestApplyDiffWhitespaceOnlyChange(t *testing.T) {
	old := "    foo()\n"                    // 4-space indent
	diff := "@@ @@\n-    foo()\n+\tfoo()\n" // reindent to a tab
	got, err := ApplyDiff([]byte(old), diff)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "\tfoo()\n" {
		t.Errorf("got %q, want a tab-indented line", got)
	}
	// Applying the same reindent again is already-applied, not a re-reindent.
	if _, err := ApplyDiff(got, diff); !errors.Is(err, ErrAlreadyApplied) {
		t.Errorf("second apply err = %v, want ErrAlreadyApplied", err)
	}
}
