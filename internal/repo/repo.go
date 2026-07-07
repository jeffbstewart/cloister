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

// Package repo is the librarian's in-memory workspace model: the entire
// agent-visible tree resident in RAM, filtered through internal/shield,
// under a hard byte budget (docs/librarian.md).
//
// Coherence, per the 2026-07-07 spike: container writers (scribe,
// builder) are covered by an inotify watcher feeding Invalidate; host
// edits generate no events on Docker Desktop mounts, so single-file
// reads stat-revalidate on access and the owner runs Rescan on a
// once-a-minute ticker.  The model is safe for concurrent use; Rescan
// swaps state atomically.
//
// Budget posture: over budget at construction is a refusal naming the
// largest offenders (the operator's tuning signal); growth past the
// budget mid-session denies loading the NEW files, never kills the
// session.  Stripped and oversized/binary files are metadata-only —
// their content never enters memory.  Hidden files have no entry at all.
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jeffbstewart/cloister/internal/shield"
)

// Typed refusals.  ErrForbidden and friends carry enough for the caller
// to build the denial audit record; they are rejections, not failures.
var (
	ErrNotFound   = errors.New("repo: no such path")
	ErrForbidden  = errors.New("repo: content is shielded") // Stripped or Hidden
	ErrNotText    = errors.New("repo: not a text file")     // binary content is never resident
	ErrTooLarge   = errors.New("repo: file exceeds the per-file cap")
	ErrOverBudget = errors.New("repo: file not resident — loading it would exceed the memory budget")
	ErrIsDir      = errors.New("repo: path is a directory")
)

// Config sizes the model.  Both caps are required; fail closed.
type Config struct {
	// Budget is the total resident-content byte cap.  Exceeding it at
	// construction refuses to start; exceeding it later denies new loads.
	Budget int64
	// MaxFileSize is the per-file cap; larger files are metadata-only.
	MaxFileSize int64
}

// Entry is one path's metadata — what listings and stat serve.  Content
// is never part of an Entry.
type Entry struct {
	Path       string // root-relative, slash-separated
	IsDir      bool
	Size       int64
	ModTime    time.Time
	Visibility shield.Visibility // Visible or Stripped; Hidden paths have no Entry
	Resident   bool              // content held in RAM (text, within caps, in budget)
	LineCount  int               // resident text only
	SHA256     string            // resident text only
}

// file is the internal per-path state.
type file struct {
	entry   Entry
	content []byte // nil unless resident
	whyNot  error  // non-resident reason: ErrNotText / ErrTooLarge / ErrOverBudget
}

// Repo is the resident model.  Construct with New; refresh with Rescan
// (owner's ticker) and Invalidate (watcher events); read with the
// accessor methods, every one of which enforces the shield.
type Repo struct {
	root string
	cfg  Config

	mu     sync.RWMutex
	sh     *shield.Shield
	files  map[string]*file // rel path → state; dirs included (IsDir entries)
	sorted []string         // sorted keys, rebuilt on swap
	spent  int64            // resident bytes
}

