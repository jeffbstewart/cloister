package sink

import (
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/runid"
)

var (
	errNoApproval     = errors.New("no approval for op")
	errAlreadyDecided = errors.New("approval already decided")
)

// Approval store store.  Pending records persist one JSON file each under
// /state/approvals so a state-service restart doesn't drop an in-flight decision.
// The state service is the pull-only authority: the scribe registers + polls;
// the status UI sets the decision.

// loadApprovals reads persisted pending records into memory on start.
func (s *Server) loadApprovals() error {
	entries, err := os.ReadDir(s.approvalsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.approvalsDir, e.Name()))
		if err != nil {
			continue
		}
		var rec approval.Record
		if json.Unmarshal(b, &rec) != nil || rec.OpID.IsZero() {
			continue
		}
		s.approvals[rec.OpID] = &rec
	}
	return nil
}

// saveApproval persists a record and updates the in-memory map.  Caller holds apprMu.
func (s *Server) saveApproval(rec *approval.Record) error {
	s.approvals[rec.OpID] = rec
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.approvalsDir, rec.OpID.String()+".json"), b, 0o644)
}

// current returns the record with lazy timeout expiry applied (a pending op
// past the approval timeout becomes rejected_timeout).  Caller holds apprMu.
func (s *Server) current(id runid.ID) *approval.Record {
	rec := s.approvals[id]
	if rec == nil {
		return nil
	}
	if rec.Decision == approval.Pending && time.Since(rec.CreatedAt) > s.cfg.ApprovalTimeout {
		rec.Decision = approval.Timeout
		rec.DecidedAt = time.Now().UTC()
		_ = s.saveApproval(rec)
	}
	return rec
}

