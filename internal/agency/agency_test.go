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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestServer(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()
	srv, err := New(Config{UpstreamURL: upstreamURL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestNewFailsClosedOnBadConfig(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
	}{
		{"empty", ""},
		{"unparseable", "http://bad url with spaces"},
		{"no host", "http://"},
		{"bad scheme", "ftp://infer:11434"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Config{UpstreamURL: tc.upstream}); err == nil {
				t.Errorf("New(%q) succeeded, want error", tc.upstream)
			}
		})
	}
}

// TestNewRejectsBothModes: pass-through and class routing are a deliberate
// either/or, never resolved by precedence.
func TestNewRejectsBothModes(t *testing.T) {
	if _, err := New(Config{UpstreamURL: "http://infer:11434", Routes: &RouterConfig{}}); err == nil {
		t.Error("New with both UpstreamURL and Routes succeeded, want error")
	}
}

// TestPassThrough proves the door is behaviorally invisible for /v1 traffic:
// method, path, query, request headers, and body reach the upstream intact
// (with the Host header presented as the upstream's own name), and the
// upstream's status, headers, and body come back intact — plus the one
// addition, the Agency-Served-By provenance header.
func TestPassThrough(t *testing.T) {
	var got struct {
		method, path, query, host, auth, body string
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.RawQuery
		got.host, got.auth, got.body = r.Host, r.Header.Get("Authorization"), string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer upstream.Close()

	ts := newTestServer(t, upstream.URL)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions?probe=1",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ignored-for-local-use")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got.method != http.MethodPost || got.path != "/v1/chat/completions" || got.query != "probe=1" {
		t.Errorf("upstream saw %s %s?%s, want POST /v1/chat/completions?probe=1", got.method, got.path, got.query)
	}
	if wantHost := strings.TrimPrefix(upstream.URL, "http://"); got.host != wantHost {
		t.Errorf("upstream saw Host %q, want %q", got.host, wantHost)
	}
	if got.auth != "Bearer ignored-for-local-use" {
		t.Errorf("upstream saw Authorization %q, want the consumer's header intact", got.auth)
	}
	if got.body != `{"model":"m"}` {
		t.Errorf("upstream saw body %q, want the consumer's body intact", got.body)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want the upstream's header intact", ct)
	}
	if served := resp.Header.Get("Agency-Served-By"); served != "127.0.0.1" {
		t.Errorf("Agency-Served-By = %q, want the upstream hostname", served)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"choices":[]}` {
		t.Errorf("body = %q, want the upstream's body intact", body)
	}
}

// TestStreamingForwardsChunksAsTheyArrive holds the upstream's second chunk
// back until the client has read the first through the agency.  If the door
// buffered responses whole, the first read could not complete before the
// upstream handler returns, and the handler only returns after the read —
// so a buffering regression deadlocks (and fails on the test timeout)
// instead of passing by luck.
func TestStreamingForwardsChunksAsTheyArrive(t *testing.T) {
	const chunk1 = "data: {\"delta\":\"one\"}\n\n"
	const chunk2 = "data: [DONE]\n\n"
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunk1)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, chunk2)
	}))
	defer upstream.Close()

	ts := newTestServer(t, upstream.URL)
	resp, err := http.Get(ts.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	first := make([]byte, len(chunk1))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if string(first) != chunk1 {
		t.Errorf("first chunk = %q, want %q", first, chunk1)
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	if string(rest) != chunk2 {
		t.Errorf("rest = %q, want %q", rest, chunk2)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

// TestRefusesOutsideV1 is the containment check: the raw model-server API
// (ollama's native endpoints — pulls, deletes, tags) does not pass the door,
// and a refused request never touches the upstream at all.
func TestRefusesOutsideV1(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()

	ts := newTestServer(t, upstream.URL)
	for _, path := range []string{"/api/tags", "/api/pull", "/api/delete", "/"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Errorf("refused paths reached the upstream %d times, want 0", n)
	}
}

func TestHealthz(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	ts := newTestServer(t, upstream.URL)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
}

// TestUpstreamUnreachable: a dead model server surfaces as a fast, honest
// 502 — never a hang or a fabricated answer.
func TestUpstreamUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	ts := newTestServer(t, deadURL)
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}
