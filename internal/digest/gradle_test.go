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

// Table-driven over captured Gradle console logs in testdata/.  Extend the
// corpus with real captures as they become available.
func TestGradle(t *testing.T) {
	tests := []struct {
		file string
		want Findings
	}{
		{
			file: "gradle_ok.log",
			want: Findings{},
		},
		{
			file: "gradle_compile_error.log", // Kotlin 2.x `e: file://...:line:col msg` format
			want: Findings{
				FailedTasks: []string{":compileKotlin"},
				CompileErrors: []CompileError{
					{
						File:    "src/main/kotlin/com/example/sampleapp/WidgetMatcherService.kt",
						Line:    42,
						Message: "unresolved reference: barr",
					},
					{
						File:    "src/main/kotlin/com/example/sampleapp/WidgetMatcherService.kt",
						Line:    57,
						Message: "type mismatch: inferred type is String but Int was expected",
					},
				},
			},
		},
		{
			file: "gradle_compile_error_legacy.log", // Kotlin ≤1.9 `e: path: (line, col): msg` + javac
			want: Findings{
				FailedTasks: []string{":compileKotlin", ":compileJava"},
				CompileErrors: []CompileError{
					{
						File:    "src/main/kotlin/com/example/sampleapp/InputScanner.kt",
						Line:    18,
						Message: "expecting member declaration",
					},
					{
						File:    "src/main/java/com/example/sampleapp/LegacyProbe.java",
						Line:    17,
						Message: "cannot find symbol",
					},
				},
			},
		},
		{
			file: "gradle_test_failure.log",
			want: Findings{
				FailedTasks: []string{":test"},
				FailedTests: []FailedTest{
					{
						Class:   "WidgetMatcherServiceTest",
						Test:    "matchesAllWidgets",
						Message: "org.opentest4j.AssertionFailedError: expected: <3> but was: <2>",
					},
					{
						Class:   "WidgetMatcherServiceTest",
						Test:    "ignoresSampleFiles",
						Message: "org.opentest4j.AssertionFailedError: expected: <true> but was: <false>",
					},
				},
				TestsReported: 2,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got := Gradle(loadLog(t, tt.file))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Gradle(%s) =\n%+v\nwant\n%+v", tt.file, got, tt.want)
			}
		})
	}
}

// TestGradleDeduplicatesTasks: the same failed task appears both as
// "> Task :x FAILED" and "Execution failed for task ':x'" — one entry.
func TestGradleDeduplicatesTasks(t *testing.T) {
	got := Gradle(loadLog(t, "gradle_test_failure.log"))
	if len(got.FailedTasks) != 1 || got.FailedTasks[0] != ":test" {
		t.Errorf("FailedTasks = %v, want exactly [:test]", got.FailedTasks)
	}
}
