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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newDoorServer builds a Server (the full door, /healthz included) routing
// the standard chat class to nodeURL.
func newDoorServer(t *testing.T, nodeURL string) *httptest.Server {
	t.Helper()
	srv, err := New(Config{Routes: routerConfig(t, chatClassYAML(nodeURL))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestNewRequiresRoutes: the door has exactly one mode; building it without
// a routing table fails closed.
func TestNewRequiresRoutes(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New without Routes succeeded, want error")
	}
}

// TestRefusesOutsideV1 is the containment check: the raw model-server API
// (ollama's native endpoints — pulls, deletes, tags) does not pass the door,
// and a refused request never touches any node at all.
func TestRefusesOutsideV1(t *testing.T) {
	var nodeCalls atomic.Int64
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeCalls.Add(1)
	}))
	defer node.Close()

	ts := newDoorServer(t, node.URL)
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
	if n := nodeCalls.Load(); n != 0 {
		t.Errorf("refused paths reached the node %d times, want 0", n)
	}
}

func TestHealthz(t *testing.T) {
	ts := newDoorServer(t, "http://infer:11434")
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
}