// New loads the workspace at root.  Over-budget is a refusal whose error
// names the largest resident candidates — the signal for tuning
// .gitignore/.aiignore exemptions.
func New(root string, cfg Config) (*Repo, error) {
	if cfg.Budget <= 0 || cfg.MaxFileSize <= 0 {
		return nil, fmt.Errorf("repo: Budget and MaxFileSize are required and positive (fail closed)")
	}
	if cfg.MaxFileSize > cfg.Budget {
		return nil, fmt.Errorf("repo: MaxFileSize %d exceeds Budget %d", cfg.MaxFileSize, cfg.Budget)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	r := &Repo{root: abs, cfg: cfg}
	if err := r.Rescan(); err != nil {
		return nil, err
	}
	// Boot-time budget check is strict: refuse, don't degrade.
	r.mu.RLock()
	defer r.mu.RUnlock()
	if over := r.overBudgetOffenders(); over != "" {
		return nil, fmt.Errorf("repo: visible workspace exceeds the %d-byte budget; largest offenders:\n%s\ntune .gitignore/.aiignore exemptions (fail closed)", cfg.Budget, over)
	}
	return r, nil
}

// Rescan walks the workspace metadata, reloading the shield and any file
// whose size or mtime changed, adding new paths, dropping vanished ones.
// New files that would exceed the budget stay metadata-only (whyNot =
// ErrOverBudget) — a live session is never killed for growth.  The new
// state swaps in atomically.
func (r *Repo) Rescan() error {
	sh, err := shield.Load(os.DirFS(r.root))
	if err != nil {
		return fmt.Errorf("repo: load shield: %w", err)
	}

	fresh := make(map[string]*file)
	var spent int64
	r.mu.RLock()
	old := r.files
	r.mu.RUnlock()

	walkErr := filepath.WalkDir(r.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, don't fail the scan
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, r.root))
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		if d.IsDir() && strings.EqualFold(d.Name(), ".git") {
			return fs.SkipDir
		}
		vis := sh.Visibility(rel, d.IsDir())
		if vis == shield.Hidden {
			if d.IsDir() {
				return fs.SkipDir // invisible subtree: never walked
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if d.IsDir() {
			fresh[rel] = &file{entry: Entry{Path: rel, IsDir: true, ModTime: info.ModTime(), Visibility: vis}}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil // symlinks etc.: confinement rejects them everywhere else
		}
		f := &file{entry: Entry{
			Path: rel, Size: info.Size(), ModTime: info.ModTime(), Visibility: vis,
		}}
		fresh[rel] = f
		if vis != shield.Visible {
			return nil // Stripped: metadata only, content never resident
		}
		if info.Size() > r.cfg.MaxFileSize {
			f.whyNot = ErrTooLarge
			return nil
		}
		// Unchanged since last load: carry the resident content over.
		if prev, ok := old[rel]; ok && prev.content != nil &&
			prev.entry.Size == info.Size() && prev.entry.ModTime.Equal(info.ModTime()) {
			f.content = prev.content
			f.entry = prev.entry
			spent += int64(len(f.content))
			return nil
		}
		if spent+info.Size() > r.cfg.Budget {
			f.whyNot = ErrOverBudget
			return nil
		}
		if err := loadContent(r.root, f); err != nil {
			f.whyNot = err
			return nil
		}
		spent += int64(len(f.content))
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	sorted := make([]string, 0, len(fresh))
	for k := range fresh {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	r.mu.Lock()
	r.sh, r.files, r.sorted, r.spent = sh, fresh, sorted, spent
	r.mu.Unlock()
	return nil
}

// loadContent reads one visible, size-checked file into residence,
// refusing binary content (a NUL byte or invalid UTF-8).
func loadContent(root string, f *file) error {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.entry.Path)))
	if err != nil {
		return err
	}
	if isBinary(data) {
		return ErrNotText
	}
	f.content = data
	f.entry.Resident = true
	f.entry.Size = int64(len(data))
	f.entry.LineCount = countLines(data)
	sum := sha256.Sum256(data)
	f.entry.SHA256 = hex.EncodeToString(sum[:])
	return nil
}

// Invalidate marks one path changed (the watcher's callback): a known
// entry is re-stat'd and reloaded; an unknown one is admitted — CREATE
// events arrive precisely for paths the model has never seen.
func (r *Repo) Invalidate(rel string) {
	rel = path.Clean(filepath.ToSlash(rel))
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[rel]
	if !ok {
		if _, err := r.lookupLocked(rel); err != nil {
			return // hidden, vanished, or inadmissible: nothing to hold
		}
		return
	}
	if f.entry.IsDir {
		return
	}
	r.reloadLocked(rel, f)
}

