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

package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeffbstewart/cloister/internal/endpoint"
)

// testTable builds a one-entry github table whose credential file
// exists, for exercising the allowlist against the local rigs (whose
// path remotes the table must refuse).
func testTable(t *testing.T) *endpoint.Table {
	t.Helper()
	cred := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(cred, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `endpoints:
  - name: github.com
    canonical: https://github.com/
    wire: https://github.com/
    forge: github
    api: https://api.github.com/
    credentialFile: ` + filepath.ToSlash(cred) + `
    bot:
      name: cloister-bot
      email: bot@cloister.test
`
	p := filepath.Join(t.TempDir(), "endpoints.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	tab, err := endpoint.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func TestPublishRefusesWithoutTable(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/unwired")
	if _, err := r.a.Publish(context.Background()); !errors.Is(err, ErrNoEndpoints) {
		t.Errorf("publish without an endpoint table = %v, want ErrNoEndpoints", err)
	}
}

func TestPublishRefusesDefaultBranch(t *testing.T) {
	r := newRig(t)
	if _, err := r.a.Publish(context.Background()); !errors.Is(err, ErrDefaultBranch) {
		t.Errorf("publish on the default branch = %v, want ErrDefaultBranch (before any endpoint concern)", err)
	}
}

// TestOpenRefusesRemoteOutsideTable: the table is the allowlist, and
// the instance is bound at startup — a workspace whose origin is a
// filesystem path (or any URL outside the table) refuses to open.
func TestOpenRefusesRemoteOutsideTable(t *testing.T) {
	r := newRig(t)
	_, err := New(r.dir, Identity{}, WithClock(r.clock.Now), WithEndpoints(testTable(t)))
	if err == nil || !strings.Contains(err.Error(), "matches no endpoint") {
		t.Errorf("New with a path remote and a table = %v, want the allowlist refusal", err)
	}
}

func TestAbandonRemoteHalfNeedsTheTable(t *testing.T) {
	r := newRig(t)
	name := r.startWork("agent/published-doomed")
	r.write("a.txt", "content\n")
	r.checkpoint("work")
	r.publish("agent/published-doomed")

	err := r.a.AbandonWork(context.Background(), name, true)
	if err == nil || !strings.Contains(err.Error(), "published counterpart remains") {
		t.Errorf("deleteRemote without a table = %v; the local half is done but the remote failure must be loud", err)
	}
	if out := r.git(r.dir, "branch", "--list", "agent/published-doomed"); out != "" {
		t.Errorf("local branch survived: %q", out)
	}
}

func TestAbandonDeleteRemoteUnpublishedIsLocalOnly(t *testing.T) {
	r := newRig(t)
	name := r.startWork("agent/never-published")
	if err := r.a.AbandonWork(context.Background(), name, true); err != nil {
		t.Errorf("deleteRemote on a never-published branch = %v; nothing to delete is not an error", err)
	}
}

func TestAuthEnvNeverTouchesArgv(t *testing.T) {
	env := authEnv("tok123")
	if len(env) != 3 || env[0] != "GIT_CONFIG_COUNT=1" || env[1] != "GIT_CONFIG_KEY_0=http.extraheader" {
		t.Fatalf("authEnv = %v", env)
	}
	if !strings.HasPrefix(env[2], "GIT_CONFIG_VALUE_0=Authorization: Basic ") {
		t.Errorf("credential form = %q, want a Basic authorization header value", env[2])
	}
	if strings.Contains(strings.Join(env, " "), "tok123") {
		t.Error("the raw token appears unencoded in the environment value")
	}
}
