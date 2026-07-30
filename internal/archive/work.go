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
	"errors"
	"fmt"
	"strings"
)

// ErrNoChanges reports a verb that had nothing to act on: a checkpoint
// with a clean tree, a set_aside with nothing to park.
var ErrNoChanges = errors.New("archive: nothing to record")

// ErrDirtyTree reports a verb refused because uncommitted work would be
// clobbered or carried; checkpoint or set_aside first.
var ErrDirtyTree = errors.New("archive: the working tree has uncommitted changes")

// ErrDefaultBranch reports a mutation refused on the default branch —
// work happens on a line of work (start_work), never on the default
// branch, whose only legitimate motion is sync_from_upstream.
var ErrDefaultBranch = errors.New("archive: refusing to modify the default branch")

// ConflictError reports a sync_from_upstream whose replay conflicted.
// The replay was aborted: the tree is back where it was, and the listed
// files are where the two lines of work disagree.
type ConflictError struct {
	Files []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("archive: replaying onto the updated default branch conflicts in: %s",
		strings.Join(e.Files, ", "))
}

// identityArgs pins author and committer from the endpoint-table
// identity on every checkpoint-creating invocation.  Never read from
// git config: nothing that can write .git/config may choose who the
// archivist commits as.
func (a *Archive) identityArgs() []string {
	return []string{
		"-c", "user.name=" + a.ident.Name,
		"-c", "user.email=" + a.ident.Email,
	}
}

// StartWork begins a new line of work off the local default branch.
// Uncommitted changes ride along (git refuses the switch itself if that
// cannot be done cleanly), so "edit first, branch when it becomes
// real" works.
func (a *Archive) StartWork(ctx context.Context, name BranchName) error {
	if name.IsZero() {
		return fmt.Errorf("archive: start_work: a branch name is required")
	}
	if name.String() == a.def.String() {
		return fmt.Errorf("%w: start_work(%s)", ErrDefaultBranch, name)
	}
	if err := a.guardConfig(ctx); err != nil {
		return err
	}
	// --end-of-options is belt on a parsed name: even a future parser bug
	// cannot turn the start point into a flag.
	_, err := a.run.out(ctx, "switch", "-c", name.String(), "--end-of-options", a.def.String())
	return err
}

// AbandonWork discards a local line of work: switches to the default
// branch when the doomed branch is checked out, then deletes it.  It
// refuses the default branch and a dirty tree — losing uncommitted work
// takes restore or dispose(force), never a side effect.  (The remote
// counterpart's deletion is a remote verb, arriving with the jail.)
func (a *Archive) AbandonWork(ctx context.Context, name BranchName) error {
	if name.IsZero() {
		return fmt.Errorf("archive: abandon_work: a branch name is required")
	}
	if name.String() == a.def.String() {
		return fmt.Errorf("%w: abandon_work(%s)", ErrDefaultBranch, name)
	}
	if err := a.guardConfig(ctx); err != nil {
		return err
	}
	st, err := a.currentState(ctx)
	if err != nil {
		return err
	}
	if !st.Clean() {
		return fmt.Errorf("%w: abandon_work would discard them with the branch; checkpoint, set_aside, or restore first", ErrDirtyTree)
	}
	if st.Branch == name.String() {
		if _, err := a.run.out(ctx, "switch", "--end-of-options", a.def.String()); err != nil {
			return err
		}
	}
	_, err = a.run.out(ctx, "branch", "-D", name.String())
	return err
}

// Checkpoint records the working tree — all of it, or just the named
// paths — as one checkpoint, and returns its id.  There are no staging
// verbs: the tree is re-staged from scratch here, every time.
func (a *Archive) Checkpoint(ctx context.Context, message string, paths []string) (CheckpointID, error) {
	if err := validMessage(message); err != nil {
		return CheckpointID{}, fmt.Errorf("archive: checkpoint: %w", err)
	}
	for _, p := range paths {
		if err := validPath(p); err != nil {
			return CheckpointID{}, fmt.Errorf("archive: checkpoint: %w", err)
		}
	}
	if err := a.guardConfig(ctx); err != nil {
		return CheckpointID{}, err
	}
	branch, err := a.currentBranch(ctx)
	if err != nil {
		return CheckpointID{}, err
	}
	if branch == "" {
		return CheckpointID{}, fmt.Errorf("archive: checkpoint: not on a branch — a checkpoint recorded on a detached HEAD belongs to no line of work")
	}
	if branch == a.def.String() {
		return CheckpointID{}, fmt.Errorf("%w: checkpoints belong on a line of work — start_work first", ErrDefaultBranch)
	}

	addArgs := []string{"add", "-A"}
	// The -c identity flags must precede the subcommand.
	commitArgs := append(a.identityArgs(), "commit", "-m", message)
	if len(paths) > 0 {
		addArgs = append(addArgs, "--")
		addArgs = append(addArgs, paths...)
		commitArgs = append(commitArgs, "--")
		commitArgs = append(commitArgs, paths...)
	}
	if _, err := a.run.out(ctx, addArgs...); err != nil {
		return CheckpointID{}, err
	}
	// Detect emptiness before committing so "nothing changed" is a
	// typed answer, not a parsed git message.
	diffArgs := []string{"diff", "--no-ext-diff", "--no-textconv", "--cached", "--quiet"}
	if len(paths) > 0 {
		diffArgs = append(diffArgs, "--")
		diffArgs = append(diffArgs, paths...)
	}
	_, code, err := a.run.exit(ctx, diffArgs...)
	if err != nil {
		return CheckpointID{}, err
	}
	if code == 0 {
		return CheckpointID{}, fmt.Errorf("%w: checkpoint(%q)", ErrNoChanges, message)
	}
	if _, err := a.run.out(ctx, commitArgs...); err != nil {
		return CheckpointID{}, err
	}
	head, err := a.run.out(ctx, "rev-parse", "HEAD")
	if err != nil {
		return CheckpointID{}, err
	}
	id, err := ParseCheckpointID(head)
	if err != nil {
		return CheckpointID{}, fmt.Errorf("archive: checkpoint: git returned %q for HEAD: %w", head, err)
	}
	return id, nil
}

