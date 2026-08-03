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
    apiRelay: github-api-relay:443
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
	_, err := New(r.dir, WithClock(r.clock.Now), WithEndpoints(testTable(t)))
	if err == nil || !errors.Is(err, endpoint.ErrNotAllowed) {
		t.Errorf("New with a path remote and a table = %v, want the allowlist refusal", err)
	}
}

// TestDeleteRemoteBranchNeedsTheTable: with no endpoint table there is
// no remote route, so even a published branch's counterpart cannot be
// deleted — the refusal comes before any endpoint touch (deleted=false).
func TestDeleteRemoteBranchNeedsTheTable(t *testing.T) {
	r := newRig(t)
	name := r.startWork("agent/published-doomed")
	r.write("a.txt", "content\n")
	r.checkpoint("work")
	r.publish("agent/published-doomed")

	deleted, err := r.a.DeleteRemoteBranch(context.Background(), name)
	if deleted || !errors.Is(err, ErrNoEndpoints) {
		t.Errorf("DeleteRemoteBranch without a table = (%v, %v), want (false, ErrNoEndpoints)", deleted, err)
	}
}

func TestDeleteRemoteBranchUnpublishedIsNoTouch(t *testing.T) {
	r := newRig(t)
	name := r.startWork("agent/never-published")
	deleted, err := r.a.DeleteRemoteBranch(context.Background(), name)
	if deleted || err != nil {
		t.Errorf("DeleteRemoteBranch on a never-published branch = (%v, %v); nothing at the endpoint is not a touch and not an error", deleted, err)
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

// TestWouldAdvanceDistinguishesRealPushFromNoOp is the answer to "did my
// work actually reach the endpoint".  git push exits 0 for "Everything
// up-to-date", so without this the agent — which cannot see the forge —
// gets the same word whether its checkpoints travelled or not.  That is
// how a pull request ends up missing a revision the agent believes it
// published.
func TestWouldAdvanceDistinguishesRealPushFromNoOp(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.startWork("agent/advance")

	// Never published: the push would create the branch.
	if adv, err := r.a.wouldAdvance(ctx, "agent/advance"); err != nil || !adv {
		t.Fatalf("unpublished branch: wouldAdvance = %v, %v; want true", adv, err)
	}

	r.write("a.txt", "one\n")
	r.checkpoint("first")
	r.publish("agent/advance")

	// Everything local is at the origin: a push now moves nothing.
	if adv, err := r.a.wouldAdvance(ctx, "agent/advance"); err != nil || adv {
		t.Errorf("up-to-date branch: wouldAdvance = %v, %v; want false", adv, err)
	}

	// One more checkpoint and there is something to carry again.
	r.write("a.txt", "two\n")
	r.checkpoint("second")
	if adv, err := r.a.wouldAdvance(ctx, "agent/advance"); err != nil || !adv {
		t.Errorf("branch ahead: wouldAdvance = %v, %v; want true", adv, err)
	}
}

// TestWouldAdvanceWhenTheEndpointLostTheBranch: an upstream can be
// configured and gone (deleted at the forge).  Pushing then recreates
// it, which is motion — treating a missing tracking ref as "nothing to
// do" would report success for a branch that is not there.
func TestWouldAdvanceWhenTheEndpointLostTheBranch(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.startWork("agent/vanished")
	r.write("a.txt", "one\n")
	r.checkpoint("first")
	r.publish("agent/vanished")

	// Delete it at the origin, and drop the stale tracking ref the way a
	// pruning fetch would.
	r.git(r.origin, "update-ref", "-d", "refs/heads/agent/vanished")
	r.git(r.dir, "update-ref", "-d", "refs/remotes/origin/agent/vanished")

	if adv, err := r.a.wouldAdvance(ctx, "agent/vanished"); err != nil || !adv {
		t.Errorf("branch missing at the endpoint: wouldAdvance = %v, %v; want true", adv, err)
	}
}
