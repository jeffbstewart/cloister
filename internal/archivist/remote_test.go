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

package archivist

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jeffbstewart/cloister/internal/audit"
)

// fakeAuditor collects remote-op records.
type fakeAuditor struct {
	mu   sync.Mutex
	recs []audit.Record
}

func (f *fakeAuditor) Append(r audit.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
	return nil
}

func (f *fakeAuditor) records() []audit.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audit.Record(nil), f.recs...)
}

// TestPublishWithoutEndpointsIsAnAuditedRefusal: local-only mode has
// no remote route, and the refusal still leaves a ledger line.
func TestPublishWithoutEndpointsIsAnAuditedRefusal(t *testing.T) {
	aud := &fakeAuditor{}
	f := newFixtureWith(t, Config{Version: "test", Audit: aud})
	f.ok(t, "start_work", map[string]any{"name": "agent/unrouted"})

	text, isErr := f.call(t, "publish", nil)
	if !isErr || !strings.Contains(text, "no endpoint table") {
		t.Fatalf("publish = %q (err=%v), want the no-endpoints refusal", text, isErr)
	}
	recs := aud.records()
	if len(recs) != 1 || recs[0].Tool != "publish" || recs[0].Decision != audit.DecisionRemoteRefused {
		t.Errorf("audit = %+v, want one publish remote_refused record", recs)
	}
	if d := recs[0].Remote(); d == nil {
		t.Errorf("detail = %+v, want a RemoteDetail", recs[0].Detail)
	}
}

// TestForgeToolsAbsentWithoutClient: a local-only instance has no PR
// verbs to misuse — the tools are not registered at all.
func TestForgeToolsAbsentWithoutClient(t *testing.T) {
	f := newFixtureWith(t, Config{Version: "test"})
	res, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	if !names["publish"] {
		t.Error("publish must register even in local-only mode (it refuses cleanly)")
	}
	for _, absent := range []string{"propose", "check_progress", "read_reviews", "reply_to_review"} {
		if names[absent] {
			t.Errorf("%s registered without a forge client", absent)
		}
	}
}

// TestAbandonRemoteHalfNoPhantomAudit: deleteRemote on a branch that
// was never published touches no endpoint, so it leaves NO remote
// record — and the local half is unaudited either way.  The audit trail
// records only operations that actually reached the endpoint.
func TestAbandonRemoteHalfNoPhantomAudit(t *testing.T) {
	aud := &fakeAuditor{}
	f := newFixtureWith(t, Config{Version: "test", Audit: aud})

	f.ok(t, "start_work", map[string]any{"name": "agent/doomed"})
	f.ok(t, "abandon_work", map[string]any{"name": "agent/doomed", "deleteRemote": true})
	if n := len(aud.records()); n != 0 {
		t.Errorf("never-published deleteRemote wrote %d records; nothing touched the endpoint", n)
	}

	f.ok(t, "start_work", map[string]any{"name": "agent/local-doomed"})
	f.ok(t, "abandon_work", map[string]any{"name": "agent/local-doomed"})
	if n := len(aud.records()); n != 0 {
		t.Errorf("local abandon wrote %d records; local verbs are unaudited", n)
	}
}
