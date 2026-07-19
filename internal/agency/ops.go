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

package agency

import (
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// opLedgerSize bounds the completed-operations history the status snapshot
// publishes: enough to see what the door has been doing lately, small enough
// that the snapshot stays a glance, not an archive.
const opLedgerSize = 50

// opTailSize bounds the response tail retained for the token scan.  The
// usage object rides the end of a completion — the final SSE chunk or the
// tail of a JSON body — so a small tail is enough.
const opTailSize = 8 << 10

// callerMaxLen caps the caller identity echoed into the snapshot.
const callerMaxLen = 64

// OpRecord is one completed operation as the status snapshot publishes it:
// who asked, for which class, who served (empty when refused), and what it
// cost.  Timestamps are the agency's clock.
type OpRecord struct {
	FinishedAt time.Time `json:"ts"`
	// Caller is the consumer's self-declared CallerHeader when present,
	// else the remote host the request came from.
	Caller string `json:"caller"`
	// Class is the engine class asked for; "(invalid)" when the model
	// field failed validation, so untrusted bytes are never echoed.
	Class string `json:"class"`
	// ServedBy is the node/model that answered; empty on a refusal.
	ServedBy string `json:"servedBy,omitempty"`
	// Status is the HTTP status the consumer received.
	Status int `json:"status"`
	// QueueWait is the admission wait actually paid across the chain walk.
	QueueWait Duration `json:"queueWait"`
	// Total is queue + forward + decode, request arrival to stream end.
	Total Duration `json:"total"`
	// Tokens is the completion's usage total, best-effort: scanned from
	// the response tail, 0 when the engine reported none.
	Tokens int `json:"tokens,omitempty"`
}

// opLedger keeps the last opLedgerSize completed operations, oldest first.
type opLedger struct {
	mu  sync.Mutex
	ops []OpRecord
}

func (l *opLedger) record(op OpRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
	if len(l.ops) > opLedgerSize {
		l.ops = l.ops[len(l.ops)-opLedgerSize:]
	}
}

// history returns a copy of the ledger, oldest first.
func (l *opLedger) history() []OpRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]OpRecord, len(l.ops))
	copy(out, l.ops)
	return out
}

// opRecorder wraps the consumer's ResponseWriter to observe an operation
// without altering it: the status code sent, and a rolling tail of the body
// for the token scan.  Flush passes through so streaming stays
// token-by-token.
type opRecorder struct {
	http.ResponseWriter
	status int
	tail   []byte
}

func (r *opRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *opRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.tail = append(r.tail, b...)
	if len(r.tail) > opTailSize {
		r.tail = r.tail[len(r.tail)-opTailSize:]
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so the proxy's token-by-token
// flushing survives the wrap.
func (r *opRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// totalTokensPattern matches the usage total in either response shape — the
// tail of a JSON body or the final SSE chunk.
var totalTokensPattern = regexp.MustCompile(`"total_tokens"\s*:\s*(\d+)`)

// tokens scans the retained tail for the usage total, last match wins (a
// stream's final chunk carries the authoritative count).  Zero when the
// engine reported none — a best effort, never a parse of the whole body.
func (r *opRecorder) tokens() int {
	matches := totalTokensPattern.FindAllSubmatch(r.tail, -1)
	if len(matches) == 0 {
		return 0
	}
	n, err := strconv.Atoi(string(matches[len(matches)-1][1]))
	if err != nil {
		return 0
	}
	return n
}

// callerIdentity names the consumer for the op ledger: its self-declared
// CallerHeader when present, else the host it dialed from.  Capped so the
// snapshot never carries an unbounded untrusted string.
func callerIdentity(r *http.Request) string {
	caller := r.Header.Get(CallerHeader)
	if caller == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		caller = host
	}
	if len(caller) > callerMaxLen {
		caller = caller[:callerMaxLen]
	}
	return caller
}
