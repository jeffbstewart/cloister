package runner

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Run-log retention defaults: oldest logs are pruned first, after each
// completed run.
const (
	DefaultMaxLogFiles  = 200
	DefaultMaxLogsBytes = 2 << 30 // 2 GiB across all retained logs
)

// pruneLogs deletes the oldest run logs beyond the retention caps.
// UUIDv7 filenames sort chronologically, so lexicographic order is age
// order — no mtime juggling.  The queue is serial, so there is never a
// concurrent pruner.  Best-effort: a file that cannot be removed (e.g.
// held open on a Windows dev box) stops this pass and is retried after
// the next run.  The newest log is never deleted — it belongs to the run
// that just finished and its digest still points at it.
func (r *Runner) pruneLogs() {
	maxFiles := r.MaxLogFiles
	if maxFiles == 0 {
		maxFiles = DefaultMaxLogFiles
	}
	maxBytes := r.MaxLogsBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxLogsBytes
	}
	if maxFiles < 0 && maxBytes < 0 {
		return
	}

	entries, err := os.ReadDir(r.LogsDir)
	if err != nil {
		return
	}
	type logInfo struct {
		name string
		size int64
	}
	var logs []logInfo
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		logs = append(logs, logInfo{e.Name(), fi.Size()})
		total += fi.Size()
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].name < logs[j].name })

	for len(logs) > 1 {
		overCount := maxFiles > 0 && len(logs) > maxFiles
		overBytes := maxBytes > 0 && total > maxBytes
		if !overCount && !overBytes {
			return
		}
		victim := logs[0]
		if os.Remove(filepath.Join(r.LogsDir, victim.name)) != nil {
			return
		}
		total -= victim.size
		logs = logs[1:]
	}
}
