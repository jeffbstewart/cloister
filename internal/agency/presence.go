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
	"log"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// presenceTracker holds each node's last observed reachability.  Sometimes-
// there nodes (a laptop, a desktop that sleeps for gaming) are probed on an
// interval; the chain skips a node marked absent instead of paying a dial
// timeout per request, and picks it back up at the next probe after it
// returns — no operator action.  Probing is DETECTION ONLY: a plain HTTP GET
// that a sleeping machine never sees; the agency must never wake a node
// (docs/agency.md).
//
// Presence is an optimization, not the correctness story: a node marked
// present that just died still fails its dial and the chain advances; a node
// marked absent that just woke serves again within one probe interval.
type presenceTracker struct {
	client *http.Client
	nodes  map[string]*nodePresence
}

// nodePresence is one node's mutable reachability flag.
type nodePresence struct {
	url     *url.URL
	present atomic.Bool
}

// newPresenceTracker builds the tracker over the configured nodes.  Every
// node starts optimistically PRESENT: until the first probe reports, a wrong
// guess costs one dial timeout — exactly the no-presence behavior — while a
// pessimistic start would refuse working nodes for no reason.
func newPresenceTracker(cfg *RouterConfig) *presenceTracker {
	t := &presenceTracker{
		// The client's timeout is the whole probe bound — dial included —
		// so a blackholed SYN to a sleeping node cannot stall the sweep.
		client: &http.Client{
			Timeout: cfg.probeTimeout,
			Transport: &http.Transport{
				// No Proxy function, as with the forwarding transport: the
				// door dials its configured nodes directly.
				DialContext: (&net.Dialer{Timeout: cfg.probeTimeout}).DialContext,
			},
		},
		nodes: make(map[string]*nodePresence, len(cfg.nodes)),
	}
	for name, node := range cfg.nodes {
		p := &nodePresence{url: node.url}
		p.present.Store(true)
		t.nodes[name] = p
	}
	return t
}

// present reports the node's reachability as of the last probe.
func (t *presenceTracker) present(node string) bool {
	return t.nodes[node].present.Load()
}

// probeAll sweeps every node once, sequentially — a handful of nodes, each
// bounded by the probe timeout, keeps the sweep well inside the interval.
// Transitions (only) are logged, so a long absence is one line, not a
// heartbeat.
func (t *presenceTracker) probeAll(ctx context.Context) {
	for name, node := range t.nodes {
		present, err := t.probe(ctx, node.url)
		if node.present.Swap(present) != present {
			if present {
				log.Printf("agency: node %s is present", name)
			} else {
				log.Printf("agency: node %s is absent (probe: %v)", name, err)
			}
		}
	}
}

// probe asks one node for its model list — the same OpenAI-compatible
// surface the door forwards, so "present" means "can serve", not just "port
// open".  Any transport error or non-2xx answer is absence.
func (t *presenceTracker) probe(ctx context.Context, u *url.URL) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.JoinPath("v1", "models").String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return false, err
	}
	// Drain a bounded slice of the body so the connection can be reused.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("status %s", resp.Status)
	}
	return true, nil
}

// run sweeps immediately, then on every interval tick until ctx ends.
func (t *presenceTracker) run(ctx context.Context, interval time.Duration) {
	t.probeAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.probeAll(ctx)
		}
	}
}
