package digest

import (
	"regexp"
	"strings"
)

var (
	// --- FAIL: TestName (0.01s)   (indented when it's a subtest)
	goTestFail = regexp.MustCompile(`^(\s*)--- FAIL: (\S+) `)
	// ./matcher.go:12:6: undefined: barr   (column optional)
	goCompileErr = regexp.MustCompile(`^(\S+\.go):(\d+)(?::\d+)?: (.+)$`)
	// FAIL	example.com/sampleapp/matcher	0.041s
	goPkgFail = regexp.MustCompile(`^FAIL\s+(\S+)`)
)

// GoTest digests Go toolchain output (`go build`, `go test`): --- FAIL
// blocks, file:line: compiler errors, and package-level FAIL lines
// . It is registered in the manifest as parser "gotest".
func GoTest(log []byte) Findings {
	var f Findings
	lines := splitLines(log)
	seenErr := map[string]bool{}
	seenPkg := map[string]bool{}
	for i, line := range lines {
		if m := goTestFail.FindStringSubmatch(line); m != nil {
			f.FailedTests = append(f.FailedTests, FailedTest{
				Test:    m[2],
				Message: goFailMessage(lines, i, len(m[1])),
			})
			continue
		}
		if m := goCompileErr.FindStringSubmatch(line); m != nil {
			addCompileError(&f, seenErr, m[1], m[2], m[3])
			continue
		}
		if m := goPkgFail.FindStringSubmatch(line); m != nil {
			if !seenPkg[m[1]] {
				seenPkg[m[1]] = true
				f.FailedTasks = append(f.FailedTasks, m[1])
			}
		}
	}
	f.TestsReported = len(f.FailedTests)
	return f
}

// goFailMessage finds the first line under a --- FAIL header that is
// deeper-indented and not itself a nested FAIL header — typically the
// "file_test.go:42: ..." assertion line.
func goFailMessage(lines []string, i, headerIndent int) string {
	for j := i + 1; j < len(lines); j++ {
		l := lines[j]
		if strings.TrimSpace(l) == "" {
			continue
		}
		if indentOf(l) <= headerIndent {
			return ""
		}
		if goTestFail.MatchString(l) {
			continue
		}
		return strings.TrimSpace(l)
	}
	return ""
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }
