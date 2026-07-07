// Package sink is the state service of a cell: the sole owner
// of the /state volume.  The builder streams run logs, audit records, and
// status updates to it over an internal network with a bearer token; it
// enforces append-only history, stamps times with its own clock, rate-limits
// writers, and serves the human status pages.
//
// Separation of concerns is the security property: the
// container that executes agent-authored build code has no filesystem
// access to the record of what it did.  A hostile build can at worst inject
// bytes into its *own, still-open* run stream — which it controls anyway,
// since the log is its stdout — and can never rewrite history, other runs,
// or the audit trail.
package sink

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/cellstate"
	"github.com/jeffbstewart/cloister/internal/runid"
	"github.com/jeffbstewart/cloister/internal/status/web"
)

// Defaults for Config's zero values.
const (
	DefaultMaxRunBytes    = 50 << 20 // mirrors the runner's output cap
	DefaultLogBytesPerSec = 16 << 20
	DefaultAuditPerSec    = 20
	DefaultStatusPerSec   = 10
	DefaultMaxDiffBytes   = 1 << 20 // per-op diff payload cap; hard-enforced on store

	DefaultMaxTranscriptBytes  = 4 << 20 // per-op transcript store backstop (scholar caps lower)
	DefaultTranscriptRetention = 1 << 30 // research transcript store total cap; oldest pruned

	DefaultApprovalTimeout = time.Hour        // a pending op unapproved this long → rejected_timeout
	DefaultApprovalPoll    = 30 * time.Second // how long a decision long-poll holds before returning "still pending"
	DefaultApprovalPerSec  = 10               // pending-approval registrations/sec, burst 2×
)

// Config wires the state service.  Token is required: the service refuses
// to start with an open write API.
type Config struct {
	StateDir string
	Token    string
	Version  string

	MaxRunBytes    int64   // per-run log cap; 0 → DefaultMaxRunBytes
	LogBytesPerSec float64 // log stream pacing; 0 → DefaultLogBytesPerSec
	AuditPerSec    float64 // audit appends/sec (burst 2×); 0 → DefaultAuditPerSec
	StatusPerSec   float64 // status updates/sec (burst 2×); 0 → DefaultStatusPerSec
	MaxDiffBytes   int64   // per-op diff payload cap; 0 → DefaultMaxDiffBytes

	ApprovalTimeout time.Duration // pending → rejected_timeout after this; 0 → DefaultApprovalTimeout
	ApprovalPoll    time.Duration // decision long-poll hold; 0 → DefaultApprovalPoll
	ApprovalPerSec  float64       // pending-approval registrations/sec (burst 2×); 0 → DefaultApprovalPerSec

	MaxTranscriptBytes  int64 // per-op research transcript store backstop; 0 → DefaultMaxTranscriptBytes
	TranscriptRetention int64 // research transcript store total cap (oldest pruned); 0 → DefaultTranscriptRetention
}

// Server owns /state and serves both the write API (token-gated, for the
// builder) and the status pages (read-only, for the operator's relay).
type Server struct {
	cfg          Config
	logsDir      string
	diffsDir     string
	approvalsDir string
	researchDir  string
	statusPath   string
	auditLog     *audit.Log
	web          *web.Server

	logBytes    *bucket
	auditLimit  *bucket
	statusLimit *bucket
	apprLimit   *bucket

	mu        sync.Mutex
	finalized map[runid.ID]bool

	apprMu    sync.Mutex
	approvals map[runid.ID]*approval.Record
	csrfToken string // synchronizer token for the unauthenticated approve/reject form
}