// RestoreResult says what Restore actually did — under the append-only
// rule the same request realizes differently before and after
// publication, and the agent deserves to know which happened.
type RestoreResult struct {
	// Rewound is true when history moved (reset --hard); false when
	// content was brought forward into the working tree, to be recorded
	// by the next checkpoint.
	Rewound bool
}

// Restore rolls back.  Four shapes:
//
//	path only                 discard one file's local edits (to the last checkpoint)
//	checkpoint + path         one file's content from that checkpoint
//	checkpoint only           the whole tree to that checkpoint
//	neither                   discard all local edits (to the last checkpoint)
//
// Whole-tree restore to a checkpoint is a rollback, so the checkpoint
// must lie on the current line of work (an ancestor of HEAD) — anything
// else would silently relocate the branch onto foreign history — and it
// is refused on the default branch, whose only legitimate motion is
// sync_from_upstream.  It obeys "published history is append-only":
// while every published checkpoint remains an ancestor of the target,
// history rewinds; otherwise the checkpoint's content is restored into
// the tree and recording it is the next checkpoint's job.  Untracked
// files are never deleted — parking or discarding them is set_aside's
// or dispose's business.
func (a *Archive) Restore(ctx context.Context, checkpoint CheckpointID, path string) (RestoreResult, error) {
	if err := a.guardConfig(ctx); err != nil {
		return RestoreResult{}, err
	}
	if path != "" {
		if err := validPath(path); err != nil {
			return RestoreResult{}, fmt.Errorf("archive: restore: %w", err)
		}
		source := "HEAD"
		if !checkpoint.IsZero() {
			source = checkpoint.String()
		}
		_, err := a.run.out(ctx, "restore", "--source="+source, "--worktree", "--staged", "--", path)
		return RestoreResult{}, err
	}
	if checkpoint.IsZero() {
		if _, err := a.run.out(ctx, "reset", "--hard", "HEAD"); err != nil {
			return RestoreResult{}, err
		}
		return RestoreResult{Rewound: true}, nil
	}

	branch, err := a.currentBranch(ctx)
	if err != nil {
		return RestoreResult{}, err
	}
	if branch == a.def.String() {
		return RestoreResult{}, fmt.Errorf("%w: whole-tree restore to a checkpoint", ErrDefaultBranch)
	}
	reachable, err := a.isAncestor(ctx, checkpoint.String(), "HEAD")
	if err != nil {
		return RestoreResult{}, err
	}
	if !reachable {
		return RestoreResult{}, fmt.Errorf("archive: restore: %s is not a checkpoint on the current line of work (history lists the ones that are)", checkpoint)
	}
	rewindSafe := true
	if branch != "" {
		up, err := a.upstreamOf(ctx, branch)
		if err != nil {
			return RestoreResult{}, err
		}
		if up != "" {
			// Safe to rewind only if the published tip stays an
			// ancestor: the next publish must remain a fast-forward.
			rewindSafe, err = a.isAncestor(ctx, up, checkpoint.String())
			if err != nil {
				return RestoreResult{}, err
			}
		}
	}
	if rewindSafe {
		if _, err := a.run.out(ctx, "reset", "--hard", checkpoint.String()); err != nil {
			return RestoreResult{}, err
		}
		return RestoreResult{Rewound: true}, nil
	}
	// Forward motion: the checkpoint's content, the branch's history.
	if _, err := a.run.out(ctx, "restore", "--source="+checkpoint.String(), "--worktree", "--staged", "--", "."); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Rewound: false}, nil
}

// SetAside parks all uncommitted work — tracked edits and untracked
// files — so the tree matches the last checkpoint.  Resume recovers it.
func (a *Archive) SetAside(ctx context.Context) error {
	if err := a.guardConfig(ctx); err != nil {
		return err
	}
	st, err := a.currentState(ctx)
	if err != nil {
		return err
	}
	if st.Clean() {
		return fmt.Errorf("%w: set_aside", ErrNoChanges)
	}
	_, err = a.run.out(ctx, append(a.identityArgs(),
		"stash", "push", "--include-untracked", "-m", "set_aside")...)
	return err
}

