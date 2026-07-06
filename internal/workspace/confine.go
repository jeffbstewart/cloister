// Package workspace holds the scribe's confinement and safe-write primitives:
// every path the scribe touches is confined under a single root, symlinks are
// REJECTED outright (never followed or resolved), writes are atomic, and text
// content is UTF-8 validated.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Rejection reasons, exported so callers (and tests) can distinguish them and
// map them to audit decisions (rejected_confinement).
var (
	ErrEmptyPath = errors.New("workspace: empty path")
	ErrEscapes   = errors.New("workspace: path is outside the workspace root")
	ErrSymlink   = errors.New("workspace: path contains a symlink or reparse-point component (rejected, not resolved)")
)

// Path is a workspace-confined absolute path.  It can only be produced by
// Root.Resolve (which validates confinement and rejects symlinks), so a non-zero
// Path is proof of validation.  The write primitives and ops take a Path, never a
// raw string — an unvalidated path cannot reach the filesystem.  The unexported
// field makes a non-zero Path unforgeable outside this package.
type Path struct {
	abs string
}

// String returns the absolute filesystem path ("" for the zero Path).
func (p Path) String() string { return p.abs }

// IsZero reports the never-resolved zero Path.
func (p Path) IsZero() bool { return p.abs == "" }

// Root is a confined workspace: the sole directory tree the scribe may touch.
// The root itself is trusted configuration; only agent-supplied paths beneath it
// are validated.
type Root struct {
	dir string // cleaned absolute path of the workspace root
}

// Open returns a Root for an absolute directory path.
func Open(dir string) (*Root, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("workspace: root %q must be absolute", dir)
	}
	return &Root{dir: filepath.Clean(dir)}, nil
}

// Dir returns the workspace root.
func (r *Root) Dir() string { return r.dir }

// Resolve validates an agent-supplied path and returns a confined Path, or a
// rejection error.  The input may be RELATIVE to the workspace root, or ABSOLUTE
// under the root — agents commonly emit `/workspace/...`, so we tolerate and
// normalize it rather than reject.  The rules:
//
//   - after cleaning, the path must lie within the root; absolute paths outside
//     the root and post-clean `..` escapes are rejected (ErrEscapes);
//   - no existing component may be a symlink or reparse point — we `lstat` each
//     and REJECT the first one we find; we never follow or resolve a symlink.
//
// A path whose leading directories exist symlink-free but whose tail does not
// yet exist is allowed (a create); a not-yet-existing component cannot be a
// symlink.
func (r *Root) Resolve(input string) (Path, error) {
	if input == "" {
		return Path{}, ErrEmptyPath
	}
	var abs string
	if filepath.IsAbs(input) {
		abs = filepath.Clean(input)
	} else {
		abs = filepath.Clean(filepath.Join(r.dir, input))
	}
	if !underRoot(r.dir, abs) {
		return Path{}, ErrEscapes
	}
	if err := r.rejectSymlinkComponents(abs); err != nil {
		return Path{}, err
	}
	return Path{abs: abs}, nil
}

// underRoot reports whether abs is the root itself or lies beneath it, using a
// separator-terminated prefix so that `/workspace-evil` is not treated as being
// under `/workspace`.  abs is always constructed by joining onto r.dir, so the
// prefix comparison is case-exact by construction (no Windows-case pitfalls).
func underRoot(root, abs string) bool {
	if abs == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(abs, prefix)
}

// rejectSymlinkComponents lstat's each component from the root down to abs and
// rejects the first symlink / reparse-point it finds.  Walking stops at the first
// non-existent component (its subtree is a create and cannot contain a symlink).
func (r *Root) rejectSymlinkComponents(abs string) error {
	rel, err := filepath.Rel(r.dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ErrEscapes
	}
	if rel == "." {
		return nil // the root itself; trusted config
	}
	cur := r.dir
	for _, comp := range strings.Split(rel, string(os.PathSeparator)) {
		if comp == "" {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // this and everything below is new — nothing to resolve
			}
			return fmt.Errorf("workspace: lstat %q: %w", cur, err)
		}
		// ModeSymlink covers POSIX symlinks and Windows symlinks; ModeIrregular
		// catches Windows junctions / other reparse points Go can't classify.
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return ErrSymlink
		}
	}
	return nil
}
