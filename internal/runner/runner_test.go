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

// Tests for runner.go.  These are portable (Windows dev box included) via
// the re-exec helper-process pattern: the runner spawns this test binary
// back with -test.run=TestHelperProcess.
//
// Manifest-contract invariant coverage:
//
//	1 (no shell, literal argv)  → TestArgvPassedLiterally
//	3 (allowlist-built env)     → TestEnvAllowlist
//	4 (process-group kill)      → TestTimeoutKillsGrandchild in proc_unix_test.go
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

// TestHelperProcess is not a real test: it's the child workload the runner
// executes in these tests.  It only acts when re-exec'd by the runner.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	sep := slices.Index(os.Args, "--")
	if sep < 0 || sep+1 >= len(os.Args) {
		os.Exit(2)
	}
	args := os.Args[sep+1:]
	switch args[0] {
	case "exit":
		code, _ := strconv.Atoi(args[1])
		os.Exit(code)
	case "sleep":
		d, _ := time.ParseDuration(args[1])
		time.Sleep(d)
		os.Exit(0)
	case "env":
		for _, e := range os.Environ() {
			fmt.Println(e)
		}
		os.Exit(0)
	case "echo":
		for _, a := range args[1:] {
			fmt.Println(a)
		}
		os.Exit(0)
	case "stderr":
		fmt.Fprintln(os.Stderr, "line on stderr")
		fmt.Println("line on stdout")
		os.Exit(0)
	case "spew":
		n, _ := strconv.Atoi(args[1])
		chunk := strings.Repeat("x", 8191) + "\n"
		for written := 0; written < n; written += len(chunk) {
			os.Stdout.WriteString(chunk)
		}
		os.Exit(0)
	case "lines":
		n, _ := strconv.Atoi(args[1])
		w := bufio.NewWriter(os.Stdout)
		for i := 1; i <= n; i++ {
			fmt.Fprintf(w, "line %d\n", i)
		}
		w.Flush()
		os.Exit(0)
	}
	os.Exit(2)
}

func helperArgv(args ...string) []string {
	return append([]string{os.Args[0], "-test.run=TestHelperProcess", "--"}, args...)
}

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	return &Runner{
		LogsDir:     t.TempDir(),
		ToolchainID: "tc-test",
		KillGrace:   200 * time.Millisecond,
	}
}

