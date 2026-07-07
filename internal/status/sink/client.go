package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/approval"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/cellstate"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// Client is the builder side of the sink protocol.  The token lives only in
// the builder server's process — the env allowlist
// keeps it out of every spawned build.
type Client struct {
	base   string
	token  string
	stream *http.Client // no overall timeout: log streams last as long as builds
}

// NewClient targets a state service, e.g. http://state:9201.
func NewClient(base, token string) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		stream: &http.Client{},
	}
}

// LogStream streams one run's log to the sink as the build produces it.
type LogStream struct {
	pw  *io.PipeWriter
	res chan error
}

// StartRun opens the streaming POST for a run's log.  Errors surface on
// Close, never on Write — the runner's forwarder treats them as "sink copy
// incomplete" and reconciles with Reupload.
func (c *Client) StartRun(id runid.ID) *LogStream {
	pr, pw := io.Pipe()
	res := make(chan error, 1)
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/runs/"+id.String()+"/log", pr)
	if err != nil {
		pr.CloseWithError(err)
		res <- err
		return &LogStream{pw: pw, res: res}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	go func() {
		resp, err := c.stream.Do(req)
		if err != nil {
			pr.CloseWithError(err) // unblock any in-flight Write
			res <- err
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode/100 != 2 {
			res <- fmt.Errorf("sink: stream log: %s", resp.Status)
			return
		}
		res <- nil
	}()
	return &LogStream{pw: pw, res: res}
}

func (l *LogStream) Write(p []byte) (int, error) { return l.pw.Write(p) }

// Close ends the stream and reports whether the sink accepted it all.
func (l *LogStream) Close() error {
	_ = l.pw.Close()
	return <-l.res
}

// Reupload replaces a run's log wholesale — the reconciliation path when
// live streaming dropped bytes or failed.
func (c *Client) Reupload(id runid.ID, log io.Reader) error {
	return c.do(http.MethodPost, "/api/runs/"+id.String()+"/log", log, 2*time.Minute)
}

// Finalize seals a run at the sink; no further log writes are accepted.
func (c *Client) Finalize(id runid.ID) error {
	return c.do(http.MethodPost, "/api/runs/"+id.String()+"/finalize", nil, 10*time.Second)
}

// Append sends one audit record (satisfies the mcpserver Auditor).
func (c *Client) Append(rec audit.Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return c.do(http.MethodPost, "/api/audit", bytes.NewReader(b), 5*time.Second)
}

// PutStatus replaces the live status document.
func (c *Client) PutStatus(st cellstate.Status) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return c.do(http.MethodPut, "/api/status", bytes.NewReader(b), 3*time.Second)
}

// RegisterPending registers a gated op as awaiting a human decision.
func (c *Client) RegisterPending(id runid.ID, tool, path string) error {
	body, err := json.Marshal(map[string]string{"tool": tool, "path": path})
	if err != nil {
		return err
	}
	return c.do(http.MethodPost, "/api/approvals/"+id.String(), bytes.NewReader(body), 10*time.Second)
}

// PollDecision long-polls for an op's decision; it returns the current record
// (which may still be Pending if the server's hold elapsed — the caller re-polls).
func (c *Client) PollDecision(id runid.ID) (approval.Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second) // > server hold
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/approvals/"+id.String(), nil)
	if err != nil {
		return approval.Record{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.stream.Do(req)
	if err != nil {
		return approval.Record{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return approval.Record{}, fmt.Errorf("sink: poll approval %s: %s", id, resp.Status)
	}
	var rec approval.Record
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&rec); err != nil {
		return approval.Record{}, err
	}
	return rec, nil
}

// Decide sets an approve/reject decision (the status UI's action; tests too).
func (c *Client) Decide(id runid.ID, d approval.Decision) error {
	body, err := json.Marshal(map[string]approval.Decision{"decision": d})
	if err != nil {
		return err
	}
	return c.do(http.MethodPost, "/api/approvals/"+id.String()+"/decision", bytes.NewReader(body), 10*time.Second)
}

// Withdraw drops a still-pending op — the caller cancelling its own request when
// it vanished or its gate timed out.  A 409 (already decided) is benign: the
// decision stands, so it is reported as success.
func (c *Client) Withdraw(id runid.ID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/api/approvals/"+id.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("sink: withdraw approval %s: %s", id, resp.Status)
	}
	return nil
}

// PutDiff stores an op's diff payload (plain bytes; the sink gzips it).
func (c *Client) PutDiff(id runid.ID, payload []byte) error {
	return c.do(http.MethodPost, "/api/diffs/"+id.String(), bytes.NewReader(payload), 10*time.Second)
}

// PutTranscript stores a research call's URLs-only transcript, keyed by opId.
func (c *Client) PutTranscript(id runid.ID, payload []byte) error {
	return c.do(http.MethodPost, "/api/research/"+id.String(), bytes.NewReader(payload), 10*time.Second)
}

// FetchDiff reads a stored diff payload back — the scribe's get_diff path.
func (c *Client) FetchDiff(id runid.ID) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/diffs/"+id.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("sink: fetch diff %s: %s", id, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, DefaultMaxDiffBytes+1))
}

// FetchLog reads a stored run log back — get_log's fallback once the
// builder's local spool has pruned the file.
func (c *Client) FetchLog(id runid.ID) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/runs/"+id.String()+"/log", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("sink: fetch log %s: %s", id, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, DefaultMaxRunBytes+1))
}

func (c *Client) do(method, path string, body io.Reader, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("sink: %s %s: %s", method, path, resp.Status)
	}
	return nil
}
