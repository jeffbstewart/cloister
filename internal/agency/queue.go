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
	"slices"
	"sync"
)

// priorityGate is a node's admission gate: maxInFlight slots, and when none
// is free, waiters queue in two FIFO lines with interactive granted ahead of
// batch.  Batch never starves forever — it drains whenever no interactive
// waiter is present.  There is no preemption: a granted slot runs to
// completion (docs/agency.md records the analysis).
type priorityGate struct {
	mu       sync.Mutex
	capacity int // the configured maxInFlight, fixed
	free     int
	// lines are the FIFO wait lines indexed by priority rank, interactive
	// first.  release always serves the lowest non-empty rank.
	lines [2][]*waiter
}

// waiter is one queued acquire; grant is closed when a slot is handed over.
type waiter struct {
	grant chan struct{}
}

func newPriorityGate(capacity int) *priorityGate {
	return &priorityGate{capacity: capacity, free: capacity}
}

// stats reports the gate's current occupancy and wait-line depths, for the
// status snapshot.
func (g *priorityGate) stats() (inFlight, queuedInteractive, queuedBatch int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.capacity - g.free, len(g.lines[0]), len(g.lines[1])
}

// rank maps a priority to its wait-line index.  Config validation admits only
// the two known priorities, so anything not interactive is batch.
func rank(p Priority) int {
	if p == PriorityInteractive {
		return 0
	}
	return 1
}

// acquire takes a slot, waiting in priority order until one frees or ctx
// ends.  On a context error the waiter withdraws from its line; a grant that
// raced the withdrawal is handed back, never leaked.
func (g *priorityGate) acquire(ctx context.Context, p Priority) error {
	w := g.enqueue(p)
	if w == nil {
		return nil
	}
	select {
	case <-w.grant:
		return nil
	case <-ctx.Done():
	}
	g.mu.Lock()
	r := rank(p)
	if i := slices.Index(g.lines[r], w); i >= 0 {
		g.lines[r] = slices.Delete(g.lines[r], i, i+1)
		g.mu.Unlock()
		return ctx.Err()
	}
	g.mu.Unlock()
	// Not in the line means release already granted our slot: hand it back.
	g.release()
	return ctx.Err()
}

// enqueue claims a free slot immediately (returning nil) or joins the
// priority's wait line and returns the waiter to block on.
func (g *priorityGate) enqueue(p Priority) *waiter {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.free > 0 {
		g.free--
		return nil
	}
	w := &waiter{grant: make(chan struct{})}
	r := rank(p)
	g.lines[r] = append(g.lines[r], w)
	return w
}

// release frees a slot: the first interactive waiter gets it, else the first
// batch waiter, else it returns to the pool.
func (g *priorityGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for r := range g.lines {
		if len(g.lines[r]) > 0 {
			w := g.lines[r][0]
			g.lines[r] = slices.Delete(g.lines[r], 0, 1)
			close(w.grant)
			return
		}
	}
	g.free++
}
