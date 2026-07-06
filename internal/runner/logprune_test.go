package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func countLogs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") {
			n++
		}
	}
	return n
}

func TestPruneByCountKeepsNewest(t *testing.T) {
	r := newTestRunner(t)
	r.MaxLogFiles = 3
	r.MaxLogsBytes = -1

	var paths []string
	for i := 0; i < 5; i++ {
		res := run(t, r, time.Minute, "lines", "10")
		paths = append(paths, res.LogPath)
	}

	if got := countLogs(t, r.LogsDir); got != 3 {
		t.Errorf("%d logs retained, want 3", got)
	}
	// UUIDv7 filenames sort chronologically: the two oldest are the pruned ones.
	for i, p := range paths {
		_, err := os.Stat(p)
		if i < 2 && err == nil {
			t.Errorf("old log %d survived pruning: %s", i, filepath.Base(p))
		}
		if i >= 2 && err != nil {
			t.Errorf("recent log %d missing: %v", i, err)
		}
	}
}

func TestPruneBySizeNeverDeletesNewest(t *testing.T) {
	r := newTestRunner(t)
	r.MaxLogFiles = -1
	r.MaxLogsBytes = 1500 // each "lines 100" log is ~800 bytes

	var paths []string
	for i := 0; i < 4; i++ {
		res := run(t, r, time.Minute, "lines", "100")
		paths = append(paths, res.LogPath)
	}

	if _, err := os.Stat(paths[0]); err == nil {
		t.Error("oldest log survived size-based pruning")
	}
	if _, err := os.Stat(paths[len(paths)-1]); err != nil {
		t.Errorf("newest log must never be pruned: %v", err)
	}
	if got := countLogs(t, r.LogsDir); got > 2 {
		t.Errorf("%d logs retained, want at most 2 under a 1500-byte cap", got)
	}
}

func TestPruneDisabled(t *testing.T) {
	r := newTestRunner(t)
	r.MaxLogFiles = -1
	r.MaxLogsBytes = -1
	for i := 0; i < 4; i++ {
		run(t, r, time.Minute, "lines", "10")
	}
	if got := countLogs(t, r.LogsDir); got != 4 {
		t.Errorf("%d logs, want all 4 with pruning disabled", got)
	}
}
