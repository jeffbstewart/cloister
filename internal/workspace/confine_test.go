package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newRoot(t *testing.T) *Root {
	t.Helper()
	r, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolveAcceptsConfinedPaths(t *testing.T) {
	r := newRoot(t)
	// Pre-create a nested dir so the leading components exist and are checked.
	if err := os.MkdirAll(filepath.Join(r.Dir(), "src", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"file.txt",
		"src/main/Foo.kt",             // forward slashes, even on Windows
		"src/main/new/Bar.kt",         // tail does not exist yet (a create)
		"a/../file.txt",               // cleans back inside the root
		filepath.Join(r.Dir(), "x"),   // ABSOLUTE under the root — tolerated
		filepath.Join(r.Dir(), "y.k"), // absolute create under the root
	} {
		got, err := r.Resolve(in)
		if err != nil {
			t.Errorf("Resolve(%q) rejected a confined path: %v", in, err)
			continue
		}
		if got.IsZero() || !underRoot(r.Dir(), got.String()) {
			t.Errorf("Resolve(%q) = %q, not under root", in, got.String())
		}
	}
}

func TestResolveRejectsEscapes(t *testing.T) {
	r := newRoot(t)
	absOutside := filepath.Join(t.TempDir(), "outside.txt") // absolute, different tree
	tests := []struct {
		in   string
		want error
	}{
		{"", ErrEmptyPath},
		{"../etc/passwd", ErrEscapes},
		{"a/../../b", ErrEscapes},
		{absOutside, ErrEscapes}, // absolute path OUTSIDE the root
	}
	for _, tt := range tests {
		_, err := r.Resolve(tt.in)
		if !errors.Is(err, tt.want) {
			t.Errorf("Resolve(%q) err = %v, want %v", tt.in, err, tt.want)
		}
	}
}

// TestResolveRejectsSymlinkComponent is the headline Phase 0 acceptance: a path
// that traverses a symlink is rejected outright, never resolved.  Skips where the
// environment can't create symlinks (e.g. Windows without Developer Mode).
func TestResolveRejectsSymlinkComponent(t *testing.T) {
	r := newRoot(t)
	outside := t.TempDir() // a directory OUTSIDE the workspace
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A symlink INSIDE the workspace pointing outside it.
	link := filepath.Join(r.Dir(), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlinks in this environment (%v); rejection logic is OS-agnostic and covered on platforms that can", err)
	}

	// Directly at the symlink, and traversing through it — both must reject.
	for _, rel := range []string{"escape", "escape/secret"} {
		_, err := r.Resolve(rel)
		if !errors.Is(err, ErrSymlink) {
			t.Errorf("Resolve(%q) err = %v, want ErrSymlink (symlink must be rejected, not followed)", rel, err)
		}
	}

	// A symlinked leaf FILE is also rejected.
	fileLink := filepath.Join(r.Dir(), "alias.txt")
	if err := os.Symlink(filepath.Join(outside, "secret"), fileLink); err != nil {
		t.Skipf("cannot create file symlink: %v", err)
	}
	if _, err := r.Resolve("alias.txt"); !errors.Is(err, ErrSymlink) {
		t.Errorf("Resolve of a symlinked file err = %v, want ErrSymlink", err)
	}
}

func TestOpenRejectsRelativeRoot(t *testing.T) {
	if _, err := Open("relative/dir"); err == nil {
		t.Error("Open accepted a relative root")
	}
}
