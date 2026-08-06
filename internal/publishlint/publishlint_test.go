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

package publishlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../.."

// TestCommittedWorkflowsPublishNothingMarked runs against the real files, so
// a commit that reintroduces the publish step fails the suite rather than
// the registry.
func TestCommittedWorkflowsPublishNothingMarked(t *testing.T) {
	v, err := CheckDir(repoRoot, filepath.Join(repoRoot, ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("a committed workflow would publish an unpublishable image:\n  - %s",
			strings.Join(v, "\n  - "))
	}
}

// TestTheMarkerIsActuallyThere: every check below is conditional on the
// marker existing.  If it were ever deleted, all of them would pass while
// enforcing nothing — the exact silent-no-op this lint replaced.
func TestTheMarkerIsActuallyThere(t *testing.T) {
	marked, err := MarkedContexts(repoRoot, "docker")
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) == 0 {
		t.Fatal("no build context carries the marker — this lint is guarding nothing")
	}
	if !contains(marked, "docker/workbench-claude") {
		t.Errorf("docker/workbench-claude is not marked; got %v — it installs a proprietary CLI and must never be pushed", marked)
	}
}

// TestCatchesTheStepThatActuallyShipped reconstructs the real step, verbatim
// in shape, that published a proprietary CLI to a world-readable package.
// If this test ever passes clean, the regression is back.
func TestCatchesTheStepThatActuallyShipped(t *testing.T) {
	const wf = `
jobs:
  publish:
    steps:
      - name: Build and push the claude workbench variant
        uses: docker/build-push-action@v6
        with:
          context: docker/workbench-claude
          push: true
          platforms: linux/amd64
          tags: ghcr.io/jeffbstewart/cloister-workbench-claude:2.1.222-sha-abc
`
	v, err := Check(repoRoot, "images.yml", []byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		t.Fatal("the step that actually published the image was accepted")
	}
	if !strings.Contains(v[0], "may not be redistributed") {
		t.Errorf("violation does not say why:\n%s", v[0])
	}
}

func TestPushShapes(t *testing.T) {
	step := func(push string) string {
		return `
jobs:
  publish:
    steps:
      - name: build
        uses: docker/build-push-action@v6
        with:
          context: docker/workbench-claude
          push: ` + push + "\n"
	}
	for _, tc := range []struct {
		name, push string
		wantFlag   bool
	}{
		{"boolean true", "true", true},
		// Actions treats these identically; a linter that understood only
		// the first would be evaded by accident, not by cunning.
		{"quoted true", `"true"`, true},
		{"yes", `"yes"`, true},
		// An expression cannot be evaluated here.  Refusing a marked context
		// that MIGHT publish is the safe direction to be wrong in.
		{"an expression", `"${{ github.ref == 'refs/heads/main' }}"`, true},
		// Build-only is legitimate: it distributes nothing.
		{"boolean false", "false", false},
		{"quoted false", `"false"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Check(repoRoot, "wf.yml", []byte(step(tc.push)))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(v) > 0; got != tc.wantFlag {
				t.Errorf("push: %s flagged = %v, want %v (%v)", tc.push, got, tc.wantFlag, v)
			}
		})
	}
}

// TestCatchesTheBackDoor: build-push-action is the sanctioned path, so a raw
// `docker push` is either a mistake or a route around the check above.
func TestCatchesTheBackDoor(t *testing.T) {
	const wf = `
jobs:
  publish:
    steps:
      - name: sneak it out
        run: |
          docker build -t ghcr.io/x/y docker/workbench-claude
          docker push ghcr.io/x/y
`
	v, err := Check(repoRoot, "wf.yml", []byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		t.Fatal("a raw `docker push` step was accepted, which routes around the context check entirely")
	}
}

// TestUnmarkedContextsPublishFreely — the lint must not become a tax on the
// images that are meant to ship.
func TestUnmarkedContextsPublishFreely(t *testing.T) {
	const wf = `
jobs:
  publish:
    steps:
      - name: Build and push the workers image
        uses: docker/build-push-action@v6
        with:
          context: docker/workers
          push: true
`
	v, err := Check(repoRoot, "images.yml", []byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("a publishable image was refused: %v", v)
	}
}

// TestPushWithoutAContextIsRefused: a push this lint cannot attribute to a
// build context is a push it cannot vet.
func TestPushWithoutAContextIsRefused(t *testing.T) {
	const wf = `
jobs:
  publish:
    steps:
      - uses: docker/build-push-action@v6
        with:
          push: true
`
	v, err := Check(repoRoot, "wf.yml", []byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		t.Error("a push with no context was accepted — nothing can tell what it publishes")
	}
}

// TestMarkerMakesTheDifference proves the marker is load-bearing rather than
// decorative: the same step, against a context with and without one.
func TestMarkerMakesTheDifference(t *testing.T) {
	root := t.TempDir()
	ctx := filepath.Join(root, "docker", "thing")
	if err := os.MkdirAll(ctx, 0o755); err != nil {
		t.Fatal(err)
	}
	const wf = `
jobs:
  publish:
    steps:
      - uses: docker/build-push-action@v6
        with:
          context: docker/thing
          push: true
`
	before, err := Check(root, "wf.yml", []byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("unmarked context was refused: %v", before)
	}
	if err := os.WriteFile(filepath.Join(ctx, MarkerName), []byte("# nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Check(root, "wf.yml", []byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) == 0 {
		t.Error("adding the marker changed nothing — the guard is decorative")
	}
}

func contains(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}
