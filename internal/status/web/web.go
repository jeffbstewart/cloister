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

// Package web serves the human-facing observability pages for a cell — the
// oversight surface for every worker in it: live queue state, the audit
// trail (builder commands, scribe mutations, scholar searches and research),
// full run logs, stored mutation diffs, and the pending-approvals banner.
// It reads ONLY from the cell's /state volume (mounted read-only) — it has
// no MCP surface and no route to any worker, and it belongs on a
// host-published network the agent cannot reach.
//
// Log and param content is agent-influenced, so logs are served as
// text/plain with X-Content-Type-Options: nosniff, and all HTML goes
// through auto-escaping templates: hostile build output must not become
// script in the operator's browser.
package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/cellstate"
	"github.com/jeffbstewart/cloister/internal/runid"
)

const (
	dashboardRecords = 20
	auditPageRecords = 200
	maxTailLines     = 5000
)

// Config points the server at a builder's state volume.
type Config struct {
	StateDir string
	Version  string
}

// Server renders the status pages.
type Server struct {
	cfg          Config
	statusPath   string
	auditPath    string
	logsDir      string
	diffsDir     string
	approvalsDir string
}

// New builds a status server over the given state directory.
func New(cfg Config) *Server {
	return &Server{
		cfg:          cfg,
		statusPath:   filepath.Join(cfg.StateDir, "status.json"),
		auditPath:    filepath.Join(cfg.StateDir, "audit.jsonl"),
		logsDir:      filepath.Join(cfg.StateDir, "logs"),
		diffsDir:     filepath.Join(cfg.StateDir, "diffs"),
		approvalsDir: filepath.Join(cfg.StateDir, "approvals"),
	}
}

// Handler serves the dashboard at /, the audit tail at /audit, run logs at
// /log/{runId}, and a liveness probe at /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /audit", s.auditPage)
	mux.HandleFunc("GET /log/{runId}", s.logPage)
	mux.HandleFunc("GET /diff/{opId}", s.diffPage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	return noSniff(mux)
}

func noSniff(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

var tmplFuncs = template.FuncMap{
	// deref renders an optional exit code (audit stores it as *int so 0
	// survives omitempty semantics).
	"deref": func(p *int) any {
		if p == nil {
			return ""
		}
		return *p
	},
	// local renders a stored-UTC instant in the server's zone; set TZ on
	// the status container to choose it (tzdata is compiled in).
	"local": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04:05 MST")
	},
}

const pageHead = `<!doctype html>
<html><head><meta charset="utf-8">
<meta http-equiv="refresh" content="5">
<title>agent-builder status</title>
<style>
body{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;margin:2em;background:#111;color:#ddd}
a{color:#8cf}
table{border-collapse:collapse;margin-top:.5em}
td,th{border:1px solid #444;padding:.25em .6em;text-align:left;font-size:.9em}
.busy{color:#fc6}
.run,.ok{color:#6f6}
.failed,.timeout,.output_overflow,.error{color:#f66}
.rejected_param,.rejected_busy,.rejected_no_manifest{color:#f96}
.applied,.no_change{color:#6f6}
.dry_run{color:#9cf}
.rejected,.rejected_confinement,.rejected_gate,.rejected_pattern,.rejected_overflow,.rejected_timeout{color:#f96}
.pending_approval{color:#fc6}
.added{color:#6f6}.removed{color:#f66}
.argv{color:#9cf;max-width:44em;overflow-wrap:anywhere;font-size:.82em}
h1{font-size:1.2em}h2{font-size:1em;margin-top:1.5em}
footer{margin-top:2em;color:#777;font-size:.8em}
.pendingbanner{background:#3a2c00;border:1px solid #fc6;color:#fc6;padding:.6em 1em;margin-bottom:1em}
.pendingbanner a{color:#fc6}
</style></head><body>
{{if .PendingApprovals}}<div class="pendingbanner">&#9888; {{.PendingApprovals}} change(s) awaiting your approval &mdash; <a href="/approvals">review &amp; approve/reject</a></div>{{end}}
`

const auditTable = `<table>
<tr><th>time</th><th>tool</th><th>target</th><th>decision</th><th>status</th><th>exit / &plusmn;</th><th>ms</th><th>params</th><th>log / diff</th></tr>
{{range .Records}}<tr>
<td>{{local .Time}}</td>
<td>{{.Tool}}</td>
<td class="argv">{{with .Mutation}}{{.Path}}{{end}}{{with .Command}}{{range .Argv}}{{.}} {{end}}{{end}}{{with .Search}}{{.Query}}{{end}}{{with .Extract}}{{.URL}}{{end}}{{with .Research}}{{.Query}}{{end}}</td>
<td class="{{.Decision}}">{{if eq .Decision "pending_approval"}}<a href="/approvals">{{.Decision}}</a>{{else}}{{.Decision}}{{end}}</td>
<td class="{{.Status}}">{{.Status}}</td>
<td>{{with .Mutation}}{{if .FilesTouched}}<span class="added">+{{.LinesAdded}}</span>/<span class="removed">-{{.LinesRemoved}}</span>{{if .DiffTruncated}} <span class="removed">(trunc)</span>{{end}}{{end}}{{end}}{{with .Command}}{{deref .ExitCode}}{{end}}</td>
<td>{{if .Duration}}{{.Duration.Std.Milliseconds}}{{end}}</td>
<td>{{with .Command}}{{range $k, $v := .Params}}{{$k}}={{$v}} {{end}}{{end}}{{with .Mutation}}{{if .From}}from={{.From}} to={{.To}} {{end}}{{end}}{{with .Research}}answerBytes={{.AnswerBytes}} {{end}}</td>
<td>{{if and .Command .Command.LogPath}}<a href="/log/{{.RunID}}">full</a> <a href="/log/{{.RunID}}?tail=200">tail</a>{{else if and .Mutation .Mutation.HasDiff}}<a href="/diff/{{.RunID}}">diff</a>{{else if and .Research .Research.TranscriptStored}}<a href="/research/{{.RunID}}">transcript</a>{{end}}</td>
</tr>{{end}}
{{if not .Records}}<tr><td colspan="9">none</td></tr>{{end}}
</table>
`