// New prepares the state directory and opens the audit log.
func New(cfg Config) (*Server, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("statesink: StateDir required")
	}
	if cfg.Token == "" {
		return nil, errors.New("statesink: refusing to serve a write API without a bearer token (set STATE_TOKEN)")
	}
	if cfg.MaxRunBytes <= 0 {
		cfg.MaxRunBytes = DefaultMaxRunBytes
	}
	if cfg.LogBytesPerSec <= 0 {
		cfg.LogBytesPerSec = DefaultLogBytesPerSec
	}
	if cfg.AuditPerSec <= 0 {
		cfg.AuditPerSec = DefaultAuditPerSec
	}
	if cfg.StatusPerSec <= 0 {
		cfg.StatusPerSec = DefaultStatusPerSec
	}
	if cfg.MaxDiffBytes <= 0 {
		cfg.MaxDiffBytes = DefaultMaxDiffBytes
	}
	if cfg.ApprovalTimeout <= 0 {
		cfg.ApprovalTimeout = DefaultApprovalTimeout
	}
	if cfg.ApprovalPoll <= 0 {
		cfg.ApprovalPoll = DefaultApprovalPoll
	}
	if cfg.ApprovalPerSec <= 0 {
		cfg.ApprovalPerSec = DefaultApprovalPerSec
	}
	if cfg.MaxTranscriptBytes <= 0 {
		cfg.MaxTranscriptBytes = DefaultMaxTranscriptBytes
	}
	if cfg.TranscriptRetention <= 0 {
		cfg.TranscriptRetention = DefaultTranscriptRetention
	}

	logsDir := filepath.Join(cfg.StateDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, err
	}
	diffsDir := filepath.Join(cfg.StateDir, "diffs")
	if err := os.MkdirAll(diffsDir, 0o755); err != nil {
		return nil, err
	}
	approvalsDir := filepath.Join(cfg.StateDir, "approvals")
	if err := os.MkdirAll(approvalsDir, 0o755); err != nil {
		return nil, err
	}
	researchDir := filepath.Join(cfg.StateDir, "research")
	if err := os.MkdirAll(researchDir, 0o755); err != nil {
		return nil, err
	}
	auditLog, err := audit.Open(filepath.Join(cfg.StateDir, "audit.jsonl"), audit.Options{})
	if err != nil {
		return nil, err
	}
	csrf := make([]byte, 16)
	if _, err := rand.Read(csrf); err != nil {
		return nil, err
	}
	s := &Server{
		csrfToken:    hex.EncodeToString(csrf),
		cfg:          cfg,
		logsDir:      logsDir,
		diffsDir:     diffsDir,
		approvalsDir: approvalsDir,
		researchDir:  researchDir,
		statusPath:   filepath.Join(cfg.StateDir, "status.json"),
		auditLog:     auditLog,
		web:          web.New(web.Config{StateDir: cfg.StateDir, Version: cfg.Version}),
		logBytes:     newBucket(cfg.LogBytesPerSec, cfg.LogBytesPerSec),
		auditLimit:   newBucket(cfg.AuditPerSec, cfg.AuditPerSec*2),
		statusLimit:  newBucket(cfg.StatusPerSec, cfg.StatusPerSec*2),
		apprLimit:    newBucket(cfg.ApprovalPerSec, cfg.ApprovalPerSec*2),
		finalized:    map[runid.ID]bool{},
		approvals:    map[runid.ID]*approval.Record{},
	}
	if err := s.loadApprovals(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases the audit log.
func (s *Server) Close() error { return s.auditLog.Close() }

// Handler routes the token-gated write/read API under /api and the
// unauthenticated status pages (including /healthz) everywhere else.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runs/{runId}/log", s.auth(s.postLog))
	mux.HandleFunc("POST /api/runs/{runId}/finalize", s.auth(s.finalizeRun))
	mux.HandleFunc("GET /api/runs/{runId}/log", s.auth(s.readLog))
	mux.HandleFunc("POST /api/audit", s.auth(s.postAudit))
	mux.HandleFunc("POST /api/diffs/{opId}", s.auth(s.postDiff))
	mux.HandleFunc("GET /api/diffs/{opId}", s.auth(s.readDiff))
	mux.HandleFunc("POST /api/approvals/{opId}", s.auth(s.postApproval))
	mux.HandleFunc("GET /api/approvals/{opId}", s.auth(s.pollApproval))
	mux.HandleFunc("DELETE /api/approvals/{opId}", s.auth(s.withdrawApproval))
	mux.HandleFunc("POST /api/approvals/{opId}/decision", s.auth(s.decideApproval))
	mux.HandleFunc("POST /api/research/{opId}", s.auth(s.postTranscript))
	mux.HandleFunc("PUT /api/status", s.auth(s.putStatus))
	// Operator approval UI — unauthenticated (localhost-only via the relay) but
	// CSRF-protected.  Registered before the "/" catch-all so it wins routing.
	mux.HandleFunc("GET /approvals", s.approvalsPage)
	mux.HandleFunc("POST /approvals/{opId}/decision", s.approvalsDecide)
	// Research transcript view — unauthenticated (localhost via the relay), read
	// only, text/plain + nosniff like the log/diff views.
	mux.HandleFunc("GET /research/{opId}", s.researchPage)
	mux.Handle("/", s.web.Handler())
	return mux
}

