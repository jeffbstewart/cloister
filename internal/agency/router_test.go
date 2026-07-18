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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// routerConfig parses a routing config or fails the test.
func routerConfig(t *testing.T, yaml string) *RouterConfig {
	t.Helper()
	cfg, err := parseRouterConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("parseRouterConfig: %v", err)
	}
	return cfg
}

// newRoutingServer builds a routing-mode agency over the given config.
func newRoutingServer(t *testing.T, yaml string) *httptest.Server {
	t.Helper()
	srv, err := New(Config{Routes: routerConfig(t, yaml)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// chatClassYAML renders a one-node, one-class config with the standard
// budgets these tests share.
func chatClassYAML(nodeURL string) string {
	return fmt.Sprintf(`
probe:
  interval: 15s
  timeout: 3s
nodes:
  infer:
    url: %s
    maxInFlight: 4
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m
    chain:
      - node: infer
        model: coder-model:30b
`, nodeURL)
}

// deadServerURL returns a URL nothing listens on: a real listener's address,
// closed before use, so a dial is refused fast.
func deadServerURL(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	return dead.URL
}

// TestRouterRoutesFirstLink: the happy path.  The class's first link gets the
// request with the model field rewritten to the link's model tag and every
// other field intact, the control headers are stripped, and the response
// comes back intact plus the node/model provenance header.
func TestRouterRoutesFirstLink(t *testing.T) {
	var got struct {
		model, host, deadlineHeader, queueWaitHeader string
		messages                                     any
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("node received unparseable body: %v", err)
		}
		got.model, _ = body["model"].(string)
		got.messages = body["messages"]
		got.host = r.Host
		got.deadlineHeader = r.Header.Get(DeadlineHeader)
		got.queueWaitHeader = r.Header.Get(QueueWaitHeader)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer node.Close()

	ts := newRoutingServer(t, chatClassYAML(node.URL))

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(DeadlineHeader, "60s")
	req.Header.Set(QueueWaitHeader, "5s")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got.model != "coder-model:30b" {
		t.Errorf("node saw model %q, want the link's model tag %q", got.model, "coder-model:30b")
	}
	if got.messages == nil {
		t.Error("node saw no messages field, want the consumer's body forwarded intact")
	}
	if wantHost := strings.TrimPrefix(node.URL, "http://"); got.host != wantHost {
		t.Errorf("node saw Host %q, want %q", got.host, wantHost)
	}
	if got.deadlineHeader != "" || got.queueWaitHeader != "" {
		t.Errorf("node saw control headers %q/%q, want both stripped", got.deadlineHeader, got.queueWaitHeader)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if served := resp.Header.Get(servedByHeader); served != "infer/coder-model:30b" {
		t.Errorf("%s = %q, want node/model provenance", servedByHeader, served)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"choices":[]}` {
		t.Errorf("body = %q, want the node's body intact", body)
	}
}

// TestRouterAdvancesPastDeadLink: an unreachable first link means the next
// link serves — and the provenance header says so.
func TestRouterAdvancesPastDeadLink(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer live.Close()

	ts := newRoutingServer(t, fmt.Sprintf(`
probe:
  interval: 15s
  timeout: 3s
nodes:
  gone:
    url: %s
    maxInFlight: 4
  backup:
    url: %s
    maxInFlight: 4
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m
    chain:
      - node: gone
        model: big-model:x
      - node: backup
        model: small-model:y
`, deadServerURL(t), live.URL))

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 from the fallback link", resp.StatusCode)
	}
	if served := resp.Header.Get(servedByHeader); served != "backup/small-model:y" {
		t.Errorf("%s = %q, want the fallback link's provenance", servedByHeader, served)
	}
}

// TestRouterAdvancesPastBusyLink: a full node whose queue budget runs out is
// an unavailable link — the chain advances instead of stalling.  The first
// request parks inside the busy node's handler (slot held); the second
// request's 5ms queue budget fires — a guaranteed event, not a sleep — and
// the fallback serves it.
func TestRouterAdvancesPastBusyLink(t *testing.T) {
	occupied := make(chan struct{})
	release := make(chan struct{})
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(occupied)
		<-release
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer busy.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer backup.Close()

	ts := newRoutingServer(t, fmt.Sprintf(`
probe:
  interval: 15s
  timeout: 3s
nodes:
  busy:
    url: %s
    maxInFlight: 1
  backup:
    url: %s
    maxInFlight: 4
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 5ms
    maxQueueWait: 1m
    chain:
      - node: busy
        model: big-model:x
      - node: backup
        model: small-model:y
`, busy.URL, backup.URL))

	firstDone := make(chan error, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"chat"}`))
		if err == nil {
			resp.Body.Close()
		}
		firstDone <- err
	}()
	<-occupied // the busy node's only slot is now held

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 from the fallback link", resp.StatusCode)
	}
	if served := resp.Header.Get(servedByHeader); served != "backup/small-model:y" {
		t.Errorf("%s = %q, want the fallback link past the busy node", servedByHeader, served)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first request: %v", err)
	}
}

