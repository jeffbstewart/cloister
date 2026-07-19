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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// settle spins — Gosched, never a sleep — until the handler goroutine has
// finished its bookkeeping (wantOps ops recorded, no slot held), so
// assertions read the settled state instead of racing the deferred record.
// A regression here fails on the test timeout, not by flaking.
func settle(t *testing.T, rt *router, wantOps int) {
	t.Helper()
	for {
		inFlight, _, _ := rt.gates["infer"].stats()
		if len(rt.ops.history()) >= wantOps && inFlight == 0 {
			return
		}
		runtime.Gosched()
	}
}

// snapshotRouter builds a router over the standard chat config with an
// injected clock and a canned transport, and sends one request through it so
// the op ledger has an entry.
func snapshotRouter(t *testing.T) (*router, time.Time) {
	t.Helper()
	fixed := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	rt := newRouter(routerConfig(t, chatClassYAML("http://infer:11434")), &recordingTransport{})
	rt.now = func() time.Time { return fixed }

	ts := httptest.NewServer(rt)
	defer ts.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(CallerHeader, "librarian")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	settle(t, rt, 1)
	return rt, fixed
}

func TestRouterSnapshot(t *testing.T) {
	rt, fixed := snapshotRouter(t)
	snap := rt.snapshot()

	if !snap.WrittenAt.Equal(fixed) {
		t.Errorf("WrittenAt = %s, want the injected clock %s", snap.WrittenAt, fixed)
	}

	node, ok := snap.Nodes["infer"]
	if !ok {
		t.Fatal("snapshot has no infer node")
	}
	if node.URL != "http://infer:11434" || !node.Present || node.MaxInFlight != 4 {
		t.Errorf("infer node = %+v, want present, url, maxInFlight from config", node)
	}
	if !slices.Equal(node.Pinned, []string{"coder-model:30b"}) {
		t.Errorf("pinned = %v, want the configured set", node.Pinned)
	}
	if node.InFlight != 0 || node.QueuedInteractive != 0 || node.QueuedBatch != 0 {
		t.Errorf("idle node reports occupancy %d/%d/%d, want zeros", node.InFlight, node.QueuedInteractive, node.QueuedBatch)
	}

	class, ok := snap.Classes["chat"]
	if !ok {
		t.Fatal("snapshot has no chat class")
	}
	if class.Priority != PriorityInteractive || class.Deadline.Std() != 90*time.Second {
		t.Errorf("chat class = %+v, want config republished", class)
	}
	if len(class.Chain) != 1 || class.Chain[0] != (ChainLinkStatus{Node: "infer", Model: "coder-model:30b"}) {
		t.Errorf("chat chain = %v, want the configured link", class.Chain)
	}

	if len(snap.Ops) != 1 {
		t.Fatalf("ops = %d entries, want the one completed request", len(snap.Ops))
	}
	op := snap.Ops[0]
	if op.Caller != "librarian" || op.Class != "chat" || op.ServedBy != "infer/coder-model:30b" || op.Status != http.StatusOK {
		t.Errorf("op = %+v, want caller/class/servedBy/status recorded", op)
	}
	if !op.FinishedAt.Equal(fixed) {
		t.Errorf("op FinishedAt = %s, want the injected clock", op.FinishedAt)
	}
}

// TestSnapshotRecordsRefusals: a refused ask still lands in the op ledger —
// the panel must show what was refused, not only what was served.
func TestSnapshotRecordsRefusals(t *testing.T) {
	rt := newRouter(routerConfig(t, chatClassYAML("http://infer:11434")), &recordingTransport{})
	ts := httptest.NewServer(rt)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"ghost"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	settle(t, rt, 1)

	ops := rt.ops.history()
	if len(ops) != 1 {
		t.Fatalf("ops = %d entries, want the refusal recorded", len(ops))
	}
	op := ops[0]
	if op.Class != "ghost" || op.Status != http.StatusNotFound || op.ServedBy != "" {
		t.Errorf("op = %+v, want the refused class, 404, and no servedBy", op)
	}
}

func TestWriteSnapshotAtomically(t *testing.T) {
	rt, _ := snapshotRouter(t)
	dir := t.TempDir()

	// Twice: the second write must replace the first via rename, the shape
	// a reader can never observe torn.
	for i := 0; i < 2; i++ {
		if err := rt.writeSnapshot(dir); err != nil {
			t.Fatalf("writeSnapshot %d: %v", i, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, StatusFileName))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}
	if len(snap.Nodes) != 1 || len(snap.Classes) != 1 {
		t.Errorf("snapshot = %d nodes, %d classes; want 1 and 1", len(snap.Nodes), len(snap.Classes))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != StatusFileName {
			t.Errorf("leftover file %q in the status dir, want only %s", e.Name(), StatusFileName)
		}
	}
}
