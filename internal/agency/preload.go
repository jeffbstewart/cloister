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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// preloadTimeout bounds one model load.  Loads are tens of seconds from a
// warm disk and minutes for a huge MoE from a cold one; this is a backstop
// against a hung node, not a performance bound.
const preloadTimeout = 10 * time.Minute

// preloader brings cold pinned models up before anyone asks for them.  When
// a probe finds a present node whose pinned model is not resident (a reboot,
// a fresh deploy), waiting for the first real request would make that caller
// pay the whole load; instead the door sends ollama's documented
// load-only request — POST /api/generate naming the model with no prompt —
// so the model is warm when the people come calling.  Under pinned sets a
// load can never evict anything, which is what makes preloading safe to do
// unprompted.
//
// keep_alive -1 rides the request so a freshly loaded model never idle-
// unloads even before real traffic arrives; the node's own OLLAMA_KEEP_ALIVE
// must agree (docker/inference.yaml sets -1), because later real requests
// reset the timer from the server default.
type preloader struct {
	// client carries no overall timeout: each load is bounded by its own
	// context, and probe-scale timeouts would abandon (not cancel) a load
	// mid-flight.
	client *http.Client

	mu sync.Mutex
	// inFlight keys (node/model) currently loading, so a 15s probe sweep
	// does not stack duplicate loads under a load that takes minutes.  A
	// finished load — success or failure — clears its key; a failure is
	// retried on the next sweep that still finds the model cold, and keeps
	// failing loudly in the log until the operator fixes the drift.
	inFlight map[string]bool
}

func newPreloader() *preloader {
	return &preloader{
		client: &http.Client{Transport: &http.Transport{
			// No Proxy function, as everywhere in the door: nodes are
			// dialed directly.
			DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		}},
		inFlight: make(map[string]bool),
	}
}

// ensureLoaded starts a background load for each cold model that is not
// already loading, and returns immediately — the probe sweep never waits on
// a load.
func (p *preloader) ensureLoaded(node string, u *url.URL, cold []string) {
	for _, model := range cold {
		key := node + "/" + model
		if !p.begin(key) {
			continue
		}
		go func(model, key string) {
			defer p.finish(key)
			p.load(node, u, model)
		}(model, key)
	}
}

// begin claims a key; false means a load for it is already in flight.
func (p *preloader) begin(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight[key] {
		return false
	}
	p.inFlight[key] = true
	return true
}

func (p *preloader) finish(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inFlight, key)
}

// load sends one load-only request and logs the outcome.
func (p *preloader) load(node string, u *url.URL, model string) {
	log.Printf("agency: preloading %s on node %s", model, node)
	start := time.Now()
	if err := p.requestLoad(u, model); err != nil {
		log.Printf("agency: preload %s on node %s failed: %v", model, node, err)
		return
	}
	log.Printf("agency: preloaded %s on node %s in %s", model, node, time.Since(start).Round(time.Second))
}

// requestLoad performs the load-only generate call.
func (p *preloader) requestLoad(u *url.URL, model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), preloadTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"keep_alive": -1,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		u.JoinPath("api", "generate").String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}
