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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestPreloadRequestShape: the load is ollama's documented load-only form —
// POST /api/generate naming the model with NO prompt, keep_alive -1 so the
// fresh load never idle-unloads before real traffic arrives.
func TestPreloadRequestShape(t *testing.T) {
	var got struct {
		method, path string
		body         map[string]any
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got.body); err != nil {
			t.Errorf("node received unparseable body: %v", err)
		}
		io.WriteString(w, `{"done":true,"done_reason":"load"}`)
	}))
	defer node.Close()
	u, err := url.Parse(node.URL)
	if err != nil {
		t.Fatal(err)
	}

	if err := newPreloader().requestLoad(u, "coder-model:30b"); err != nil {
		t.Fatalf("requestLoad: %v", err)
	}
	if got.method != http.MethodPost || got.path != "/api/generate" {
		t.Errorf("node saw %s %s, want POST /api/generate", got.method, got.path)
	}
	if model, _ := got.body["model"].(string); model != "coder-model:30b" {
		t.Errorf("load named model %q, want coder-model:30b", model)
	}
	if keepAlive, _ := got.body["keep_alive"].(float64); keepAlive != -1 {
		t.Errorf("keep_alive = %v, want -1 (never idle-unload)", got.body["keep_alive"])
	}
	if _, hasPrompt := got.body["prompt"]; hasPrompt {
		t.Error("load carries a prompt, want the load-only form (no tokens generated)")
	}
}

// TestPreloadDedupes: one load per (node, model) at a time — a claim holds
// until finished, then the key is claimable again (the retry path).
func TestPreloadDedupes(t *testing.T) {
	p := newPreloader()
	const key = "infer/coder-model:30b"
	if !p.begin(key) {
		t.Fatal("first claim refused, want it granted")
	}
	if p.begin(key) {
		t.Error("second claim granted while the first is in flight")
	}
	p.finish(key)
	if !p.begin(key) {
		t.Error("claim refused after finish, want the key claimable again (retry)")
	}
}

// TestProbePreloadsColdPinnedModel: a sweep that finds a present node with a
// pinned model not resident kicks off the load — the test blocks on the
// node's own receipt of it, a guaranteed event, not a sleep.
func TestProbePreloadsColdPinnedModel(t *testing.T) {
	loaded := make(chan string, 1)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[]}`)
		case "/api/ps":
			io.WriteString(w, `{"models":[]}`)
		case "/api/generate":
			var body struct {
				Model string `json:"model"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			loaded <- body.Model
			io.WriteString(w, `{"done":true,"done_reason":"load"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer node.Close()

	tr := newPresenceTracker(presenceConfig(t, node.URL, deadServerURL(t)))
	tr.probeAll(context.Background())

	if model := <-loaded; model != "coder-model:30b" {
		t.Errorf("preload asked for %q, want the cold pinned model coder-model:30b", model)
	}
}
