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

// Package shield is the single source of workspace visibility decisions,
// shared by the librarian (serving reads) and the scribe (refusing writes
// that would covertly read): which paths the agent may see, which it may
// read, and which writes must be held for human approval.
//
// Two ignore files drive it, both in gitignore syntax, with different
// meanings (docs/librarian.md):
//
//   - .gitignore — "not part of the project": build outputs, caches.
//     Matched paths are HIDDEN — absent from listings, trees, and
//     searches; explicit reads deny.
//   - .aiignore — "part of the project, but shielded": matched paths are
//     STRIPPED — named in listings with read/execute permission removed;
//     any content operation denies.
//
// The ignore files themselves are always visible and readable (the agent
// may know its own constraints), and .aiignore edits are gated for human
// approval — without the gate the agent could unshield itself.
//
// The package is deliberately stdlib-only with no subprocesses; a test
// asserts the import graph stays that way.
package shield

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Visibility is a path's listing state.
type Visibility string

const (
	// Visible: an ordinary path — listed normally, content readable.
	Visible Visibility = "visible"
	// Stripped: matched by .aiignore — listed by name with read and
	// execute permission bits stripped; every content op denies.
	Stripped Visibility = "stripped"
	// Hidden: matched by .gitignore — absent from listings, trees,
	// globs, and searches entirely; explicit reads still deny.
	Hidden Visibility = "hidden"
)

// GitignoreName and AiignoreName are the two ignore-file base names.
const (
	GitignoreName = ".gitignore"
	AiignoreName  = ".aiignore"
)

// Shield answers visibility questions for one loaded workspace state.
// Load a fresh one on rescan; a Shield itself is immutable and safe for
// concurrent use.
type Shield struct {
	git *matcher
	ai  *matcher
}