// auth gates the API on the bearer token, compared in constant time.  The
// token also defeats browser-origin CSRF against the localhost-published
// relay: a cross-origin page cannot attach the Authorization header.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// postLog stores a run's log from the request body — one streaming POST
// per run, written to disk as it arrives so the status pages can tail it
// live.  A retry or reconciliation re-POST replaces the file wholesale;
// finalized runs accept nothing.
func (s *Server) postLog(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("runId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.isFinalized(id) {
		http.Error(w, "run is finalized", http.StatusConflict)
		return
	}
	f, err := os.OpenFile(filepath.Join(s.logsDir, id.String()+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, "open log", http.StatusInternalServerError)
		return
	}
	_, copyErr := s.copyPaced(f, io.LimitReader(r.Body, s.cfg.MaxRunBytes))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		http.Error(w, "store log", http.StatusInternalServerError)
		return
	}
	var probe [1]byte
	if n, _ := r.Body.Read(probe[:]); n > 0 {
		http.Error(w, "log exceeds the per-run size cap", http.StatusRequestEntityTooLarge)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// copyPaced copies src to dst while drawing log-byte tokens, so stream
// throughput is bounded server-side regardless of the writer's behavior.
func (s *Server) copyPaced(dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			s.logBytes.take(float64(n))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// finalizeRun seals a run: its log becomes immutable history.
func (s *Server) finalizeRun(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("runId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.finalized[id] = true
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) isFinalized(id runid.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalized[id]
}

// readLog returns a stored run log verbatim — the read-back path for the
// builder's get_log tool once its local spool has pruned the file.
func (s *Server) readLog(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("runId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(filepath.Join(s.logsDir, id.String()+".log"))
	if err != nil {
		http.Error(w, "no log for run "+id.String(), http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, f)
}

// postAudit appends one audit record.  The sink's clock is authoritative:
// any client-supplied timestamp is discarded before the append stamps a
// fresh one, so even a stolen token cannot backdate history.
func (s *Server) postAudit(w http.ResponseWriter, r *http.Request) {
	if !s.auditLimit.allow(1) {
		http.Error(w, "audit rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	var rec audit.Record
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&rec); err != nil {
		http.Error(w, "bad record: "+err.Error(), http.StatusBadRequest)
		return
	}
	rec.Time = time.Time{} // discard the client's clock; Append stamps ours
	if err := s.auditLog.Append(rec); err != nil {
		http.Error(w, "append", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postDiff stores an op's diff payload — gzip'd at diffs/<shard>/<opId>.diff.gz,
// sharded on the opId's random tail.  Corruption-isolated: one bad file
// costs exactly one diff.  The size cap is the backstop; the scribe truncates and
// flags before upload.
func (s *Server) postDiff(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.diffsDir, id.Shard())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "mkdir", http.StatusInternalServerError)
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, id.String()+".diff.gz"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, "open diff", http.StatusInternalServerError)
		return
	}
	gz := gzip.NewWriter(f)
	_, copyErr := io.Copy(gz, io.LimitReader(r.Body, s.cfg.MaxDiffBytes))
	gzErr := gz.Close()
	closeErr := f.Close()
	if copyErr != nil || gzErr != nil || closeErr != nil {
		http.Error(w, "store diff", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readDiff serves a stored diff payload decompressed — the read-back path for
// the scribe's get_diff tool.
func (s *Server) readDiff(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(filepath.Join(s.diffsDir, id.Shard(), id.String()+".diff.gz"))
	if err != nil {
		http.Error(w, "no diff for op "+id.String(), http.StatusNotFound)
		return
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		http.Error(w, "corrupt diff", http.StatusInternalServerError)
		return
	}
	defer gz.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, gz)
}

// putStatus replaces the live status document; cellstate.WriteFile stamps
// UpdatedAt with this server's clock.
func (s *Server) putStatus(w http.ResponseWriter, r *http.Request) {
	if !s.statusLimit.allow(1) {
		http.Error(w, "status rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	var st cellstate.Status
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&st); err != nil {
		http.Error(w, "bad status: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := cellstate.WriteFile(s.statusPath, st); err != nil {
		http.Error(w, "write status", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
