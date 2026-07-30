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

package archive

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// State is current_state()'s answer: where the working tree stands.
// The documented idiom is to read it before any destructive verb.
type State struct {
	Branch    string       // checked-out branch; "" when HEAD is detached
	Default   string       // the default branch (start_work's base)
	Published bool         // the branch has a published counterpart
	Ahead     int          // checkpoints not yet published (0 when unpublished)
	Behind    int          // published checkpoints not yet local
	Dirty     []FileChange // tracked files whose content differs from the last checkpoint
	Untracked []string     // files no checkpoint has ever recorded
	SetAside  int          // parcels parked by set_aside and not yet resumed
}

// Clean reports whether nothing separates the working tree from the
// last checkpoint (set-aside parcels do not count: they are parked, not
// pending).
func (s State) Clean() bool { return len(s.Dirty) == 0 && len(s.Untracked) == 0 }

// FileChange is one dirty entry.
type FileChange struct {
	Path   string
	Status string // "modified", "added", "deleted", "renamed", "type-changed", "conflicted"
	From   string // the old path, for renames only
}

// CurrentState reports branch, publication standing, dirty files, and
// set-aside count in one read.
func (a *Archive) CurrentState(ctx context.Context) (State, error) {
	if err := a.guardConfig(ctx); err != nil {
		return State{}, err
	}
	return a.currentState(ctx)
}

// currentState is CurrentState without the config guard, for the verbs
// that have already run it (the guard is a per-verb precondition, not a
// per-invocation one).
func (a *Archive) currentState(ctx context.Context) (State, error) {
	st := State{Default: a.def.String()}
	branch, err := a.currentBranch(ctx)
	if err != nil {
		return State{}, err
	}
	st.Branch = branch

	out, err := a.run.out(ctx, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		return State{}, err
	}
	if err := parseStatus(out, &st); err != nil {
		return State{}, err
	}

	if branch != "" {
		up, err := a.upstreamOf(ctx, branch)
		if err != nil {
			return State{}, err
		}
		st.Published = up != ""
	}
	if !st.Published {
		st.Ahead, st.Behind = 0, 0
	}

	_, code, err := a.run.exit(ctx, "rev-parse", "--verify", "--quiet", "refs/stash")
	if err != nil {
		return State{}, err
	}
	if code == 0 { // refs/stash exists only while parcels are parked
		count, err := a.run.out(ctx, "rev-list", "--walk-reflogs", "--count", "refs/stash")
		if err != nil {
			return State{}, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(count))
		if err != nil {
			return State{}, fmt.Errorf("archive: unparseable stash count %q", count)
		}
		st.SetAside = n
	}
	return st, nil
}

// parseStatus fills st from `status --porcelain=v2 --branch -z` output:
// NUL-terminated records; '#' headers, then '1' (ordinary change), '2'
// (rename/copy, old path in the NEXT NUL field), 'u' (unmerged), '?'
// (untracked).  '!' (ignored) does not appear without --ignored.
func parseStatus(out string, st *State) error {
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if rec == "" {
			continue
		}
		switch {
		case strings.HasPrefix(rec, "# branch.ab "):
			// "# branch.ab +A -B"
			parts := strings.Fields(rec)
			if len(parts) == 4 {
				a, err1 := strconv.Atoi(strings.TrimPrefix(parts[2], "+"))
				b, err2 := strconv.Atoi(strings.TrimPrefix(parts[3], "-"))
				if err1 == nil && err2 == nil {
					st.Ahead, st.Behind = a, b
				}
			}
		case strings.HasPrefix(rec, "#"):
			// other headers: branch.oid, branch.head, branch.upstream
		case strings.HasPrefix(rec, "1 "):
			parts := strings.SplitN(rec, " ", 9)
			if len(parts) != 9 {
				return fmt.Errorf("archive: unparseable status record %q", rec)
			}
			st.Dirty = append(st.Dirty, FileChange{Path: parts[8], Status: statusWord(parts[1])})
		case strings.HasPrefix(rec, "2 "):
			parts := strings.SplitN(rec, " ", 10)
			if len(parts) != 10 || i+1 >= len(fields) {
				return fmt.Errorf("archive: unparseable rename record %q", rec)
			}
			i++ // the old path rides in the next NUL field
			st.Dirty = append(st.Dirty, FileChange{Path: parts[9], Status: "renamed", From: fields[i]})
		case strings.HasPrefix(rec, "u "):
			parts := strings.SplitN(rec, " ", 11)
			if len(parts) != 11 {
				return fmt.Errorf("archive: unparseable conflict record %q", rec)
			}
			st.Dirty = append(st.Dirty, FileChange{Path: parts[10], Status: "conflicted"})
		case strings.HasPrefix(rec, "? "):
			st.Untracked = append(st.Untracked, rec[2:])
		}
	}
	return nil
}

// statusWord folds porcelain-v2's two-letter staged/worktree state into
// the contract's vocabulary.  Because checkpoints always re-stage from
// the worktree, the staged-vs-worktree distinction carries no meaning
// here — only what kind of change the file has.
func statusWord(xy string) string {
	switch {
	case strings.Contains(xy, "A"):
		return "added"
	case strings.Contains(xy, "D"):
		return "deleted"
	case strings.Contains(xy, "T"):
		return "type-changed"
	default:
		return "modified"
	}
}

// Pending is pending_changes()'s answer: the uncommitted delta as a
// unified diff, plus the untracked files a diff cannot show but the
// next checkpoint would record.
type Pending struct {
	Diff      string
	Untracked []string
}

// PendingChanges reports the delta between the working tree and the
// last checkpoint — the whole tree, or one path.
func (a *Archive) PendingChanges(ctx context.Context, path string) (Pending, error) {
	if err := a.guardConfig(ctx); err != nil {
		return Pending{}, err
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "HEAD"}
	if path != "" {
		if err := validPath(path); err != nil {
			return Pending{}, fmt.Errorf("archive: pending_changes: %w", err)
		}
		args = append(args, "--", path)
	}
	diff, err := a.run.out(ctx, args...)
	if err != nil {
		return Pending{}, err
	}
	st, err := a.currentState(ctx)
	if err != nil {
		return Pending{}, err
	}
	untracked := st.Untracked
	if path != "" {
		var scoped []string
		for _, u := range untracked {
			if u == path || strings.HasPrefix(u, path+"/") {
				scoped = append(scoped, u)
			}
		}
		untracked = scoped
	}
	return Pending{Diff: diff, Untracked: untracked}, nil
}