// postApproval registers a gated op as pending; the state service stamps the
// authoritative CreatedAt (a stolen token cannot backdate the timeout).
func (s *Server) postApproval(w http.ResponseWriter, r *http.Request) {
	if !s.apprLimit.allow(1) {
		http.Error(w, "approval rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Tool string `json:"tool"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "bad record: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.apprMu.Lock()
	defer s.apprMu.Unlock()
	rec := &approval.Record{OpID: id, Tool: body.Tool, Path: body.Path, CreatedAt: time.Now().UTC(), Decision: approval.Pending}
	if err := s.saveApproval(rec); err != nil {
		http.Error(w, "store approval", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// withdrawApproval drops a still-pending op — the registerer cancelling its own
// request when the caller vanished or its gate timed out.  This is NOT a
// decision (one-way glass): it only removes a pending record, never sets
// an outcome.  A resolved op is left intact; an unknown op is a no-op (idempotent).
func (s *Server) withdrawApproval(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.apprMu.Lock()
	defer s.apprMu.Unlock()
	rec := s.current(id)
	if rec == nil {
		w.WriteHeader(http.StatusNoContent) // already gone
		return
	}
	if rec.Decision.Resolved() {
		http.Error(w, "already decided", http.StatusConflict)
		return
	}
	delete(s.approvals, id)
	_ = os.Remove(filepath.Join(s.approvalsDir, id.String()+".json"))
	w.WriteHeader(http.StatusNoContent)
}

// pollApproval returns the op's decision, holding the request up to ApprovalPoll
// while still pending so the scribe learns of a decision promptly with few
// requests (long-poll).
func (s *Server) pollApproval(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	deadline := time.Now().Add(s.cfg.ApprovalPoll)
	for {
		s.apprMu.Lock()
		rec := s.current(id)
		s.apprMu.Unlock()
		if rec == nil {
			http.Error(w, "no approval for op "+id.String(), http.StatusNotFound)
			return
		}
		if rec.Decision.Resolved() || time.Now().After(deadline) {
			respJSON(w, rec)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// setDecision records a terminal decision on a pending op — the shared core of
// the token API and the browser form.  It locks internally; a resolved op is
// immutable.
func (s *Server) setDecision(id runid.ID, d approval.Decision) error {
	s.apprMu.Lock()
	defer s.apprMu.Unlock()
	rec := s.current(id)
	if rec == nil {
		return errNoApproval
	}
	if rec.Decision.Resolved() {
		return fmt.Errorf("%w: %s", errAlreadyDecided, rec.Decision)
	}
	rec.Decision = d
	rec.DecidedAt = time.Now().UTC()
	return s.saveApproval(rec)
}

// decideApproval sets a decision over the token-gated API (the scribe never
// calls this — the operator's UI and tests do).
func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Decision approval.Decision `json:"decision"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "bad decision: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Decision != approval.Approved && body.Decision != approval.Rejected {
		http.Error(w, "decision must be approved or rejected", http.StatusBadRequest)
		return
	}
	switch err := s.setDecision(id, body.Decision); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errNoApproval):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errAlreadyDecided):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "store decision", http.StatusInternalServerError)
	}
}

// listPending returns the currently-pending records (expiry applied), oldest
// first, for the operator UI.
func (s *Server) listPending() []approval.Record {
	s.apprMu.Lock()
	defer s.apprMu.Unlock()
	var out []approval.Record
	for id := range s.approvals {
		if rec := s.current(id); rec != nil && rec.Decision == approval.Pending {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// diffText reads and decompresses an op's stored diff for inline display ("" if
// none / unreadable).
func (s *Server) diffText(id runid.ID) string {
	f, err := os.Open(filepath.Join(s.diffsDir, id.Shard(), id.String()+".diff.gz"))
	if err != nil {
		return ""
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return ""
	}
	defer gz.Close()
	b, _ := io.ReadAll(io.LimitReader(gz, s.cfg.MaxDiffBytes))
	return string(b)
}

type approvalItem struct {
	Rec  approval.Record
	Diff string
}

// approvalsPage renders the operator's pending-approvals view: each pending op
// with its full diff and Approve/Reject controls.  Unauthenticated (localhost via
// the relay); the diff is agent-influenced, so html/template auto-escapes it.
func (s *Server) approvalsPage(w http.ResponseWriter, r *http.Request) {
	items := make([]approvalItem, 0)
	for _, rec := range s.listPending() {
		items = append(items, approvalItem{Rec: rec, Diff: s.diffText(rec.OpID)})
	}
	data := struct {
		CSRF  string
		Items []approvalItem
	}{s.csrfToken, items}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := approvalsTmpl.Execute(w, data); err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
	}
}

// approvalsDecide handles the Approve/Reject form POST. CSRF-checked (the token
// is only present in the same-origin page a cross-origin attacker cannot read).
func (s *Server) approvalsDecide(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(s.csrfToken)) != 1 {
		http.Error(w, "bad or missing CSRF token", http.StatusForbidden)
		return
	}
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var d approval.Decision
	switch r.FormValue("decision") {
	case "approve":
		d = approval.Approved
	case "reject":
		d = approval.Rejected
	default:
		http.Error(w, "decision must be approve or reject", http.StatusBadRequest)
		return
	}
	if err := s.setDecision(id, d); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/approvals", http.StatusSeeOther)
}

var approvalsTmpl = template.Must(template.New("approvals").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="10">
<title>pending approvals</title>
<style>
body{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;margin:2em;background:#111;color:#ddd}
a{color:#8cf}h1{font-size:1.2em}h2{font-size:1em}
.op{border:1px solid #444;padding:.6em 1em;margin:1em 0}
pre{background:#0a0a0a;padding:.6em;overflow:auto;max-height:28em;font-size:.82em}
button{font:inherit;padding:.3em .9em;margin-right:.6em;cursor:pointer}
.approve{background:#163;color:#dfd;border:1px solid #4a4}
.reject{background:#611;color:#fdd;border:1px solid #a44}
.meta{color:#9cf;font-size:.85em}
footer{margin-top:2em;color:#777;font-size:.8em}
</style></head><body>
<h1>pending approvals</h1>
<p><a href="/">&larr; back to status</a> &middot; refreshes every 10s</p>
{{range .Items}}
<div class="op">
<h2>{{.Rec.Tool}} &mdash; {{.Rec.Path}}</h2>
<p class="meta">opId {{.Rec.OpID}} &middot; requested {{.Rec.CreatedAt}}</p>
<pre>{{.Diff}}</pre>
<form method="post" action="/approvals/{{.Rec.OpID}}/decision">
<input type="hidden" name="csrf" value="{{$.CSRF}}">
<button class="approve" name="decision" value="approve">Approve</button>
<button class="reject" name="decision" value="reject">Reject</button>
</form>
</div>
{{else}}
<p>none pending</p>
{{end}}
<footer>agent-builder &mdash; approvals are recorded in the audit trail</footer>
</body></html>
`))

func respJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
