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

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The recent-repositories list: the operator's shortcut past retyping a
// clone URL, kept on the per-user caches bind so it follows the person
// across their projects rather than dying with a cell.
//
// It is a CONVENIENCE INDEX, never a source of truth.  The archivist's
// disk-derived state is the only authority on what is provisioned; this
// file just remembers what was asked for.  Every failure to read or
// write it is therefore non-fatal — a lost list costs one typed URL,
// while a workbench that refused to start over a corrupt cache file
// would cost the whole session.
//
// Entries are recorded only after a provision SUCCEEDS.  That is what
// keeps the list free of typos and unreachable hosts, and it is why a
// "forget" affordance is a convenience rather than a necessity.

// maxEntries caps the list.  Twenty is a couple of screens; past that
// the list is worse than typing.
const maxEntries = 20

// entry is one remembered repository.
type entry struct {
	// Used is the last successful provision, epoch seconds (the house
	// on-disk time format).
	Used int64
	Repo string
	// Branch is the line of work last provisioned from this repository,
	// empty when it was the default branch.  Offered back as the default
	// on the next visit: resuming is the common case, and the codename
	// is exactly the thing nobody remembers.
	Branch string
}

// repoList is the ordered list, most recent first.
type repoList struct {
	path    string
	entries []entry
}

// loadRepos reads the list.  A missing, unreadable, or partly-garbled
// file yields whatever could be salvaged and no error: see the package
// note above.
func loadRepos(path string) *repoList {
	l := &repoList{path: path}
	f, err := os.Open(path)
	if err != nil {
		return l
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// used \t repo [\t branch]
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		used, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || parts[1] == "" {
			continue // a line we cannot read is a line we drop
		}
		e := entry{Used: used, Repo: parts[1]}
		if len(parts) > 2 {
			e.Branch = parts[2]
		}
		l.entries = append(l.entries, e)
	}
	// Sort on load; never trust the file's order.
	l.sort()
	return l
}

func (l *repoList) sort() {
	sort.SliceStable(l.entries, func(i, j int) bool {
		if l.entries[i].Used != l.entries[j].Used {
			return l.entries[i].Used > l.entries[j].Used
		}
		return l.entries[i].Repo < l.entries[j].Repo
	})
}

// touch records a successful provision, moving the repository to the
// front.  One entry per repository: the branch is overwritten, so the
// list answers "where was I on this repo", not "every branch I ever
// had".
func (l *repoList) touch(repo, branch string, now time.Time) {
	for i := range l.entries {
		if l.entries[i].Repo == repo {
			l.entries[i].Used = now.Unix()
			l.entries[i].Branch = branch
			l.sort()
			l.trim()
			return
		}
	}
	l.entries = append(l.entries, entry{Used: now.Unix(), Repo: repo, Branch: branch})
	l.sort()
	l.trim()
}

// forget drops one entry by its 1-based display position.
func (l *repoList) forget(n int) (entry, bool) {
	if n < 1 || n > len(l.entries) {
		return entry{}, false
	}
	e := l.entries[n-1]
	l.entries = append(l.entries[:n-1], l.entries[n:]...)
	return e, true
}

func (l *repoList) trim() {
	if len(l.entries) > maxEntries {
		l.entries = l.entries[:maxEntries]
	}
}

// at returns the entry at a 1-based display position.
func (l *repoList) at(n int) (entry, bool) {
	if n < 1 || n > len(l.entries) {
		return entry{}, false
	}
	return l.entries[n-1], true
}

// save writes the list atomically — a temp file in the same directory,
// then a rename — so an interrupted write cannot leave a half-list
// behind.  The error is returned for the caller to MENTION, not to fail
// on.
func (l *repoList) save() error {
	if l.path == "" {
		return nil
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".repos-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds

	w := bufio.NewWriter(tmp)
	for _, e := range l.entries {
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\n", e.Used, e.Repo, e.Branch); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), l.path)
}

// shortRepo trims a clone URL to the part worth reading in a menu:
// github.com/op/repo rather than https://github.com/op/repo.git.
func shortRepo(url string) string {
	s := url
	for _, p := range []string{"https://", "http://", "ssh://", "git@"} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.TrimSuffix(s, ".git")
}

// ago renders how long since an epoch-second stamp, coarsely — the
// operator wants "3d" to spot a stale entry, not a timestamp to do
// arithmetic on.
func ago(used int64, now time.Time) string {
	d := now.Sub(time.Unix(used, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
