//go:build linux

package runner

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestTimeoutKillsGrandchild enforces the kill-the-whole-tree invariant: on timeout the
// whole process *group* dies, including a grandchild the direct child
// spawned.  The shell here is the test workload (a stand-in for Gradle
// spawning daemons and test JVMs) — the runner itself never invokes one.
//
// Linux-only: run it in WSL or inside the builder container
// (`go test ./...` on the Windows side skips it).
func TestTimeoutKillsGrandchild(t *testing.T) {
	r := &Runner{
		LogsDir:     t.TempDir(),
		ToolchainID: "tc-test",
		KillGrace:   500 * time.Millisecond,
	}
	res, err := r.Run(context.Background(), Request{
		Action:  "kill",
		Argv:    []string{"/bin/sh", "-c", "sleep 300 & echo GRANDCHILD_PID=$!; sleep 300"},
		Dir:     t.TempDir(),
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusTimeout {
		t.Fatalf("status=%q, want timeout", res.Status)
	}

	log, err := os.ReadFile(res.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`GRANDCHILD_PID=(\d+)`).FindSubmatch(log)
	if m == nil {
		t.Fatalf("grandchild pid not found in log: %q", log)
	}
	pid, _ := strconv.Atoi(string(m[1]))

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return // grandchild gone — the whole group died
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the process-group kill (kill(pid,0) err=%v)", pid, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
