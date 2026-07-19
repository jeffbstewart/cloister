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
	"log"
	"os"
	"path/filepath"
	"time"
)

// StatusFileName is the well-known snapshot file inside the status volume;
// the volume name plus this name is the whole discovery mechanism for the
// state services' Inference panel (docs/agency.md).
const StatusFileName = "agency.json"

// statusWriteInterval paces the snapshot writer.  The snapshot is a few KB;
// writing it often keeps the panel near-live without a config knob to get
// wrong.
const statusWriteInterval = 5 * time.Second

// Snapshot is the status volume's payload: everything the operator's
// Inference panel shows.  It flows ONLY through the volume — never on the
// consumer-facing port, where machine-wide operation metadata would leak
// cross-cell activity to agents.
type Snapshot struct {
	WrittenAt time.Time              `json:"ts"`
	Nodes     map[string]NodeStatus  `json:"nodes"`
	Classes   map[string]ClassStatus `json:"classes"`
	// Ops is the last-N completed operations, oldest first.
	Ops []OpRecord `json:"ops"`
}

// NodeStatus is one node as the door last observed it.
type NodeStatus struct {
	URL               string   `json:"url"`
	Present           bool     `json:"present"`
	Pinned            []string `json:"pinned"`
	ResidencyKnown    bool     `json:"residencyKnown"`
	Resident          []string `json:"resident,omitempty"`
	MaxInFlight       int      `json:"maxInFlight"`
	InFlight          int      `json:"inFlight"`
	QueuedInteractive int      `json:"queuedInteractive"`
	QueuedBatch       int      `json:"queuedBatch"`
}

// ClassStatus is one engine class's configuration, republished so the panel
// can show routes without reading the agency's config mount.
type ClassStatus struct {
	Priority     Priority          `json:"priority"`
	Deadline     Duration          `json:"deadline"`
	MaxDeadline  Duration          `json:"maxDeadline"`
	QueueWait    Duration          `json:"queueWait"`
	MaxQueueWait Duration          `json:"maxQueueWait"`
	Chain        []ChainLinkStatus `json:"chain"`
}

// ChainLinkStatus is one (node, model) link of a class's chain.
type ChainLinkStatus struct {
	Node  string `json:"node"`
	Model string `json:"model"`
}

// snapshot collects the door's current picture: config, presence, residency,
// queue depths, and the op ledger.
func (rt *router) snapshot() Snapshot {
	snap := Snapshot{
		WrittenAt: rt.now(),
		Nodes:     make(map[string]NodeStatus, len(rt.cfg.nodes)),
		Classes:   make(map[string]ClassStatus, len(rt.cfg.classes)),
		Ops:       rt.ops.history(),
	}
	for name, node := range rt.cfg.nodes {
		inFlight, queuedInteractive, queuedBatch := rt.gates[name].stats()
		resident, known := rt.presence.residency(name)
		snap.Nodes[name] = NodeStatus{
			URL:               node.url.String(),
			Present:           rt.presence.present(name),
			Pinned:            node.models,
			ResidencyKnown:    known,
			Resident:          resident,
			MaxInFlight:       node.maxInFlight,
			InFlight:          inFlight,
			QueuedInteractive: queuedInteractive,
			QueuedBatch:       queuedBatch,
		}
	}
	for name, route := range rt.cfg.classes {
		chain := make([]ChainLinkStatus, 0, len(route.links))
		for _, link := range route.links {
			chain = append(chain, ChainLinkStatus{Node: link.node, Model: link.model})
		}
		snap.Classes[name.String()] = ClassStatus{
			Priority:     route.priority,
			Deadline:     Duration(route.deadline),
			MaxDeadline:  Duration(route.maxDeadline),
			QueueWait:    Duration(route.queueWait),
			MaxQueueWait: Duration(route.maxQueueWait),
			Chain:        chain,
		}
	}
	return snap
}

// writeSnapshot publishes one snapshot atomically: written beside the
// destination and renamed into place, so a reader never sees a torn file.
func (rt *router) writeSnapshot(dir string) error {
	data, err := json.MarshalIndent(rt.snapshot(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	tmp := filepath.Join(dir, "."+StatusFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, StatusFileName)); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	return nil
}

// WriteStatusSnapshots publishes the status snapshot into dir on an
// interval until ctx ends, starting immediately.  It is a no-op for a
// pass-through door.  Callers run it on its own goroutine beside the HTTP
// server; write failures are logged and retried on the next tick — a full
// or briefly absent volume must never take the door down.
func (s *Server) WriteStatusSnapshots(ctx context.Context, dir string) {
	if s.router == nil {
		return
	}
	write := func() {
		if err := s.router.writeSnapshot(dir); err != nil {
			log.Printf("agency: status snapshot: %v", err)
		}
	}
	write()
	ticker := time.NewTicker(statusWriteInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			write()
		}
	}
}
