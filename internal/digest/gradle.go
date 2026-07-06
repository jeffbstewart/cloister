package digest

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// Kotlin ≤1.9: e: /path/Foo.kt: (42, 13): unresolved reference: barr
	gradleKtErrOld = regexp.MustCompile(`^e: (.+?): \((\d+), \d+\): (.+)$`)
	// Kotlin 2.x: e: file:///path/Foo.kt:42:13 unresolved reference: barr
	gradleKtErrNew = regexp.MustCompile(`^e: (?:file://)?(.+?):(\d+):(\d+) (.+)$`)
	// javac: /path/Foo.java:17: error: cannot find symbol
	gradleJavacErr = regexp.MustCompile(`^(\S+\.java):(\d+): error: (.+)$`)
	// > Task :compileKotlin FAILED
	gradleTaskFail = regexp.MustCompile(`^> Task (:\S+) FAILED$`)
	// Execution failed for task ':test'.
	gradleExecFail = regexp.MustCompile(`^Execution failed for task '(:[^']+)'\.?$`)
	// TranscodeMatcherServiceTest > matchesRemasteredEdition() FAILED
	gradleTestFail = regexp.MustCompile(`^(\S+) > (.+) FAILED$`)
	// 142 tests completed, 2 failed
	gradleTestCount = regexp.MustCompile(`^(\d+) tests? completed, (\d+) failed`)
)

// Gradle digests Gradle console output: Kotlin (old and new format) and
// javac compile errors, failed-task lines, and JUnit failure blocks from
// the test run.
func Gradle(log []byte) Findings {
	var f Findings
	lines := splitLines(log)
	seenTask := map[string]bool{}
	seenErr := map[string]bool{}
	addTask := func(task string) {
		if !seenTask[task] {
			seenTask[task] = true
			f.FailedTasks = append(f.FailedTasks, task)
		}
	}
	for i, line := range lines {
		if m := gradleKtErrOld.FindStringSubmatch(line); m != nil {
			addCompileError(&f, seenErr, m[1], m[2], m[3])
			continue
		}
		if m := gradleKtErrNew.FindStringSubmatch(line); m != nil {
			addCompileError(&f, seenErr, m[1], m[2], m[4])
			continue
		}
		if m := gradleJavacErr.FindStringSubmatch(line); m != nil {
			addCompileError(&f, seenErr, m[1], m[2], m[3])
			continue
		}
		if m := gradleTaskFail.FindStringSubmatch(line); m != nil {
			addTask(m[1])
			continue
		}
		if m := gradleExecFail.FindStringSubmatch(line); m != nil {
			addTask(m[1])
			continue
		}
		if m := gradleTestFail.FindStringSubmatch(line); m != nil {
			f.FailedTests = append(f.FailedTests, FailedTest{
				Class:   m[1],
				Test:    strings.TrimSuffix(m[2], "()"),
				Message: firstIndented(lines, i+1),
			})
			continue
		}
		if m := gradleTestCount.FindStringSubmatch(line); m != nil {
			f.TestsReported, _ = strconv.Atoi(m[2])
		}
	}
	return f
}
