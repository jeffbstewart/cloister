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
	"slices"
	"sync"
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
    models:
      - coder-model:30b
  down:
    url: %s
    maxInFlight: 4
    models:
      - coder-model:30b
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

// TestProbeAllMarksReachability: an answering node is present (asked on the
// OpenAI surface the door forwards, GET /v1/models), an unreachable one is
// absent with unknown residency, and the present node's /api/ps answer
// becomes its recorded resident set.
func TestProbeAllMarksReachability(t *testing.T) {
	var mu sync.Mutex
	var probedPaths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		probedPaths = append(probedPaths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[]}`)
		case "/api/ps":
			io.WriteString(w, `{"models":[{"name":"coder-model:30b"}]}`)
		default:
			http.NotFound(w, r)
		}
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
	mu.Lock()
	paths := slices.Clone(probedPaths)
	mu.Unlock()
	if !slices.Equal(paths, []string{"/v1/models", "/api/ps"}) {
		t.Errorf("probe hit %v, want the health check then the residency check", paths)
	}
	resident, known := tr.residency("up")
	if !known || !slices.Equal(resident, []string{"coder-model:30b"}) {
		t.Errorf("up residency = %v (known %t), want the /api/ps answer", resident, known)
	}
	if _, known := tr.residency("down"); known {
		t.Error("absent node's residency is known, want unknown")
	}
}

// TestProbeAssertsResidencyDrift: the recorded resident set follows what
// /api/ps reports — a foreign resident and a missing pinned model both show
// up, and a later probe sees the correction.
func TestProbeAssertsResidencyDrift(t *testing.T) {
	var psBody atomic.Value
	psBody.Store(`{"models":[{"name":"stray:7b"}]}`)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			io.WriteString(w, psBody.Load().(string))
			return
		}
		io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer node.Close()

	tr := newPresenceTracker(presenceConfig(t, node.URL, deadServerURL(t)))
	tr.probeAll(context.Background())
	resident, known := tr.residency("up")
	if !known || !slices.Equal(resident, []string{"stray:7b"}) {
		t.Errorf("residency = %v (known %t), want the drifted set [stray:7b]", resident, known)
	}

	psBody.Store(`{"models":[{"name":"coder-model:30b"}]}`)
	tr.probeAll(context.Background())
	if resident, _ := tr.residency("up"); !slices.Equal(resident, []string{"coder-model:30b"}) {
		t.Errorf("residency = %v after correction, want [coder-model:30b]", resident)
	}
}

// TestResidencyUnknownWhenPsFails: a node whose /api/ps is unavailable (a
// non-ollama engine, say) stays PRESENT with unknown residency — the drift
// alarm goes quiet, routing is untouched.
func TestResidencyUnknownWhenPsFails(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer node.Close()

	tr := newPresenceTracker(presenceConfig(t, node.URL, deadServerURL(t)))
	tr.probeAll(context.Background())
	if !tr.present("up") {
		t.Error("node with no /api/ps marked absent, want present")
	}
	if _, known := tr.residency("up"); known {
		t.Error("residency known despite /api/ps failing, want unknown")
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
