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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpLedgerCapsHistory(t *testing.T) {
	l := &opLedger{}
	for i := 0; i < opLedgerSize+5; i++ {
		l.record(OpRecord{Caller: fmt.Sprintf("op-%d", i)})
	}
	ops := l.history()
	if len(ops) != opLedgerSize {
		t.Fatalf("history = %d ops, want capped at %d", len(ops), opLedgerSize)
	}
	if ops[0].Caller != "op-5" || ops[len(ops)-1].Caller != fmt.Sprintf("op-%d", opLedgerSize+4) {
		t.Errorf("history spans %s..%s, want the oldest trimmed and order kept",
			ops[0].Caller, ops[len(ops)-1].Caller)
	}
}

func TestOpRecorderCapturesStatusAndTokens(t *testing.T) {
	cases := []struct {
		name       string
		status     int // 0 = no explicit WriteHeader
		body       []string
		wantStatus int
		wantTokens int
	}{
		{"plain JSON with usage", http.StatusOK,
			[]string{`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":32,"total_tokens":42}}`},
			http.StatusOK, 42},
		{"SSE stream, final chunk carries usage", 0,
			[]string{
				"data: {\"delta\":\"one\"}\n\n",
				"data: {\"delta\":\"two\",\"usage\":{\"total_tokens\":7}}\n\ndata: [DONE]\n\n",
			},
			http.StatusOK, 7},
		{"no usage reported", http.StatusOK, []string{`{"choices":[]}`}, http.StatusOK, 0},
		{"refusal", http.StatusNotFound, []string{"agency: unknown engine class"}, http.StatusNotFound, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &opRecorder{ResponseWriter: httptest.NewRecorder()}
			if tc.status != 0 {
				rec.WriteHeader(tc.status)
			}
			for _, chunk := range tc.body {
				io.WriteString(rec, chunk)
			}
			if rec.status != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.status, tc.wantStatus)
			}
			if got := rec.tokens(); got != tc.wantTokens {
				t.Errorf("tokens = %d, want %d", got, tc.wantTokens)
			}
		})
	}
}

// TestOpRecorderTokensSurviveTailTrim: usage rides the END of a long stream,
// so the rolling tail must still hold it after far more than opTailSize has
// streamed through.
func TestOpRecorderTokensSurviveTailTrim(t *testing.T) {
	rec := &opRecorder{ResponseWriter: httptest.NewRecorder()}
	filler := "data: {\"delta\":\"" + strings.Repeat("x", 100) + "\"}\n\n"
	for written := 0; written < 3*opTailSize; written += len(filler) {
		io.WriteString(rec, filler)
	}
	io.WriteString(rec, "data: {\"usage\":{\"total_tokens\":1234}}\n\n")
	if got := rec.tokens(); got != 1234 {
		t.Errorf("tokens = %d after a long stream, want 1234 from the tail", got)
	}
}

func TestCallerIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "cell-agent:51234"
	if got := callerIdentity(req); got != "cell-agent" {
		t.Errorf("callerIdentity without header = %q, want the remote host", got)
	}

	req.Header.Set(CallerHeader, "librarian")
	if got := callerIdentity(req); got != "librarian" {
		t.Errorf("callerIdentity = %q, want the self-declared header", got)
	}

	req.Header.Set(CallerHeader, strings.Repeat("z", 200))
	if got := callerIdentity(req); len(got) != callerMaxLen {
		t.Errorf("callerIdentity length = %d, want capped at %d", len(got), callerMaxLen)
	}
}