// hostRecordingTransport answers 200 and records each host attempted, so a
// test can assert which links were dialed at all.
type hostRecordingTransport struct {
	hosts []string
}

func (h *hostRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.hosts = append(h.hosts, req.URL.Host)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}, nil
}

// TestRouterSkipsAbsentNode: a node the last probe marked absent is never
// dialed — the chain advances straight past it, and the node serves again
// once a probe marks it back.
func TestRouterSkipsAbsentNode(t *testing.T) {
	rec := &hostRecordingTransport{}
	rt := newRouter(routerConfig(t, `
probe:
  interval: 15s
  timeout: 3s
nodes:
  gone:
    url: http://gone-node:11434
    maxInFlight: 4
  backup:
    url: http://backup-node:11434
    maxInFlight: 4
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m
    chain:
      - node: gone
        model: big-model:x
      - node: backup
        model: small-model:y
`), rec)
	rt.presence.nodes["gone"].present.Store(false)
	ts := httptest.NewServer(rt)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 from the fallback link", resp.StatusCode)
	}
	if served := resp.Header.Get(servedByHeader); served != "backup/small-model:y" {
		t.Errorf("%s = %q, want the fallback past the absent node", servedByHeader, served)
	}
	if len(rec.hosts) != 1 || rec.hosts[0] != "backup-node:11434" {
		t.Errorf("dialed hosts = %v, want only the backup — an absent node must never be dialed", rec.hosts)
	}

	// The node returns: the next probe marks it present and it serves again.
	rt.presence.nodes["gone"].present.Store(true)
	resp, err = http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if served := resp.Header.Get(servedByHeader); served != "gone/big-model:x" {
		t.Errorf("%s = %q, want the recovered node serving again", servedByHeader, served)
	}
}

// TestRouterFreesSlotAfterResponse: a completed response hands its node slot
// back.  With maxInFlight 1 and a 5ms queue budget, a leaked slot would turn
// the second request into a fast chain-exhausted refusal.
func TestRouterFreesSlotAfterResponse(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer node.Close()

	ts := newRoutingServer(t, fmt.Sprintf(`
probe:
  interval: 15s
  timeout: 3s
nodes:
  infer:
    url: %s
    maxInFlight: 1
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 5ms
    maxQueueWait: 1m
    chain:
      - node: infer
        model: coder-model:30b
`, node.URL))

	for i := 0; i < 2; i++ {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"chat"}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (a non-200 second request means the slot leaked)", i, resp.StatusCode)
		}
	}
}

