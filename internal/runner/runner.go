// Package runner executes manifest actions one at a time with hard
// timeouts, whole-process-tree kill, and capped, persisted output.  It
// enforces three of the manifest-contract invariants: argv is
// executed directly (never via a shell), the child environment is built
// from an allowlist, and timeout/overflow kill the entire process group.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jeffbstewart/cloister/internal/cellstate"
	"github.com/jeffbstewart/cloister/internal/runid"
)

const (
	// DefaultMaxOutput is the hard cap on captured output: past it
	// the run is killed with status "output_overflow".
	DefaultMaxOutput = 50 << 20 // 50 MB
	// TailLines is how many trailing lines stay in memory for the digest.
	TailLines = 200
	// defaultKillGrace is the SIGTERM→SIGKILL grace period.
	defaultKillGrace = 10 * time.Second
	// recentRuns bounds the run history reported by harness_info.
	recentRuns = 20
	// maxLineBytes splits pathological unterminated lines so the in-memory
	// tail buffer stays bounded even for logs with no newlines.
	maxLineBytes = 8192
)

// Status is the terminal disposition of one run — a named string type, like
// audit.Decision, so callers stay type-safe.
type Status string

// Status values for a completed run.
const (
	StatusOK       Status = "ok"
	StatusFailed   Status = "failed"
	StatusTimeout  Status = "timeout"
	StatusOverflow Status = "output_overflow"
	StatusError    Status = "error"
)

// Runner is the global one-at-a-time execution queue.  The zero
// value plus LogsDir and ToolchainID is ready to use.  LogsDir is a local
// spool (tmpfs in production) used for digest parsing and get_log's fast
// path; durable storage is the Sink's job.
type Runner struct {
	LogsDir     string
	ToolchainID string
	Sink        Sink          // durable log/status owner; nil → local-only
	StatusPath  string        // local status.json when Sink is nil (tests)
	MaxOutput   int64         // 0 → DefaultMaxOutput
	KillGrace   time.Duration // 0 → defaultKillGrace

	// Run-log retention (see logprune.go).  0 selects the default;
	// negative disables that cap.
	MaxLogFiles  int   // 0 → DefaultMaxLogFiles
	MaxLogsBytes int64 // 0 → DefaultMaxLogsBytes

	mu     sync.Mutex
	busy   bool
	active runid.ID
	recent []cellstate.RunSummary
}

// Request describes one action execution.  Argv comes only from the
// manifest's run array plus validated params — never from free text.
type Request struct {
	Action  string
	Argv    []string
	Dir     string
	Timeout time.Duration
	// Env is appended to the allowlist base (PATH, HOME, TOOLCHAIN_ID);
	// the parent environment is never inherited.
	Env map[string]string
}

// Result is the raw outcome of a run; the mcpserver layer turns it into a
// digest.
type Result struct {
	RunID         runid.ID
	Status        Status
	ExitCode      int
	Duration      time.Duration
	LogPath       string
	LogBytes      int64
	LogTotalLines int
	Tail          []string // last TailLines lines
	Err           string   // populated when Status == StatusError
}

// ErrBusy reports that another action is already running (a second
// call returns immediately; the agent can watch the active run instead).
type ErrBusy struct{ ActiveRunID runid.ID }

func (e *ErrBusy) Error() string {
	return fmt.Sprintf("an action is already running (runId %s)", e.ActiveRunID)
}

// State reports queue occupancy and recent run summaries (oldest first).
func (r *Runner) State() (busy bool, activeRunID runid.ID, recent []cellstate.RunSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy, r.active, slices.Clone(r.recent)
}

// Run executes one request, blocking until it finishes.  It returns
// *ErrBusy without side effects if a run is already active.  Any other
// failure is reported inside the Result, not as an error.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return nil, errors.New("empty argv")
	}
	if req.Timeout <= 0 {
		return nil, errors.New("timeout required")
	}

	runID, err := runid.New()
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.busy {
		active := r.active
		r.mu.Unlock()
		return nil, &ErrBusy{ActiveRunID: active}
	}
	r.busy, r.active = true, runID
	r.mu.Unlock()

	// Status posts happen outside r.mu (a slow sink must not block
	// harness_info) and synchronously in code order (so busy precedes idle).
	r.writeStatus(cellstate.Status{Busy: true, Active: &cellstate.ActiveRun{
		RunID:     runID,
		Action:    req.Action,
		StartedAt: time.Now().UTC(),
	}})

	res := r.execute(ctx, runID, req)

	last := cellstate.RunSummary{RunID: runID, Action: req.Action, Status: string(res.Status)}
	r.mu.Lock()
	r.busy, r.active = false, runid.ID{}
	r.recent = append(r.recent, last)
	if len(r.recent) > recentRuns {
		r.recent = r.recent[len(r.recent)-recentRuns:]
	}
	r.mu.Unlock()

	r.writeStatus(cellstate.Status{Busy: false, LastRun: &last})
	r.pruneLogs()
	return res, nil
}

