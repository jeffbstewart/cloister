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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// presenceConfig builds a two-node config for presence tests; classes are
// irrelevant here but the schema requires one.
func presenceConfig(t *testing.T, upURL, downURL string) *RouterConfig {
	t.Helper()
	return routerConfig(t, fmt.Sprintf(`
probe:
  interval: 15s
  timeout: 3s
nodes:
  up:
    url: %s
    maxInFlight: 4
  down:
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
      - node: up
        model: coder-model:30b
`, upURL, downURL))
}

// TestPresenceStartsOptimistic: before any probe, every node counts as
// present — a wrong guess costs one dial timeout, while a pessimistic start
// would refuse working nodes for no reason.
func TestPresenceStartsOptimistic(t *testing.T) {
	tr := newPresenceTracker(presenceConfig(t, "http://up:11434", "http://down:11434"))
	if !tr.present("up") || !tr.present("down") {
		t.Error("nodes not present before the first probe, want an optimistic start")
	}
}

// TestProbeAllMarksReachability: an answering node is present, an
// unreachable one absent, and the probe asks the OpenAI surface the door
// forwards — GET /v1/models.
func TestProbeAllMarksReachability(t *testing.T) {
	var probedPath atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probedPath.Store(r.URL.Path)
		io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer up.Close()

	tr := newPresenceTracker(presenceConfig(t, up.URL, deadServerURL(t)))
	tr.probeAll(context.Background())

	if !tr.present("up") {
		t.Error("answering node marked absent")
	}
	if tr.present("down") {
		t.Error("unreachable node marked present")
	}
	if got, _ := probedPath.Load().(string); got != "/v1/models" {
		t.Errorf("probe hit %q, want /v1/models", got)
	}
}

// TestProbeRecoversAndDegradesOnStatus: a node answering non-2xx is absent —
// "port open" is not "can serve" — and flips back to present as soon as a
// probe sees a healthy answer, with no operator action.
func TestProbeRecoversAndDegradesOnStatus(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusServiceUnavailable)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer node.Close()

	tr := newPresenceTracker(presenceConfig(t, node.URL, deadServerURL(t)))
	tr.probeAll(context.Background())
	if tr.present("up") {
		t.Error("node answering 503 marked present, want absent")
	}

	status.Store(http.StatusOK)
	tr.probeAll(context.Background())
	if !tr.present("up") {
		t.Error("recovered node still absent after a healthy probe")
	}
}