// TestRouterChainExhaustedRefusal: every link unreachable is a distinct,
// fast refusal naming the class — never a mystery stall.
func TestRouterChainExhaustedRefusal(t *testing.T) {
	ts := newRoutingServer(t, chatClassYAML(deadServerURL(t)))

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"chat"`, "no engine in the chain is reachable"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("refusal body %q does not mention %s", body, want)
		}
	}
}

// TestRouterRefusesBadRequests: malformed or unknown asks are refused before
// any node is dialed, and the unknown/missing-model refusals name the
// configured classes so the caller learns what it could have asked for.
func TestRouterRefusesBadRequests(t *testing.T) {
	var nodeCalls atomic.Int64
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeCalls.Add(1)
	}))
	defer node.Close()

	ts := newRoutingServer(t, fmt.Sprintf(`
probe:
  interval: 15s
  timeout: 3s
nodes:
  infer:
    url: %s
    maxInFlight: 4
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m
    chain:
      - node: infer
        model: coder-model:30b
  summarize-cheap:
    priority: batch
    deadline: 30s
    maxDeadline: 2m
    queueWait: 30s
    maxQueueWait: 2m
    chain:
      - node: infer
        model: tiny-model:3b
`, node.URL))

	cases := []struct {
		name       string
		body       string
		header     http.Header
		wantStatus int
		wantInBody string
	}{
		{"unknown class", `{"model":"ghost"}`, nil, http.StatusNotFound, "configured: chat, summarize-cheap"},
		{"missing model", `{"messages":[]}`, nil, http.StatusBadRequest, "configured: chat, summarize-cheap"},
		{"model not a string", `{"model":7}`, nil, http.StatusBadRequest, "JSON string"},
		{"invalid class name", `{"model":"bad name"}`, nil, http.StatusBadRequest, "invalid engine class name"},
		{"not a JSON object", `[1,2]`, nil, http.StatusBadRequest, "JSON object"},
		{"deadline over class cap", `{"model":"chat"}`,
			http.Header{DeadlineHeader: []string{"10m"}}, http.StatusBadRequest, "exceeds class"},
		{"queue wait over class cap", `{"model":"chat"}`,
			http.Header{QueueWaitHeader: []string{"10m"}}, http.StatusBadRequest, "exceeds class"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
				strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			for name, vals := range tc.header {
				req.Header[name] = vals
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tc.wantInBody) {
				t.Errorf("refusal body %q does not contain %q", body, tc.wantInBody)
			}
		})
	}
	if n := nodeCalls.Load(); n != 0 {
		t.Errorf("refused requests reached the node %d times, want 0", n)
	}
}

func TestRequestDeadline(t *testing.T) {
	route := classRoute{
		deadline:    90 * time.Second,
		maxDeadline: 5 * time.Minute,
	}
	cases := []struct {
		name    string
		header  string
		want    time.Duration
		wantErr bool
	}{
		{"absent means class default", "", 90 * time.Second, false},
		{"tighter is honored", "30s", 30 * time.Second, false},
		{"at the cap is honored", "5m", 5 * time.Minute, false},
		{"over the cap is refused", "10m", 0, true},
		{"unparseable is refused", "soon", 0, true},
		{"bare number is refused", "90", 0, true},
		{"non-positive is refused", "-5s", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requestDeadline(route, tc.header)
			if tc.wantErr {
				if err == nil {
					t.Errorf("requestDeadline(%q) succeeded, want error", tc.header)
				}
				return
			}
			if err != nil {
				t.Fatalf("requestDeadline(%q): %v", tc.header, err)
			}
			if got != tc.want {
				t.Errorf("requestDeadline(%q) = %s, want %s", tc.header, got, tc.want)
			}
		})
	}
}

// TestRequestQueueWait: the queue budget resolves against its own class
// fields — the shared header semantics are covered by TestRequestDeadline.
func TestRequestQueueWait(t *testing.T) {
	route := classRoute{
		queueWait:    10 * time.Second,
		maxQueueWait: time.Minute,
	}
	if got, err := requestQueueWait(route, ""); err != nil || got != 10*time.Second {
		t.Errorf("requestQueueWait(absent) = %s, %v; want the class default 10s", got, err)
	}
	if got, err := requestQueueWait(route, "2s"); err != nil || got != 2*time.Second {
		t.Errorf("requestQueueWait(2s) = %s, %v; want 2s", got, err)
	}
	if _, err := requestQueueWait(route, "5m"); err == nil {
		t.Error("requestQueueWait over the cap succeeded, want refusal")
	}
}

// recordingTransport is the injected round-tripper seam: it records the
// per-attempt request and answers 200 without any network.
type recordingTransport struct {
	deadline    time.Time
	hadDeadline bool
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.deadline, rt.hadDeadline = req.Context().Deadline()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}, nil
}

// TestRouterAppliesDeadline proves the resolved budget rides the outbound
// request context.  The bound is checked as an upper limit against the wall
// clock — no sleeps: the deadline either is or is not within the budget.
func TestRouterAppliesDeadline(t *testing.T) {
	rec := &recordingTransport{}
	rt := newRouter(routerConfig(t, chatClassYAML("http://infer:11434")), rec)
	ts := httptest.NewServer(rt)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(DeadlineHeader, "30s")
	before := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	after := time.Now()

	if !rec.hadDeadline {
		t.Fatal("outbound request context carries no deadline, want the caller's budget applied")
	}
	// The router stamps the deadline somewhere between before and after, so
	// the budget bounds it from both sides.
	if limit := after.Add(30 * time.Second); rec.deadline.After(limit) {
		t.Errorf("outbound deadline %s is beyond the 30s budget (limit %s)", rec.deadline, limit)
	}
	if floor := before.Add(20 * time.Second); rec.deadline.Before(floor) {
		t.Errorf("outbound deadline %s is far below the 30s budget, want roughly now+30s", rec.deadline)
	}
}

// blockedTransport parks every attempt until the request's budget fires,
// then reports the context's own error — the shape of a link that accepts
// the dial and never answers.
type blockedTransport struct{}

func (blockedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// TestRouterDeadlineExhaustedRefusal: a burned budget is its own refusal —
// 504, not the chain-exhausted 503.  The transport blocks on the context's
// Done channel, an event the budget guarantees, so the test waits on a
// certainty rather than sleeping.
func TestRouterDeadlineExhaustedRefusal(t *testing.T) {
	rt := newRouter(routerConfig(t, `
probe:
  interval: 15s
  timeout: 3s
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
classes:
  chat:
    priority: interactive
    deadline: 5ms
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m
    chain:
      - node: infer
        model: coder-model:30b
`), blockedTransport{})
	ts := httptest.NewServer(rt)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "operation deadline exhausted") {
		t.Errorf("refusal body %q does not name the exhausted deadline", body)
	}
}

// TestRouterServesModelList: GET /v1/models lists the classes — consumers
// discover classes, never what lies behind the door.
func TestRouterServesModelList(t *testing.T) {
	ts := newRoutingServer(t, `
probe:
  interval: 15s
  timeout: 3s
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m
    chain:
      - node: infer
        model: coder-model:30b
  deep-think:
    priority: batch
    deadline: 2m
    maxDeadline: 10m
    queueWait: 30s
    maxQueueWait: 5m
    chain:
      - node: infer
        model: big-moe:latest
`)
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 2 {
		t.Fatalf("got %+v, want a list of the 2 classes", list)
	}
	if list.Data[0].ID != "chat" || list.Data[1].ID != "deep-think" {
		t.Errorf("classes = %s, %s; want chat, deep-think (sorted)", list.Data[0].ID, list.Data[1].ID)
	}
}

// TestRouterStreamsChunksAsTheyArrive: the router mode keeps the door's
// token-by-token forwarding — same deadlock-on-buffering construction as the
// pass-through streaming test.
func TestRouterStreamsChunksAsTheyArrive(t *testing.T) {
	const chunk1 = "data: {\"delta\":\"one\"}\n\n"
	const chunk2 = "data: [DONE]\n\n"
	release := make(chan struct{})
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunk1)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, chunk2)
	}))
	defer node.Close()

	ts := newRoutingServer(t, chatClassYAML(node.URL))

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"chat","stream":true}`))
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
}