// Resume recovers the most recently parked parcel.  A conflict with
// work done since leaves the conflict markers in the tree and keeps the
// parcel parked, exactly as git does; current_state shows both.
func (a *Archive) Resume(ctx context.Context) error {
	if err := a.guardConfig(ctx); err != nil {
		return err
	}
	st, err := a.currentState(ctx)
	if err != nil {
		return err
	}
	if st.SetAside == 0 {
		return fmt.Errorf("%w: resume — nothing is set aside", ErrNoChanges)
	}
	_, err = a.run.out(ctx, append(a.identityArgs(), "stash", "pop")...)
	return err
}

// SyncResult says what SyncFromUpstream did.
type SyncResult struct {
	// Replayed is true when a line of work was replayed onto the
	// updated default branch (false when the default branch itself was
	// checked out and simply fast-forwarded).
	Replayed bool
	// Merged is true when the replay was a merge (the published-branch
	// realization); false means rebase or no replay.
	Merged bool
}

// SyncFromUpstream updates the local default branch from its remote and
// replays the current line of work on it.  The replay is a rebase while
// the branch is unpublished; once published it is a merge (the rebase
// would need a refused force-push).  A conflicted replay is aborted —
// tree restored, *ConflictError returned — rather than left half-done.
func (a *Archive) SyncFromUpstream(ctx context.Context) (SyncResult, error) {
	if err := a.guardConfig(ctx); err != nil {
		return SyncResult{}, err
	}
	st, err := a.currentState(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if st.Branch == "" {
		return SyncResult{}, fmt.Errorf("archive: sync_from_upstream: not on a branch")
	}
	if !st.Clean() {
		return SyncResult{}, fmt.Errorf("%w: sync_from_upstream replays history; checkpoint or set_aside first", ErrDirtyTree)
	}

	def := a.def.String()
	if st.Branch == def {
		// The refspec is explicit even here, where `fetch origin` would
		// do: a bare `fetch origin` obeys the configured refspecs, and a
		// configured refspec is something .git/config could have made
		// force-update local refs.  (guardConfig refuses a '+' refspec
		// too; naming the refspec means not depending on that.)
		if _, err := a.run.out(ctx, "fetch", "--end-of-options", originRemote,
			"refs/heads/"+def+":refs/remotes/"+originRemote+"/"+def); err != nil {
			return SyncResult{}, err
		}
		_, err := a.run.out(ctx, "merge", "--ff-only", "--end-of-options", originRemote+"/"+def)
		return SyncResult{}, err
	}

	// Updating the unchecked-out default: fetch refuses non-fast-forward
	// on its own (no leading '+' on the refspec), which is exactly the
	// append-only stance.
	if _, err := a.run.out(ctx, "fetch", "--end-of-options", originRemote, def+":"+def); err != nil {
		return SyncResult{}, err
	}

	up, err := a.upstreamOf(ctx, st.Branch)
	if err != nil {
		return SyncResult{}, err
	}
	if up == "" {
		if _, err := a.run.out(ctx, append(a.identityArgs(), "rebase", "--end-of-options", def)...); err != nil {
			return SyncResult{}, a.abortReplay(ctx, "rebase", err)
		}
		return SyncResult{Replayed: true}, nil
	}
	if _, err := a.run.out(ctx, append(a.identityArgs(),
		"merge", "--no-edit", "-m", "sync_from_upstream: merge "+def, "--end-of-options", def)...); err != nil {
		return SyncResult{}, a.abortReplay(ctx, "merge", err)
	}
	return SyncResult{Replayed: true, Merged: true}, nil
}

// abortReplay turns a conflicted rebase/merge into a clean tree and a
// typed ConflictError; any other replay failure passes through.  The
// abort itself is verified by exit code — a ConflictError promises the
// tree is back where it was, and a failed abort (which git reports as a
// non-zero exit, not a spawn error) would make that promise a lie.
func (a *Archive) abortReplay(ctx context.Context, op string, cause error) error {
	// Read the conflicted files before the abort wipes them.  -z framing:
	// a path may contain anything but NUL.
	files, filesErr := a.run.out(ctx, "diff", "-z", "--no-ext-diff", "--no-textconv", "--name-only", "--diff-filter=U")
	if filesErr != nil || files == "" {
		// Not a content conflict (or unreadable): there may be no replay
		// in progress to abort, so abort best-effort and surface the
		// original failure.
		a.run.exit(ctx, op, "--abort")
		return cause
	}
	_, code, err := a.run.exit(ctx, op, "--abort")
	if err != nil {
		return fmt.Errorf("archive: aborting the conflicted %s: %v; the workspace needs manual recovery: %w", op, err, cause)
	}
	if code != 0 {
		return fmt.Errorf("archive: `%s --abort` exited %d; the workspace needs manual recovery: %w", op, code, cause)
	}
	return &ConflictError{Files: strings.Split(strings.TrimRight(files, "\x00"), "\x00")}
}