func run(t *testing.T, r *Runner, timeout time.Duration, args ...string) *Result {
	t.Helper()
	res, err := r.Run(context.Background(), Request{
		Action:  "t",
		Argv:    helperArgv(args...),
		Dir:     t.TempDir(),
		Timeout: timeout,
		Env:     map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func readLog(t *testing.T, res *Result) string {
	t.Helper()
	b, err := os.ReadFile(res.LogPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

func TestExitZeroIsOK(t *testing.T) {
	res := run(t, newTestRunner(t), time.Minute, "exit", "0")
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Errorf("status=%q exit=%d, want ok/0", res.Status, res.ExitCode)
	}
}

func TestNonZeroExitIsFailed(t *testing.T) {
	res := run(t, newTestRunner(t), time.Minute, "exit", "3")
	if res.Status != StatusFailed || res.ExitCode != 3 {
		t.Errorf("status=%q exit=%d, want failed/3", res.Status, res.ExitCode)
	}
}

func TestRunIDIsValidAndLogFileMatches(t *testing.T) {
	res := run(t, newTestRunner(t), time.Minute, "exit", "0")
	if res.RunID.IsZero() {
		t.Fatal("run completed without a RunID")
	}
	if _, err := runid.Parse(res.RunID.String()); err != nil {
		t.Errorf("runner produced an id runid.Parse rejects: %v", err)
	}
	if got, want := filepath.Base(res.LogPath), res.RunID.String()+".log"; got != want {
		t.Errorf("log file %q, want %q", got, want)
	}
	if _, err := os.Stat(res.LogPath); err != nil {
		t.Errorf("log not persisted: %v", err)
	}
}

// TestEnvAllowlist enforces the env-allowlist invariant: the child env is built from
// scratch — a var set in the server's environment must not leak through.
func TestEnvAllowlist(t *testing.T) {
	t.Setenv("RUNNER_CANARY_SECRET", "leaked")
	res := run(t, newTestRunner(t), time.Minute, "env")
	log := readLog(t, res)
	if strings.Contains(log, "RUNNER_CANARY_SECRET") {
		t.Error("parent environment leaked into the child (invariant 3 violated)")
	}
	if !strings.Contains(log, "TOOLCHAIN_ID=tc-test") {
		t.Error("TOOLCHAIN_ID missing from child env")
	}
	if !strings.Contains(log, "GO_WANT_HELPER_PROCESS=1") {
		t.Error("requested extra env var missing from child env")
	}
}

// TestArgvPassedLiterally enforces the no-shell invariant: no shell ever runs, so
// metacharacters arrive at the child as literal argv elements.
func TestArgvPassedLiterally(t *testing.T) {
	res := run(t, newTestRunner(t), time.Minute,
		"echo", "$(rm -rf /)", "; echo pwned", "&& dir", "|tee")
	log := readLog(t, res)
	for _, want := range []string{"$(rm -rf /)", "; echo pwned", "&& dir", "|tee"} {
		if !strings.Contains(log, want) {
			t.Errorf("argv element %q not passed literally", want)
		}
	}
}

func TestStderrInterleaved(t *testing.T) {
	res := run(t, newTestRunner(t), time.Minute, "stderr")
	log := readLog(t, res)
	if !strings.Contains(log, "line on stderr") || !strings.Contains(log, "line on stdout") {
		t.Errorf("stdout+stderr not both captured; log=%q", log)
	}
}

func TestTimeoutKillsRun(t *testing.T) {
	res := run(t, newTestRunner(t), 200*time.Millisecond, "sleep", "30s")
	if res.Status != StatusTimeout {
		t.Fatalf("status=%q, want timeout", res.Status)
	}
	if res.Duration > 10*time.Second {
		t.Errorf("took %s; kill did not happen promptly", res.Duration)
	}
}

func TestOutputOverflowKillsRun(t *testing.T) {
	r := newTestRunner(t)
	r.MaxOutput = 64 << 10
	res := run(t, r, time.Minute, "spew", "1000000")
	if res.Status != StatusOverflow {
		t.Fatalf("status=%q, want output_overflow", res.Status)
	}
	if res.LogBytes > r.MaxOutput {
		t.Errorf("logBytes=%d exceeds cap %d", res.LogBytes, r.MaxOutput)
	}
	if fi, err := os.Stat(res.LogPath); err != nil || fi.Size() > r.MaxOutput {
		t.Errorf("log file size exceeds cap (err=%v)", err)
	}
}

func TestTailAndLineCount(t *testing.T) {
	res := run(t, newTestRunner(t), time.Minute, "lines", "300")
	if res.LogTotalLines != 300 {
		t.Errorf("LogTotalLines=%d, want 300", res.LogTotalLines)
	}
	if len(res.Tail) != TailLines {
		t.Fatalf("len(Tail)=%d, want %d", len(res.Tail), TailLines)
	}
	if res.Tail[0] != "line 101" || res.Tail[len(res.Tail)-1] != "line 300" {
		t.Errorf("tail window wrong: first=%q last=%q", res.Tail[0], res.Tail[len(res.Tail)-1])
	}
}

func TestBusyQueue(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	resCh := make(chan *Result, 1)
	go func() {
		res, _ := r.Run(context.Background(), Request{
			Action:  "long",
			Argv:    helperArgv("sleep", "10s"),
			Dir:     dir,
			Timeout: 700 * time.Millisecond,
			Env:     map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		})
		resCh <- res
	}()

	var active runid.ID
	deadline := time.Now().Add(3 * time.Second)
	for {
		busy, a, _ := r.State()
		if busy {
			active = a
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner never became busy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err := r.Run(context.Background(), Request{
		Action:  "second",
		Argv:    helperArgv("exit", "0"),
		Dir:     dir,
		Timeout: time.Minute,
		Env:     map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
	})
	var busyErr *ErrBusy
	if !errors.As(err, &busyErr) {
		t.Fatalf("second call: err=%v, want *ErrBusy", err)
	}
	if busyErr.ActiveRunID != active {
		t.Errorf("busy reports runId %q, active is %q", busyErr.ActiveRunID, active)
	}

	first := <-resCh
	if first.Status != StatusTimeout {
		t.Errorf("first run status=%q, want timeout", first.Status)
	}

	// The queue frees up and history records completed runs only.
	res := run(t, r, time.Minute, "exit", "0")
	if res.Status != StatusOK {
		t.Errorf("post-busy run status=%q, want ok", res.Status)
	}
	_, _, recent := r.State()
	if len(recent) != 2 {
		t.Fatalf("recent=%v, want 2 entries (rejected call must not be recorded)", recent)
	}
	if recent[0].Action != "long" || recent[1].Action != "t" {
		t.Errorf("run history must carry action names: %v", recent)
	}
}

func TestRejectsEmptyArgvAndTimeout(t *testing.T) {
	r := newTestRunner(t)
	if _, err := r.Run(context.Background(), Request{Action: "x", Timeout: time.Minute}); err == nil {
		t.Error("empty argv accepted")
	}
	if _, err := r.Run(context.Background(), Request{Action: "x", Argv: []string{"a"}}); err == nil {
		t.Error("zero timeout accepted")
	}
}
