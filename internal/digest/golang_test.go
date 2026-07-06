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
				FailedTasks: []string{"example.com/mediamanager/matcher"},
				FailedTests: []FailedTest{
					{
						Test:    "TestMatchesRemasteredEdition",
						Message: "matcher_test.go:42: expected 3 matches, got 2",
					},
					{
						Test:    "TestIgnoresSampleFiles",
						Message: "matcher_test.go:61: sample.mkv should be ignored, matched anyway",
					},
					{
						Test:    "TestScoring",
						Message: "scoring_test.go:33: bonus not applied",
					},
					{
						Test:    "TestScoring/remaster_bonus",
						Message: "scoring_test.go:33: bonus not applied",
					},
				},
				TestsReported: 4,
			},
		},
		{
			file: "golang_compile_error.log",
			want: Findings{
				FailedTasks: []string{"example.com/mediamanager/matcher"},
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
