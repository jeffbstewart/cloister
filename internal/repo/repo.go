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

	mu       sync.RWMutex
	sh       *shield.Shield
	files    map[string]*file // rel path → state; dirs included (IsDir entries)
	sorted   []string         // sorted keys, rebuilt on swap
	spent    int64            // resident bytes
	lastScan ScanStats        // timing of the most recent Rescan
}

// ScanStats reports where a scan spent its wall-clock: the metadata walk
// (shield load + stat every visible file — the serial cost that dominates
// on a slow bind mount) versus the content read (the parallelized file
// reads plus budget assignment).
type ScanStats struct {
	Walk time.Duration
	Read time.Duration
}

// Total is the whole scan's wall-clock.
func (s ScanStats) Total() time.Duration { return s.Walk + s.Read }

// now is the clock behind scan-phase timing; tests swap it to make the
// measured Walk/Read durations deterministic rather than racing the real
// clock.
var now = time.Now

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

// scanConcurrency bounds the parallel content reads.  The scan is
// I/O-latency-bound — each read waits on a bind-mount round-trip, not the
// CPU — so overlapping many reads shrinks wall-clock far past core count
// (a 52 s serial scan of a Docker Desktop mount was the motivating case).
const scanConcurrency = 16

// Rescan walks the workspace metadata, reloading the shield and any file
// whose size or mtime changed, adding new paths, dropping vanished ones.
// New files that would exceed the budget stay metadata-only (whyNot =
// ErrOverBudget) — a live session is never killed for growth.  The new
// state swaps in atomically.
//
// The heavy step — reading changed/new file content — runs concurrently
// (scanConcurrency workers).  Budget assignment stays deterministic: it
// happens after the reads, in sorted path order, so which files fall
// out over-budget never depends on goroutine timing.
func (r *Repo) Rescan() error {
	walkStart := now()
	sh, err := shield.Load(os.DirFS(r.root))
	if err != nil {
		return fmt.Errorf("repo: load shield: %w", err)
	}

	r.mu.RLock()
	old := r.files
	r.mu.RUnlock()

	// Parallel metadata walk: workers scan directories concurrently, each
	// feeding its subdirectories back into the queue.  The per-file stat is
	// the bind mount's dominant cost (a 70 s serial walk forced this), and
	// scanning directories in parallel overlaps those round-trips.
	raws := parallelWalk(r.root, sh)

	// Serial post-process — pure map/memory work, no I/O: build the model
	// entries, carry unchanged content over, and collect the changed/new
	// files that still need a content read.
	fresh := make(map[string]*file, len(raws))
	var toLoad []*file // visible, in-cap, changed/new: needs a content read
	var spent int64    // carry-overs are counted up front and never evicted
	for _, e := range raws {
		if e.isDir {
			fresh[e.rel] = &file{entry: Entry{Path: e.rel, IsDir: true, ModTime: e.modTime, Visibility: e.vis}}
			continue
		}
		f := &file{entry: Entry{Path: e.rel, Size: e.size, ModTime: e.modTime, Visibility: e.vis}}
		fresh[e.rel] = f
		if e.vis != shield.Visible {
			continue // Stripped: metadata only, content never resident
		}
		if e.size > r.cfg.MaxFileSize {
			f.whyNot = ErrTooLarge
			continue
		}
		// Unchanged since last load: carry the resident content over,
		// counted immediately so budget assignment never evicts it.
		if prev, ok := old[e.rel]; ok && prev.content != nil &&
			prev.entry.Size == e.size && prev.entry.ModTime.Equal(e.modTime) {
			f.content = prev.content
			f.entry = prev.entry
			spent += int64(len(f.content))
			continue
		}
		toLoad = append(toLoad, f)
	}
	// One clock read marks the boundary: end of the walk, start of the read.
	walkEnd := now()
	walkDur := walkEnd.Sub(walkStart)

	// Read every changed/new file's content concurrently — no budget yet.
	readContentParallel(r.root, toLoad)

	// Assign budget deterministically, smallest first (path as tiebreaker):
	// when the set overflows, the FEWEST files are evicted — the largest
	// ones fall out, not whatever sorts last alphabetically.  Carry-overs
	// are already counted and never evicted.  Evicted files had their
	// content read then discarded — bounded, and only on the boot-refuse /
	// mid-session-growth paths.
	sort.Slice(toLoad, func(i, j int) bool {
		if toLoad[i].entry.Size != toLoad[j].entry.Size {
			return toLoad[i].entry.Size < toLoad[j].entry.Size
		}
		return toLoad[i].entry.Path < toLoad[j].entry.Path
	})
	for _, f := range toLoad {
		if f.content == nil {
			continue // binary (ErrNotText) or a read error: never resident
		}
		if spent+int64(len(f.content)) > r.cfg.Budget {
			f.content = nil
			f.entry.Resident, f.entry.LineCount, f.entry.SHA256 = false, 0, ""
			f.whyNot = ErrOverBudget
			continue
		}
		spent += int64(len(f.content))
	}
	readDur := now().Sub(walkEnd)

	sorted := make([]string, 0, len(fresh))
	for k := range fresh {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	r.mu.Lock()
	r.sh, r.files, r.sorted, r.spent = sh, fresh, sorted, spent
	r.lastScan = ScanStats{Walk: walkDur, Read: readDur}
	r.mu.Unlock()
	return nil
}

// ScanStats returns the timing of the most recent scan.
func (r *Repo) ScanStats() ScanStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastScan
}

