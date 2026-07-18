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
	"testing"
)

// granted reports whether the waiter's slot has been handed over, without
// blocking.
func granted(w *waiter) bool {
	select {
	case <-w.grant:
		return true
	default:
		return false
	}
}

// TestGateGrantsUpToCapacity: free slots are granted immediately; the next
// asker waits.  The enqueue/release seam keeps the test single-threaded —
// order is asserted on data, never on goroutine timing.
func TestGateGrantsUpToCapacity(t *testing.T) {
	g := newPriorityGate(2)
	for i := 0; i < 2; i++ {
		if w := g.enqueue(PriorityInteractive); w != nil {
			t.Fatalf("enqueue %d queued, want an immediate grant", i)
		}
	}
	if w := g.enqueue(PriorityInteractive); w == nil {
		t.Fatal("enqueue beyond capacity granted immediately, want it queued")
	}
}

// TestGateGrantsInteractiveAheadOfBatch: with the gate full, a batch waiter
// that queued FIRST still yields to a later interactive waiter — and batch
// drains once no interactive waiter remains.
func TestGateGrantsInteractiveAheadOfBatch(t *testing.T) {
	g := newPriorityGate(1)
	if w := g.enqueue(PriorityInteractive); w != nil {
		t.Fatal("first enqueue queued, want an immediate grant")
	}
	batch := g.enqueue(PriorityBatch)
	interactive := g.enqueue(PriorityInteractive)
	if batch == nil || interactive == nil {
		t.Fatal("waiters on a full gate were granted immediately, want them queued")
	}

	g.release()
	if !granted(interactive) {
		t.Error("interactive waiter not granted on release, want it served first")
	}
	if granted(batch) {
		t.Error("batch waiter granted ahead of interactive")
	}

	g.release()
	if !granted(batch) {
		t.Error("batch waiter not granted once interactive drained")
	}
}

// TestGateFIFOWithinPriority: two waiters of the same priority are served in
// arrival order.
func TestGateFIFOWithinPriority(t *testing.T) {
	g := newPriorityGate(1)
	g.enqueue(PriorityBatch)
	first := g.enqueue(PriorityBatch)
	second := g.enqueue(PriorityBatch)

	g.release()
	if !granted(first) || granted(second) {
		t.Error("same-priority waiters served out of arrival order")
	}
}

// TestGateAcquireWithdrawsOnContextEnd: a waiter whose budget ends leaves the
// line, and the slot it never got is not lost.
func TestGateAcquireWithdrawsOnContextEnd(t *testing.T) {
	g := newPriorityGate(1)
	g.enqueue(PriorityInteractive) // occupy the only slot

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.acquire(ctx, PriorityBatch); err == nil {
		t.Fatal("acquire on a full gate with an ended context succeeded, want error")
	}

	// The withdrawn waiter must not swallow the released slot.
	g.release()
	if w := g.enqueue(PriorityInteractive); w != nil {
		t.Error("slot lost after a withdrawn waiter, want an immediate grant")
	}
}

// TestGateAcquireImmediateIgnoresEndedContext: a free slot is a grant — the
// context only governs waiting.
func TestGateAcquireImmediateIgnoresEndedContext(t *testing.T) {
	g := newPriorityGate(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.acquire(ctx, PriorityInteractive); err != nil {
		t.Fatalf("acquire with a free slot = %v, want immediate grant", err)
	}
}