var dashboardTmpl = template.Must(template.New("dashboard").Funcs(tmplFuncs).Parse(pageHead + `
<h1>agent-builder &mdash; builder cell</h1>
{{if not .HaveStatus}}
<p>no runs recorded yet</p>
{{else if .Status.Busy}}{{with .Status.Active}}
<p class="busy">RUNNING <b>{{.Action}}</b> since {{local .StartedAt}}
&mdash; <a href="/log/{{.RunID}}?tail=200">live tail</a> ({{.RunID}})</p>
{{end}}{{else}}
<p>idle{{with .Status.LastRun}} &mdash; last: {{.Action}}
<span class="{{.Status}}">{{.Status}}</span>
(<a href="/log/{{.RunID}}">{{.RunID}}</a>){{end}},
as of {{local .Status.UpdatedAt}}</p>
{{end}}
<h2>recent action calls (newest first)</h2>
` + auditTable + `
<p><a href="/audit">longer audit tail</a> &middot; <a href="/approvals">pending approvals</a></p>
<footer>agent-builder {{.Version}} &mdash; read-only view of /state; refreshes every 5s</footer>
</body></html>
`))

var auditTmpl = template.Must(template.New("audit").Funcs(tmplFuncs).Parse(pageHead + `
<h1>agent-builder &mdash; audit tail</h1>
<p><a href="/">back to status</a></p>
` + auditTable + `</body></html>
`))

type pageData struct {
	Version          string
	HaveStatus       bool
	Status           cellstate.Status
	Records          []audit.Record
	PendingApprovals int
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	data := pageData{Version: s.cfg.Version, PendingApprovals: s.countPending()}
	if st, err := cellstate.Read(s.statusPath); err == nil {
		data.HaveStatus = true
		data.Status = st
	}
	data.Records = s.readAuditTail(dashboardRecords)
	renderHTML(w, dashboardTmpl, data)
}

// countPending counts the approvals still awaiting a human (a hint for the
// banner; expiry is applied lazily by the state service, so a swept one drops
// off within a poll cycle).
func (s *Server) countPending() int {
	entries, err := os.ReadDir(s.approvalsDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.approvalsDir, e.Name()))
		if err != nil {
			continue
		}
		var rec approval.Record
		if json.Unmarshal(b, &rec) == nil && rec.Decision == approval.Pending {
			n++
		}
	}
	return n
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	renderHTML(w, auditTmpl, pageData{Records: s.readAuditTail(auditPageRecords), PendingApprovals: s.countPending()})
}

// logPage serves a run log verbatim as text/plain (optionally only the
// last ?tail=N lines). runid.Parse is the trust boundary that keeps the
// path join traversal-proof.
func (s *Server) logPage(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("runId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tailN := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		tailN, err = strconv.Atoi(v)
		if err != nil || tailN < 1 {
			http.Error(w, "tail must be a positive integer", http.StatusBadRequest)
			return
		}
		if tailN > maxTailLines {
			tailN = maxTailLines
		}
	}
	path := filepath.Join(s.logsDir, id.String()+".log")

	if tailN == 0 {
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, "no log for run "+id.String(), http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.Copy(w, f)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "no log for run "+id.String(), http.StatusNotFound)
		return
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) > tailN {
		lines = lines[len(lines)-tailN:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, strings.Join(lines, "\n")+"\n")
}

// diffPage serves a stored mutation diff, decompressed, as text/plain (the
// nosniff wrapper covers it). runid.Parse is the traversal-proof trust boundary.
func (s *Server) diffPage(w http.ResponseWriter, r *http.Request) {
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

// readAuditTail returns up to n most-recent audit records, newest first.
// If the current file is short (it just rotated), the previous generation
// tops up the view.  Malformed lines are skipped, not fatal — observability
// over strictness.
func (s *Server) readAuditTail(n int) []audit.Record {
	lines := tailLines(s.auditPath+".1", n)
	lines = append(lines, tailLines(s.auditPath, n)...)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]audit.Record, 0, len(lines))
	seen := map[runid.ID]bool{}
	for i := len(lines) - 1; i >= 0; i-- { // newest first
		var rec audit.Record
		if json.Unmarshal([]byte(lines[i]), &rec) != nil {
			continue
		}
		// Collapse a gated op's two audit lines to one.  The op writes
		// pending_approval, then a terminal decision (applied / rejected /
		// rejected_timeout) under the SAME opId; once the terminal exists, the
		// pending line is stale, so drop it.  Show pending_approval only while it's
		// genuinely the latest word on that op.  Records without an opId are never
		// collapsed. (The append-only log keeps both; this is display-only.)
		if !rec.RunID.IsZero() {
			if rec.Decision == "pending_approval" && seen[rec.RunID] {
				continue
			}
			seen[rec.RunID] = true
		}
		out = append(out, rec)
	}
	return out
}

// tailLines returns up to the last n lines of a file ("" slice for a
// missing file).
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return lines
}

func renderHTML(w http.ResponseWriter, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