func (r *Runner) execute(ctx context.Context, runID runid.ID, req Request) *Result {
	res := &Result{
		RunID:    runID,
		ExitCode: -1,
		LogPath:  filepath.Join(r.LogsDir, runID.String()+".log"),
	}

	logFile, err := os.OpenFile(res.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		res.Status = StatusError
		res.Err = fmt.Sprintf("create log: %v", err)
		return res
	}
	defer logFile.Close()

	tail := newTailBuffer(TailLines)
	overflow := make(chan struct{})

	// Fan out to the local spool (fast, tmpfs) and the in-memory tail
	// always; add the sink's live stream when wired.  The pump never
	// backpressures the build — dropped bytes are reconciled at finalize.
	writers := []io.Writer{logFile, tail}
	var pump *livePump
	if r.Sink != nil {
		pump = newLivePump(r.Sink.StartRun(runID))
		writers = append(writers, pump)
	}
	cw := &capWriter{w: io.MultiWriter(writers...), max: r.maxOutput(), exceeded: overflow}
	out := &lockedWriter{w: cw}

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	// Stdout and Stderr get the identical writer value, so os/exec gives
	// the child a single shared pipe — output stays interleaved in order.
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = buildEnv(r.ToolchainID, req.Env)
	setProcAttr(cmd) // own process group on unix, so the whole tree is killable

	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Status = StatusError
		res.Err = fmt.Sprintf("start %q: %v", req.Argv[0], err)
		return res
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timeout := time.NewTimer(req.Timeout)
	defer timeout.Stop()

	var waitErr error
	select {
	case waitErr = <-done:
		var ee *exec.ExitError
		switch {
		case waitErr == nil:
			res.Status = StatusOK
		case errors.As(waitErr, &ee):
			res.Status = StatusFailed
		default:
			res.Status = StatusError
			res.Err = waitErr.Error()
		}
	case <-timeout.C:
		waitErr = r.killAndWait(cmd, done)
		res.Status = StatusTimeout
	case <-overflow:
		waitErr = r.killAndWait(cmd, done)
		res.Status = StatusOverflow
	case <-ctx.Done():
		waitErr = r.killAndWait(cmd, done)
		res.Status = StatusError
		res.Err = "canceled: " + ctx.Err().Error()
	}
	res.ExitCode = exitCode(waitErr)
	res.Duration = time.Since(start)

	// cmd.Wait has returned, so the io copiers are finished: the writers
	// are safe to read without the lock.
	tail.Flush()
	res.LogBytes = cw.written
	res.LogTotalLines = tail.Total()
	res.Tail = tail.Lines()

	// Seal the sink's copy.  If the live stream dropped bytes under
	// backpressure, reconcile from the now-complete local spool before
	// finalizing, so durable history is always whole.
	if pump != nil {
		if err := pump.Close(); err != nil {
			if f, e := os.Open(res.LogPath); e == nil {
				_ = r.Sink.Reupload(runID, f)
				_ = f.Close()
			}
		}
		_ = r.Sink.Finalize(runID)
	}
	return res
}

// killAndWait terminates the whole process group: SIGTERM, a grace period,
// then SIGKILL. On Windows (dev only) it degrades to killing the
// direct child.
func (r *Runner) killAndWait(cmd *exec.Cmd, done <-chan error) error {
	terminate(cmd)
	grace := r.KillGrace
	if grace <= 0 {
		grace = defaultKillGrace
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		killHard(cmd)
		return <-done
	}
}

func (r *Runner) maxOutput() int64 {
	if r.MaxOutput > 0 {
		return r.MaxOutput
	}
	return DefaultMaxOutput
}

// buildEnv constructs the child environment from scratch — the allowlist
// invariant: PATH and HOME pass through, TOOLCHAIN_ID is set explicitly, plus
// the manifest's cache env vars.  Nothing else leaks from the server's env.
func buildEnv(toolchainID string, extra map[string]string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TOOLCHAIN_ID=" + toolchainID,
	}
	env = append(env, platformEnv()...)
	keys := make([]string, 0, len(extra))
	for k := range extra {
		if k == "PATH" || k == "HOME" || k == "TOOLCHAIN_ID" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

func exitCode(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode() // -1 when signal-killed
	}
	return -1
}

// lockedWriter serializes writes; cheap insurance in case os/exec ever
// copies stdout and stderr on separate goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// capWriter counts bytes through to w and signals exceeded once the cap is
// crossed; further output is discarded while the process is being killed.
type capWriter struct {
	w        io.Writer
	max      int64
	written  int64
	signaled bool
	exceeded chan struct{}
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if c.written >= c.max {
		c.trip()
		return n, nil // discard, but keep the pipe drained
	}
	if c.written+int64(n) > c.max {
		c.trip()
		p = p[:c.max-c.written]
	}
	if _, err := c.w.Write(p); err != nil {
		return 0, err
	}
	c.written += int64(len(p))
	return n, nil
}

func (c *capWriter) trip() {
	if !c.signaled {
		c.signaled = true
		close(c.exceeded)
	}
}

// tailBuffer keeps the last max lines written through it plus a running
// total, so a digest can report "line 4812 of 4812" without re-reading the
// log file.
type tailBuffer struct {
	max     int
	lines   []string
	head    int
	total   int
	partial []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.partial = append(t.partial, p...)
			// Bound memory on newline-free output by splitting long lines.
			for len(t.partial) >= maxLineBytes {
				t.add(string(t.partial[:maxLineBytes]))
				t.partial = append(t.partial[:0], t.partial[maxLineBytes:]...)
			}
			break
		}
		t.partial = append(t.partial, p[:i]...)
		t.add(strings.TrimSuffix(string(t.partial), "\r"))
		t.partial = t.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

func (t *tailBuffer) add(line string) {
	t.total++
	if len(t.lines) < t.max {
		t.lines = append(t.lines, line)
		return
	}
	t.lines[t.head] = line
	t.head = (t.head + 1) % t.max
}

// Flush counts a trailing unterminated line.  Call once, after the process
// has exited.
func (t *tailBuffer) Flush() {
	if len(t.partial) > 0 {
		t.add(string(t.partial))
		t.partial = nil
	}
}

func (t *tailBuffer) Total() int { return t.total }

// Lines returns the retained tail in original order.
func (t *tailBuffer) Lines() []string {
	out := make([]string, 0, len(t.lines))
	out = append(out, t.lines[t.head:]...)
	out = append(out, t.lines[:t.head]...)
	return out
}