// Load walks fsys (the workspace root) collecting every .gitignore and
// .aiignore, and parses them into a Shield.  Directories named .git are
// not descended into (workspace confinement rejects them everywhere
// else; see internal/workspace).
func Load(fsys fs.FS) (*Shield, error) {
	var gitFiles, aiFiles []ignoreFile
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.EqualFold(d.Name(), ".git") {
			return fs.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		switch d.Name() {
		case GitignoreName, AiignoreName:
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return fmt.Errorf("shield: read %s: %w", p, err)
			}
			f := ignoreFile{base: path.Dir(p), lines: string(data)}
			if f.base == "." {
				f.base = ""
			}
			if d.Name() == GitignoreName {
				gitFiles = append(gitFiles, f)
			} else {
				aiFiles = append(aiFiles, f)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Shield{git: newMatcher(gitFiles), ai: newMatcher(aiFiles)}, nil
}

// Visibility classifies a slash-separated, root-relative path.  When both
// ignore files match, Hidden wins: "not part of the project" is the
// stronger statement, and content is denied either way.  Ignore files
// themselves are always Visible.
func (s *Shield) Visibility(rel string, isDir bool) Visibility {
	if IsIgnoreFile(rel) {
		return Visible
	}
	if s.git.ignored(rel, isDir) {
		return Hidden
	}
	if s.ai.ignored(rel, isDir) {
		return Stripped
	}
	return Visible
}

// MayRead reports whether the path's content may be served.  Only Visible
// paths are readable; Stripped and Hidden both deny (the caller audits
// the denial — see docs/librarian.md, "Audit: denials only").
func (s *Shield) MayRead(rel string, isDir bool) bool {
	return s.Visibility(rel, isDir) == Visible
}

// MustGate reports whether a WRITE to the path requires human approval
// under this package's rules: the .aiignore files themselves, since an
// ungated edit would let the agent unshield itself.  (Build-logic gating
// is the scribe's own, pre-existing set.)
func (s *Shield) MustGate(rel string) bool {
	return path.Base(rel) == AiignoreName
}

// IsIgnoreFile reports whether the path is one of the two ignore files —
// always visible and readable, whatever patterns say.
func IsIgnoreFile(rel string) bool {
	base := path.Base(rel)
	return base == GitignoreName || base == AiignoreName
}

// --- gitignore-syntax matching engine ---

// ignoreFile is one ignore file's raw content plus the directory (root-
// relative, "" for the root) its patterns are anchored to.
type ignoreFile struct {
	base  string
	lines string
}

// pattern is one parsed ignore line.
type pattern struct {
	base     string   // patterns apply only beneath their file's dir
	segs     []string // slash-split pattern; "**" spans segments
	negate   bool     // "!" prefix: re-include
	dirOnly  bool     // trailing "/": matches directories only
	anchored bool     // "/" at start or middle: relative to base, not any depth
}

// matcher holds every pattern from every ignore file of one kind, in
// evaluation order: shallower files first, file order within a file, so
// that last-match-wins gives deeper files and later lines precedence —
// git's rules.
type matcher struct {
	patterns []pattern
}

func newMatcher(files []ignoreFile) *matcher {
	sort.SliceStable(files, func(i, j int) bool {
		return strings.Count(files[i].base, "/")+len(files[i].base) <
			strings.Count(files[j].base, "/")+len(files[j].base)
	})
	m := &matcher{}
	for _, f := range files {
		for _, line := range strings.Split(f.lines, "\n") {
			if p, ok := parseLine(f.base, line); ok {
				m.patterns = append(m.patterns, p)
			}
		}
	}
	return m
}

// parseLine parses one ignore line per gitignore rules: blanks and #
// comments skipped (escapable), trailing unescaped spaces trimmed, "!"
// negation, trailing "/" dir-only, "/" at start-or-middle anchors.
func parseLine(base, line string) (pattern, bool) {
	line = strings.TrimSuffix(line, "\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}
	// Trailing spaces are ignored unless escaped with backslash.
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, "\\ ") {
		line = line[:len(line)-1]
	}
	line = strings.ReplaceAll(line, "\\ ", " ")
	if line == "" {
		return pattern{}, false
	}
	p := pattern{base: base}
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	line = strings.TrimPrefix(line, "\\") // \# and \! literals
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = strings.TrimPrefix(line, "/")
	} else if strings.Contains(line, "/") {
		p.anchored = true // a middle slash also anchors
	}
	if line == "" {
		return pattern{}, false
	}
	p.segs = strings.Split(line, "/")
	if !p.anchored {
		// Unanchored: may match at any depth beneath the base.
		p.segs = append([]string{"**"}, p.segs...)
	}
	return p, true
}

// ignored applies git's algorithm: a path is ignored if any ancestor
// directory is ignored (no re-inclusion beneath an excluded directory),
// else the last matching pattern decides.
func (m *matcher) ignored(rel string, isDir bool) bool {
	if rel == "." || rel == "" {
		return false
	}
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		if m.decide(strings.Join(segs[:i], "/"), true) {
			return true
		}
	}
	return m.decide(rel, isDir)
}

// decide runs every pattern in order over one exact path; the last match
// wins.
func (m *matcher) decide(rel string, isDir bool) bool {
	ignored := false
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		target, ok := underBase(p.base, rel)
		if !ok {
			continue
		}
		if matchSegs(p.segs, strings.Split(target, "/")) {
			ignored = !p.negate
		}
	}
	return ignored
}

// underBase strips base from rel, reporting whether rel lies beneath it.
func underBase(base, rel string) (string, bool) {
	if base == "" {
		return rel, true
	}
	prefix := base + "/"
	if !strings.HasPrefix(rel, prefix) {
		return "", false
	}
	return rel[len(prefix):], true
}

// matchSegs matches pattern segments against path segments; "**" spans
// any number of segments (including none), other segments use path.Match
// semantics within the segment.
func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for skip := 0; skip <= len(segs); skip++ {
			if matchSegs(pat[1:], segs[skip:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}