// reloadLocked re-stats and reloads one file under the write lock.
func (r *Repo) reloadLocked(rel string, f *file) {
	info, err := os.Lstat(filepath.Join(r.root, filepath.FromSlash(rel)))
	if err != nil {
		// Vanished: drop the entry.
		r.spent -= int64(len(f.content))
		delete(r.files, rel)
		i := sort.SearchStrings(r.sorted, rel)
		if i < len(r.sorted) && r.sorted[i] == rel {
			r.sorted = append(r.sorted[:i], r.sorted[i+1:]...)
		}
		return
	}
	if f.entry.Size == info.Size() && f.entry.ModTime.Equal(info.ModTime()) {
		return // unchanged
	}
	r.spent -= int64(len(f.content))
	f.content = nil
	f.entry.Resident, f.entry.LineCount, f.entry.SHA256 = false, 0, ""
	f.entry.Size, f.entry.ModTime = info.Size(), info.ModTime()
	f.whyNot = nil
	if f.entry.Visibility != shield.Visible {
		return
	}
	if info.Size() > r.cfg.MaxFileSize {
		f.whyNot = ErrTooLarge
		return
	}
	if r.spent+info.Size() > r.cfg.Budget {
		f.whyNot = ErrOverBudget
		return
	}
	if err := loadContent(r.root, f); err != nil {
		f.whyNot = err
		return
	}
	r.spent += int64(len(f.content))
}

// Read returns a visible file's content, stat-revalidating first (the
// host-edit insurance the spike mandated).  A path absent from the model
// is stat'd and ADMITTED on the spot — re-stat of a known entry alone
// would miss a file created since the last rescan.  Stripped and Hidden
// paths refuse with ErrForbidden; the caller audits the denial.
func (r *Repo) Read(rel string) ([]byte, error) {
	rel = path.Clean(filepath.ToSlash(rel))
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.lookupLocked(rel)
	if err != nil {
		return nil, err
	}
	if f.entry.IsDir {
		return nil, ErrIsDir
	}
	if f.entry.Visibility != shield.Visible {
		return nil, ErrForbidden
	}
	r.reloadLocked(rel, f)
	if f2, ok := r.files[rel]; !ok {
		return nil, ErrNotFound // vanished under revalidation
	} else if f2.content == nil {
		return nil, f2.whyNot
	}
	return f.content, nil
}

// Stat returns one path's Entry (metadata is served for Stripped paths —
// name visibility is the design).  Unknown paths are admitted like Read;
// Hidden and absent behave identically.
func (r *Repo) Stat(rel string) (Entry, error) {
	rel = path.Clean(filepath.ToSlash(rel))
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.lookupLocked(rel)
	if err != nil {
		return Entry{}, err
	}
	return f.entry, nil
}

// lookupLocked resolves rel to model state, admitting a path that exists
// on disk but is not yet in the model (created since the last rescan —
// the case a bare re-stat cannot catch).  An explicit request for a
// Hidden path denies rather than 404s, whether or not it exists.
func (r *Repo) lookupLocked(rel string) (*file, error) {
	if f, ok := r.files[rel]; ok {
		return f, nil
	}
	full := filepath.Join(r.root, filepath.FromSlash(rel))
	info, statErr := os.Lstat(full)
	isDir := statErr == nil && info.IsDir()
	if r.sh.Visibility(rel, isDir) == shield.Hidden {
		return nil, ErrForbidden
	}
	if statErr != nil {
		return nil, ErrNotFound
	}
	if !isDir && !info.Mode().IsRegular() {
		return nil, ErrNotFound // symlinks etc. do not exist to the model
	}
	// Admit missing ancestor directories first so listings stay coherent.
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		r.admitLocked(strings.Join(segs[:i], "/"))
	}
	f := r.admitLocked(rel)
	if f == nil {
		return nil, ErrNotFound
	}
	return f, nil
}

