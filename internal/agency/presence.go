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
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
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
	// preload brings cold pinned models up as soon as a sweep finds them,
	// instead of letting the first real caller pay the load.
	preload *preloader
	nodes   map[string]*nodePresence
}

// nodePresence is one node's mutable observed state: the reachability flag
// the hot path reads, and the resident-model set the probes assert against
// the pinned config.
type nodePresence struct {
	url     *url.URL
	pinned  []string // the node's configured model set
	present atomic.Bool

	mu sync.Mutex
	// resident is the model set the last probe saw loaded (sorted);
	// meaningful only while residencyKnown.  Residency is unknown when the
	// node is absent or its /api/ps is unreachable — a non-ollama node
	// stays permanently unknown, which costs nothing: routing never reads
	// this, it exists to assert the pinned config against reality.
	resident       []string
	residencyKnown bool
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
		preload: newPreloader(),
		nodes:   make(map[string]*nodePresence, len(cfg.nodes)),
	}
	for name, node := range cfg.nodes {
		p := &nodePresence{url: node.url, pinned: node.models}
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
		if present {
			t.probeResidency(ctx, name, node)
		} else {
			// An absent node's residency is unknown, and recovery gets a
			// fresh residency line rather than trusting stale state.
			node.setResidency(nil, false)
		}
	}
}

// probeResidency asserts the node's pinned config against what its /api/ps
// says is actually resident.  Residency never steers routing (the pinned
// config already makes a bad ask unroutable); this is the drift alarm — a
// pinned model not yet loaded, or a foreign model that something other than
// the door put there — and the trigger that preloads cold pinned models so
// the first real caller finds them warm.
func (t *presenceTracker) probeResidency(ctx context.Context, name string, node *nodePresence) {
	resident, err := t.fetchResident(ctx, node.url)
	if err != nil {
		if node.setResidency(nil, false) {
			log.Printf("agency: node %s residency unknown (api/ps: %v)", name, err)
		}
		return
	}
	if node.setResidency(resident, true) {
		log.Printf("agency: node %s %s", name, describeResidency(resident, node.pinned))
	}
	// Every sweep, not just transitions: a load that failed retries as long
	// as the model stays cold (the in-flight ledger stops duplicates while
	// one is still running).
	if cold := subtract(node.pinned, resident); len(cold) > 0 {
		t.preload.ensureLoaded(name, node.url, cold)
	}
}

// setResidency records the observed resident set and reports whether it
// changed since the last observation.
func (n *nodePresence) setResidency(resident []string, known bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	changed := known != n.residencyKnown || !slices.Equal(resident, n.resident)
	n.resident, n.residencyKnown = resident, known
	return changed
}

// residency returns the last observed resident set and whether it is known.
func (t *presenceTracker) residency(node string) ([]string, bool) {
	n := t.nodes[node]
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.resident), n.residencyKnown
}

// describeResidency renders one residency observation against the pinned
// set: what is loaded, which pinned models are not (a cold start — the
// preloader is already bringing them up), and which residents are FOREIGN
// (nothing the door routes can have loaded them, so someone else reached
// the node).
func describeResidency(resident, pinned []string) string {
	msg := "resident models: " + joinOrNone(resident)
	if cold := subtract(pinned, resident); len(cold) > 0 {
		msg += "; pinned but not loaded: " + joinOrNone(cold)
	}
	if foreign := subtract(resident, pinned); len(foreign) > 0 {
		msg += "; FOREIGN, not pinned here: " + joinOrNone(foreign)
	}
	return msg
}

func joinOrNone(models []string) string {
	if len(models) == 0 {
		return "(none)"
	}
	return strings.Join(models, ", ")
}

// subtract returns the members of a not present in b, preserving a's order.
func subtract(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// fetchResident asks ollama's native /api/ps which models are loaded.  The
// consumer-facing door never exposes this path; only the probe dials it.
func (t *presenceTracker) fetchResident(ctx context.Context, u *url.URL) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.JoinPath("api", "ps").String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ps); err != nil {
		return nil, fmt.Errorf("parse api/ps: %w", err)
	}
	resident := make([]string, 0, len(ps.Models))
	for _, m := range ps.Models {
		resident = append(resident, m.Name)
	}
	slices.Sort(resident)
	return resident, nil
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
