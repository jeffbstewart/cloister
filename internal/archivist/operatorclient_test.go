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
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/jeffbstewart/cloister/internal/operator"
)

// The two halves of the operator surface are written in different
// packages — the verbs here, the typed client in internal/operator —
// and they agree only by JSON field names.  Nothing in either package's
// own tests would notice `provisioned_at` becoming `provisionedAt`, so
// this drives the REAL archivist through the REAL client over HTTP and
// asserts the values arrive populated.  It is the contract test for the
// whole session-manager path.

func TestOperatorClientAgainstTheRealSurface(t *testing.T) {
	f, _, _ := newProvisionFixture(t)
	ts := httptest.NewServer(f.srv.Handler())
	defer ts.Close()
	ctx := context.Background()

	c, err := operator.Dial(ctx, operator.Config{
		URL: ts.URL + OperatorPath, Name: "workbench-test", Version: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != operator.StateEmpty {
		t.Fatalf("fresh workspace = %+v, want empty", st)
	}

	res, err := c.Provision(ctx, provisionURL, "agent/wire")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Repo != "op/repo" || res.Branch != "agent/wire" || res.Endpoint == "" {
		t.Errorf("provision result = %+v, want every field populated", res)
	}

	st, err = c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != operator.StateProvisioned || st.Repo != "op/repo" ||
		st.Branch != "agent/wire" || st.ProvisionedAt == 0 {
		t.Fatalf("status after provision = %+v, want fully populated provenance", st)
	}
	if got := st.Provisioned(); got != "op/repo on agent/wire" {
		t.Errorf("display line = %q", got)
	}

	// A refusal crosses as a typed refusal carrying the archivist's own
	// words — a second provision onto a live workspace.
	if _, err := c.Provision(ctx, provisionURL, "agent/again"); err == nil {
		t.Fatal("provision onto a live workspace succeeded")
	} else {
		var ref *operator.RefusedError
		if !errors.As(err, &ref) {
			t.Errorf("re-provision = %v (%T), want a *RefusedError", err, err)
		}
	}

	dis, err := c.Dispose(ctx, false)
	if err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if !dis.Disposed || dis.Repo != "op/repo" {
		t.Errorf("dispose result = %+v", dis)
	}
	if st, err := c.Status(ctx); err != nil || st.State != operator.StateEmpty {
		t.Errorf("status after dispose = %+v, %v; want empty", st, err)
	}
}

// TestAgentSurfaceRefusesTheOperatorClient: the same client pointed at
// the agent path finds nothing to call.  This is the split's property
// asserted from the outside, over HTTP, rather than through the
// in-memory transport.
func TestAgentSurfaceRefusesTheOperatorClient(t *testing.T) {
	f, _, _ := newProvisionFixture(t)
	ts := httptest.NewServer(f.srv.Handler())
	defer ts.Close()
	ctx := context.Background()

	c, err := operator.Dial(ctx, operator.Config{URL: ts.URL + AgentPath, Name: "misaimed", Version: "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Status(ctx); err == nil {
		t.Error("workspace_state resolved on the agent surface")
	}
	if _, err := c.Provision(ctx, provisionURL, ""); err == nil {
		t.Error("provision resolved on the agent surface")
	}
}