// walkConcurrency bounds the directory-scanning workers; walkQueueDepth
// buffers the directory worklist.  The walk is I/O-latency-bound on a bind
// mount — each readdir and stat waits on a round-trip — so scanning many
// directories at once overlaps those waits (a 70 s serial stat-walk was
// the motivating case).  The buffer must exceed the greatest number of
// directories pending at once; a source tree has hundreds, far under this,
// so a worker's enqueue never blocks in practice.
const (
	walkConcurrency = 16
	walkQueueDepth  = 4096
)

// rawEntry is one path's metadata as the parallel walk discovers it,
// before the serial pass turns it into model state.
type rawEntry struct {
	rel     string
	isDir   bool
	size    int64
	modTime time.Time
	vis     shield.Visibility
}

// parallelWalk scans the tree under root with a fixed pool of workers
// pulling directories from one buffered channel; each worker enqueues the
// subdirectories it finds back into that same channel.  Discovered entries
// are collected without a lock: workers send batches to an unbuffered
// results channel and a single goroutine owns the output slice.  A
// pending-count WaitGroup lets the main goroutine close the worklist once
// every queued directory has been scanned.  .git and Hidden (.gitignore)
// subtrees are pruned — never descended.  Returns every visible or stripped
// path's metadata in unspecified order (the caller sorts).
func parallelWalk(root string, sh *shield.Shield) []rawEntry {
	jobs := make(chan string, walkQueueDepth)
	results := make(chan []rawEntry)
	var pending sync.WaitGroup // directories enqueued but not yet scanned
	var workers sync.WaitGroup

	// Single collector owns out — no mutex; share by communicating.
	var out []rawEntry
	collected := make(chan struct{})
	go func() {
		for batch := range results {
			out = append(out, batch...)
		}
		close(collected)
	}()

	// A directory's children are enqueued (each a pending.Add) during its
	// scan, before its own pending.Done, so the counter never hits zero
	// mid-walk — the standard safe shape for a self-feeding WaitGroup.
	enqueue := func(relDir string) {
		pending.Add(1)
		jobs <- relDir
	}

	for i := 0; i < walkConcurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for relDir := range jobs {
				if local := scanDir(root, relDir, sh, enqueue); len(local) > 0 {
					results <- local
				}
				pending.Done()
			}
		}()
	}

	enqueue("")    // the root's children
	pending.Wait() // every discovered directory has been scanned
	close(jobs)    // workers fall out of their range loop
	workers.Wait()
	close(results) // collector finishes and signals
	<-collected
	return out
}

// isRepoMeta reports whether a slash path has a .git component — git
// metadata the librarian never exposes, at any depth (submodules and
// worktrees plant nested .git), matched case-insensitively.
func isRepoMeta(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.EqualFold(seg, ".git") {
			return true
		}
	}
	return false
}

// classify is the ONE decision for a path's standing in the model, from
// its slash path and dir-ness: whether it is admissible at all and, if so,
// its visibility.  Both the walk and the on-demand admission path consult
// it, so a new gate lands in exactly one place and the two can never
// drift.  admit == false means excluded — .git or Hidden (.gitignore):
// never in the model, and explicit access denies without an existence
// oracle.
func classify(sh *shield.Shield, rel string, isDir bool) (vis shield.Visibility, admit bool) {
	if isRepoMeta(rel) {
		return shield.Hidden, false
	}
	vis = sh.Visibility(rel, isDir)
	return vis, vis != shield.Hidden
}