// admitLocked stats one on-disk path and inserts its model entry (dirs as
// dir entries; files loaded via reloadLocked under the usual shield,
// per-file, and budget rules).  Returns nil if the path is not
// admissible.  Idempotent for already-present paths.
func (r *Repo) admitLocked(rel string) *file {
	if f, ok := r.files[rel]; ok {
		return f
	}
	info, err := os.Lstat(filepath.Join(r.root, filepath.FromSlash(rel)))
	if err != nil {
		return nil
	}
	vis := r.sh.Visibility(rel, info.IsDir())
	if vis == shield.Hidden {
		return nil
	}
	var f *file
	if info.IsDir() {
		f = &file{entry: Entry{Path: rel, IsDir: true, ModTime: info.ModTime(), Visibility: vis}}
	} else if !info.Mode().IsRegular() {
		return nil
	} else {
		// Size -1 guarantees reloadLocked sees "changed" and loads.
		f = &file{entry: Entry{Path: rel, Size: -1, Visibility: vis}}
	}
	r.files[rel] = f
	if i := sort.SearchStrings(r.sorted, rel); i == len(r.sorted) || r.sorted[i] != rel {
		r.sorted = append(r.sorted, "")
		copy(r.sorted[i+1:], r.sorted[i:])
		r.sorted[i] = rel
	}
	if !f.entry.IsDir {
		r.reloadLocked(rel, f)
		f = r.files[rel] // reload may have dropped a vanishing file
	}
	return f
}

// List returns a directory's immediate children, sorted: Visible and
// Stripped entries; Hidden paths simply are not there.
func (r *Repo) List(dir string) ([]Entry, error) {
	dir = path.Clean(filepath.ToSlash(dir))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if dir != "." {
		d, ok := r.files[dir]
		if !ok {
			return nil, ErrNotFound
		}
		if !d.entry.IsDir {
			return nil, fmt.Errorf("repo: %s is not a directory", dir)
		}
	}
	prefix := ""
	if dir != "." {
		prefix = dir + "/"
	}
	var out []Entry
	i := sort.SearchStrings(r.sorted, prefix)
	for ; i < len(r.sorted); i++ {
		p := r.sorted[i]
		if !strings.HasPrefix(p, prefix) {
			break
		}
		rest := p[len(prefix):]
		if rest == "" || strings.Contains(rest, "/") {
			continue // not an immediate child
		}
		out = append(out, r.files[p].entry)
	}
	return out, nil
}

// ForEachResident visits every resident (Visible, text, in-budget) file
// in sorted order — the substrate for tree-wide ops (search, count).
// Stripped and Hidden content is structurally unreachable here.  The
// content slice must not be retained or mutated.
func (r *Repo) ForEachResident(fn func(rel string, content []byte) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.sorted {
		f := r.files[p]
		if f.content == nil {
			continue
		}
		if err := fn(p, f.content); err != nil {
			return err
		}
	}
	return nil
}

// Resident reports current resident bytes and the configured budget.
func (r *Repo) Resident() (spent, budget int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.spent, r.cfg.Budget
}

// overBudgetOffenders returns a top-offender listing when non-resident-
// because-of-budget files exist, "" otherwise.  Callers hold the lock.
func (r *Repo) overBudgetOffenders() string {
	var over []*file
	for _, p := range r.sorted {
		if f := r.files[p]; errors.Is(f.whyNot, ErrOverBudget) {
			over = append(over, f)
		}
	}
	if len(over) == 0 {
		return ""
	}
	sort.Slice(over, func(i, j int) bool { return over[i].entry.Size > over[j].entry.Size })
	if len(over) > 10 {
		over = over[:10]
	}
	var b strings.Builder
	for _, f := range over {
		fmt.Fprintf(&b, "  %10d  %s\n", f.entry.Size, f.entry.Path)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// isBinary reports content the model never holds: a NUL byte or invalid
// UTF-8 (matching the scribe's text discipline).
func isBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	for _, b := range probe {
		if b == 0 {
			return true
		}
	}
	return !utf8.Valid(data)
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := strings.Count(string(data), "\n")
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}
