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

package digest

import (
	"reflect"
	"testing"
)

// Table-driven over captured `go build` / `go test` logs in testdata/.
func TestGoTest(t *testing.T) {
	tests := []struct {
		file string
		want Findings
	}{
		{
			file: "golang_ok.log",
			want: Findings{},
		},
		{
			file: "golang_fail.log",
			want: Findings{
				FailedTasks: []string{"example.com/sampleapp/matcher"},
				FailedTests: []FailedTest{
					{
						Test:    "TestMatchesAllWidgets",
						Message: "matcher_test.go:42: expected 3 matches, got 2",
					},
					{
						Test:    "TestIgnoresSampleFiles",
						Message: "matcher_test.go:61: sample.tmp should be ignored, matched anyway",
					},
					{
						Test:    "TestScoring",
						Message: "scoring_test.go:33: bonus not applied",
					},
					{
						Test:    "TestScoring/priority_bonus",
						Message: "scoring_test.go:33: bonus not applied",
					},
				},
				TestsReported: 4,
			},
		},
		{
			file: "golang_compile_error.log",
			want: Findings{
				FailedTasks: []string{"example.com/sampleapp/matcher"},
				CompileErrors: []CompileError{
					{
						File:    "matcher.go",
						Line:    12,
						Message: "undefined: barr",
					},
					{
						File:    "matcher.go",
						Line:    29,
						Message: "cannot use score (variable of type string) as int value in return statement",
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got := GoTest(loadLog(t, tt.file))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GoTest(%s) =\n%+v\nwant\n%+v", tt.file, got, tt.want)
			}
		})
	}
}

func TestIndentOf(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"no indent", 0},
		{"    four spaces", 4},
		{"\ttab", 1},
		{"", 0},
	}
	for _, tt := range tests {
		if got := indentOf(tt.in); got != tt.want {
			t.Errorf("indentOf(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
