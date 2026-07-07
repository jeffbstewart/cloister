package egress

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ledger is the restart-surviving burn meter: an append-only file of
// retrieval times — TIMES ONLY, no query, no URL, no content — that the daily
// caps evaluate against a trailing window.  Because a crash-loop can no longer
// reset an in-memory counter, an injection- or bug-driven spiral is bounded
// across restarts.  The model can neither read nor write this file, so it grants
// the model no memory.
//
// Each line is a Unix time_t (epoch seconds) — second resolution is plenty for a
// daily cap, and an integer needs no format/parse ceremony.  Every operation
// over the file is assumed to run under the same process TZ, so absolute epoch
// seconds are compared directly with no zone handling.  The subsystem keeps two
// ledgers: one for search, one for extract (separate meters).
type Ledger struct {
	mu        sync.Mutex
	path      string
	retention time.Duration
	secs      []int64 // in-window retrieval times, epoch seconds, ascending
}

// OpenLedger loads (or creates) the ledger at path, pruning anything older than
// retention relative to now.  Pass now explicitly so callers/tests control time.
func OpenLedger(path string, retention time.Duration, now time.Time) (*Ledger, error) {
	l := &Ledger{path: path, retention: retention}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			s, perr := strconv.ParseInt(line, 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("egress: corrupt ledger %q line %q: %w", path, line, perr)
			}
			l.secs = append(l.secs, s)
		}
	case os.IsNotExist(err):
		// fresh ledger; nothing to load
	default:
		return nil, fmt.Errorf("egress: read ledger %q: %w", path, err)
	}
	slices.Sort(l.secs) // don't trust on-disk order; pruneLocked drops a prefix
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pruneLocked(now.Add(-l.retention).Unix()) {
		if err := l.rewriteLocked(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// CountSince returns how many recorded retrievals are at or after cutoff — the
// daily-cap check passes start-of-UTC-day as the cutoff.
func (l *Ledger) CountSince(cutoff time.Time) int {
	c := cutoff.Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.secs {
		if s >= c {
			n++
		}
	}
	return n
}

// Record appends one retrieval at t, then prunes/rewrites if t moved the window.
func (l *Ledger) Record(t time.Time) error {
	sec := t.Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secs = append(l.secs, sec)
	if l.pruneLocked(t.Add(-l.retention).Unix()) {
		return l.rewriteLocked() // dropped stale lines → rewrite the whole file
	}
	return l.appendLocked(sec) // common path: pure append
}

// pruneLocked drops entries strictly before cutoff; reports whether any went.
// secs is kept ascending (records arrive in time order), so the drop is a prefix.
func (l *Ledger) pruneLocked(cutoff int64) bool {
	i := 0
	for i < len(l.secs) && l.secs[i] < cutoff {
		i++
	}
	if i == 0 {
		return false
	}
	l.secs = append(l.secs[:0], l.secs[i:]...)
	return true
}

func (l *Ledger) appendLocked(sec int64) error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("egress: open ledger %q: %w", l.path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(strconv.FormatInt(sec, 10) + "\n"); err != nil {
		return fmt.Errorf("egress: append ledger %q: %w", l.path, err)
	}
	return nil
}

// rewriteLocked atomically replaces the file with the current in-window set.
func (l *Ledger) rewriteLocked() error {
	var b strings.Builder
	for _, s := range l.secs {
		b.WriteString(strconv.FormatInt(s, 10))
		b.WriteByte('\n')
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("egress: write ledger %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("egress: replace ledger %q: %w", l.path, err)
	}
	return nil
}
