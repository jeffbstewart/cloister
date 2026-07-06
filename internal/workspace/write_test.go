package workspace

import (
	"os"
	"testing"
)

func TestWriteAtomic(t *testing.T) {
	r := newRoot(t)
	p, err := r.Resolve("out.txt")
	if err != nil {
		t.Fatal(err)
	}

	if err := WriteAtomic(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p.String()); string(got) != "hello" {
		t.Errorf("content = %q, want hello", got)
	}

	// Overwrite replaces atomically.
	if err := WriteAtomic(p, []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p.String()); string(got) != "world" {
		t.Errorf("content after overwrite = %q, want world", got)
	}

	// No temp files are left behind.
	entries, err := os.ReadDir(r.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "out.txt" {
		t.Errorf("dir contents = %v, want only out.txt (no leftover temp)", entries)
	}
}

// TestWriteAtomicRejectsZeroPath: the zero Path is the only Path forgeable
// outside the package; WriteAtomic must refuse it (the structural gate).
func TestWriteAtomicRejectsZeroPath(t *testing.T) {
	if err := WriteAtomic(Path{}, []byte("x"), 0o644); err != ErrUnresolvedPath {
		t.Errorf("WriteAtomic(zero Path) err = %v, want ErrUnresolvedPath", err)
	}
}

func TestWriteAtomicFailsIfParentMissing(t *testing.T) {
	r := newRoot(t)
	p, err := r.Resolve("nope/out.txt") // parent "nope" doesn't exist (a valid create path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(p, []byte("x"), 0o644); err == nil {
		t.Error("WriteAtomic succeeded with a missing parent directory")
	}
}

func TestValidUTF8(t *testing.T) {
	if !ValidUTF8([]byte("plain ascii and - an em-dash")) {
		t.Error("valid UTF-8 reported invalid")
	}
	if ValidUTF8([]byte{0x61, 0x97, 0x62}) { // 0x97 = a lone Windows-1252 em-dash byte
		t.Error("invalid UTF-8 (lone 0x97) reported valid")
	}
}
