package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// Size caps.  Ops apply the one that fits
// their kind; these are the primitives' defaults.
const (
	MaxTextFileBytes   = 20 << 20 // text ops
	MaxBinaryFileBytes = 50 << 20 // write_binary_file (gated anyway)
)

// ErrNotUTF8 is returned by the text-op layer when content is not valid UTF-8
// and the caller did not set permit_non_utf8.  The primitive `ValidUTF8` is the
// check; ops decide the policy.
var ErrNotUTF8 = errors.New("workspace: content is not valid UTF-8")

// ErrUnresolvedPath guards the zero Path (the only Path forgeable outside the
// package): WriteAtomic refuses it, so a caller must go through Root.Resolve.
var ErrUnresolvedPath = errors.New("workspace: unresolved (zero) Path — use Root.Resolve")

// ValidUTF8 reports whether b is valid UTF-8.
func ValidUTF8(b []byte) bool { return utf8.Valid(b) }

// WriteAtomic writes data to a confined Path atomically: a temp file in the SAME
// directory (so the rename stays on one filesystem) is written, chmod'd, then
// renamed over the target.  A reader/builder never observes a torn file.  On any
// failure the temp file is removed.  The Path argument IS the confinement gate —
// it can only come from Root.Resolve — so no path re-check is needed here beyond
// refusing the zero value; the parent directory must exist.
func WriteAtomic(p Path, data []byte, perm os.FileMode) (err error) {
	if p.abs == "" {
		return ErrUnresolvedPath
	}
	absPath := p.abs
	dir := filepath.Dir(absPath)
	tmp, err := os.CreateTemp(dir, ".scribe-*.tmp")
	if err != nil {
		return fmt.Errorf("workspace: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName) // best-effort cleanup; nothing was renamed into place
		}
	}()

	// io.Writer's contract: Write returns a non-nil error whenever it writes
	// fewer than len(data) bytes, so a partial write cannot pass silently.
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("workspace: write temp: %w", err)
	}
	if err = tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("workspace: chmod temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("workspace: close temp: %w", err)
	}
	// os.Rename replaces the target atomically on POSIX and on Windows
	// (MoveFileEx with REPLACE_EXISTING).
	if err = os.Rename(tmpName, absPath); err != nil {
		return fmt.Errorf("workspace: rename into place: %w", err)
	}
	return nil
}