// scanDir reads one directory, records its visible/stripped children, and
// enqueues visible/stripped subdirectories for further scanning.  An
// unreadable directory is skipped, not fatal.
func scanDir(root, relDir string, sh *shield.Shield, enqueue func(string)) []rawEntry {
	absDir := root
	if relDir != "" {
		absDir = filepath.Join(root, filepath.FromSlash(relDir))
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}
	var out []rawEntry
	for _, e := range entries {
		rel := e.Name()
		if relDir != "" {
			rel = relDir + "/" + e.Name()
		}
		isDir := e.IsDir()
		vis, admit := classify(sh, rel, isDir)
		if !admit {
			continue // .git or hidden: never recorded, never descended
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if isDir {
			out = append(out, rawEntry{rel: rel, isDir: true, modTime: info.ModTime(), vis: vis})
			enqueue(rel)
			continue
		}
		if !info.Mode().IsRegular() {
			continue // symlinks etc.: confinement rejects them everywhere else
		}
		out = append(out, rawEntry{rel: rel, size: info.Size(), modTime: info.ModTime(), vis: vis})
	}
	return out
}

// readContentParallel loads the files' content through a pool of
// scanConcurrency workers reading one shared channel.  Each worker
// touches only the *file it pulls, so no locking is needed; loadContent
// sets content (success) or leaves it nil and records whyNot (binary or
// read error).
func readContentParallel(root string, files []*file) {
	if len(files) == 0 {
		return
	}
	workers := scanConcurrency
	if len(files) < workers {
		workers = len(files)
	}
	// Buffer the whole worklist so the feed loop never blocks on a busy
	// worker: fill, close, and let the pool drain.
	ch := make(chan *file, len(files))
	for _, f := range files {
		ch <- f
	}
	close(ch)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for f := range ch {
				if err := loadContent(root, f); err != nil {
					f.whyNot = err
				}
			}
		}()
	}
	wg.Wait()
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
func (r *Repo) Read(rel string) (shield.AIReadable, error) {
	rel = path.Clean(filepath.ToSlash(rel))
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.lookupLocked(rel)
	if err != nil {
		return shield.AIReadable{}, err
	}
	if f.entry.IsDir {
		return shield.AIReadable{}, ErrIsDir
	}
	if f.entry.Visibility != shield.Visible {
		return shield.AIReadable{}, ErrForbidden
	}
	r.reloadLocked(rel, f)
	if f2, ok := r.files[rel]; !ok {
		return shield.AIReadable{}, ErrNotFound // vanished under revalidation
	} else if f2.content == nil {
		return shield.AIReadable{}, f2.whyNot
	}
	// The visibility check above already guarantees Clear succeeds; route the
	// bytes through it so what leaves the repo is a genuine capability — content
	// the shield minted, not a bare slice that merely claims clearance.
	ar, _ := r.sh.Clear(rel, f.content)
	return ar, nil
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
	// Excluded paths (.git, Hidden) deny without revealing existence —
	// same central decision the walk uses, so the two never drift.
	if _, admit := classify(r.sh, rel, isDir); !admit {
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
	vis, admit := classify(r.sh, rel, info.IsDir())
	if !admit {
		return nil // .git or hidden: never admitted
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
// Stripped and Hidden content is structurally unreachable here; each
// visited file is handed to the callback as a shield-minted AIReadable, so
// even bulk scans cannot observe content the shield did not clear.  The
// content slice the AIReadable carries must not be retained or mutated.
func (r *Repo) ForEachResident(fn func(shield.AIReadable) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.sorted {
		f := r.files[p]
		if f.content == nil {
			continue
		}
		// Resident implies Visible, so Clear always succeeds here; the ok
		// guard is belt-and-suspenders against a future model change.
		ar, ok := r.sh.Clear(p, f.content)
		if !ok {
			continue
		}
		if err := fn(ar); err != nil {
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

// Report summarizes the in-memory model: what content is held, and the
// heaviest files holding it.  The librarian logs this at boot so the
// operator can see what fills the budget and tune .gitignore/.aiignore
// exemptions from evidence rather than guesswork.
type Report struct {
	Bytes   int64   // resident content bytes
	Files   int     // files holding content in RAM
	Budget  int64   // configured cap
	Largest []Entry // up to the requested N resident files, largest first
}

// Report builds a Report naming the topN largest resident files.
func (r *Repo) Report(topN int) Report {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rep := Report{Bytes: r.spent, Budget: r.cfg.Budget}
	largest := make([]Entry, 0, len(r.sorted))
	for _, p := range r.sorted {
		if f := r.files[p]; f.content != nil {
			rep.Files++
			largest = append(largest, f.entry)
		}
	}
	sort.Slice(largest, func(i, j int) bool { return largest[i].Size > largest[j].Size })
	if len(largest) > topN {
		largest = largest[:topN]
	}
	rep.Largest = largest
	return rep
}

// All returns a sorted snapshot of every Entry in the model — the
// substrate for tree, glob, and recently-modified listings.  Hidden
// paths are absent by construction.
func (r *Repo) All() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.sorted))
	for _, p := range r.sorted {
		out = append(out, r.files[p].entry)
	}
	return out
}

// Watchable reports whether a directory is worth a filesystem watch:
// everything except Hidden subtrees (build outputs churn constantly and
// would burn kernel watch descriptors for events the model ignores).
func (r *Repo) Watchable(relDir string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sh.Visibility(relDir, true) != shield.Hidden
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
