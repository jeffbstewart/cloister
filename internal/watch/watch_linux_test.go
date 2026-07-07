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

//go:build linux

package watch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// collector gathers onChange paths with a wait helper.
type collector struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newCollector() *collector { return &collector{seen: map[string]bool{}} }

func (c *collector) change(rel string) {
	c.mu.Lock()
	c.seen[rel] = true
	c.mu.Unlock()
}

// waitFor polls until rel was seen or the deadline passes.  Polling an
// asynchronous kernel queue needs a real deadline; 5s is far beyond any
// observed latency and only slow CI would ever approach it.
func (c *collector) waitFor(t *testing.T, rel string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ok := c.seen[rel]
		c.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("never saw %q; saw %v", rel, c.seen)
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSeesWritesRenamesDeletes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "sub/existing.txt", "x")
	c := newCollector()
	w, err := New(root, nil, c.change, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	write(t, root, "a.txt", "one")
	c.waitFor(t, "a.txt")

	write(t, root, "sub/b.txt", "two")
	c.waitFor(t, "sub/b.txt")

	// The scribe's atomic pattern: tmp write + rename.
	write(t, root, "sub/.tmp1", "v2")
	if err := os.Rename(filepath.Join(root, "sub/.tmp1"), filepath.Join(root, "sub/existing.txt")); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "sub/existing.txt")

	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "a.txt") // delete reports the same path; Invalidate drops it
}

func TestNewDirectorySubtreeIsWatchedAndReported(t *testing.T) {
	root := t.TempDir()
	c := newCollector()
	w, err := New(root, nil, c.change, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// mkdir + immediate file: the create-before-watch race the tree
	// report exists for.
	write(t, root, "fresh/deep/file.txt", "born")
	c.waitFor(t, "fresh/deep/file.txt")

	// And the new subtree has a live watch of its own afterward.
	write(t, root, "fresh/deep/second.txt", "later")
	c.waitFor(t, "fresh/deep/second.txt")
}

func TestShouldDescendPrunes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "build/out.jar", "x")
	write(t, root, "src/a.go", "y")
	c := newCollector()
	w, err := New(root, func(rel string) bool { return rel != "build" }, c.change, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	write(t, root, "src/b.go", "z")
	c.waitFor(t, "src/b.go")

	write(t, root, "build/other.jar", "noise")
	time.Sleep(200 * time.Millisecond) // give a wrong event time to arrive
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen["build/other.jar"] {
		t.Error("pruned subtree produced events")
	}
}

func TestGitDirNeverWatched(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".git/config", "x")
	c := newCollector()
	w, err := New(root, nil, c.change, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	write(t, root, ".git/index", "y")
	write(t, root, "normal.txt", "z")
	c.waitFor(t, "normal.txt")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen[".git/index"] {
		t.Error(".git generated events")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, func(string) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// A second Close must not touch the (possibly reused) descriptor.
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
