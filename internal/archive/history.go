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
	"time"
)

// Change is one recorded checkpoint as history reports it.
type Change struct {
	ID      CheckpointID
	Time    time.Time
	Author  string
	Email   string
	Subject string
}

// historyCap bounds a history read; the contract says "capped" and the
// consumer is a model context, not a pager.
const historyCap = 200

// defaultHistoryLimit applies when the caller does not choose.
const defaultHistoryLimit = 50

// logFormat renders one Change per record: unit-separated fields, no
// user-controlled text before the last field.  Record framing is left
// to `log -z` (NUL between records) rather than an in-band terminator
// in the format string: a commit message can carry any byte but NUL, so
// a crafted subject in a fetched or agent-authored commit could smuggle
// a terminator and forge a phantom record with an attacker-chosen id
// and author — the exact fields callers trust.  NUL framing makes
// records unforgeable.
const logFormat = "--format=%H%x1f%ct%x1f%an%x1f%ae%x1f%s"

// HistoryQuery scopes a history read.  Zero values mean: the current
// branch, the whole tree, defaultHistoryLimit entries.
type HistoryQuery struct {
	Ref   Ref    // start point; zero → HEAD
	Path  string // limit to one path's changes; "" → whole tree
	Limit int    // max entries; 0 → defaultHistoryLimit, capped at historyCap
}

// History lists checkpoints newest-first.
func (a *Archive) History(ctx context.Context, q HistoryQuery) ([]Change, error) {
	if err := a.guardConfig(ctx); err != nil {
		return nil, err
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultHistoryLimit
	case limit > historyCap:
		limit = historyCap
	}
	args := []string{"log", "-z", "--no-decorate", logFormat, "-n", strconv.Itoa(limit)}
	if !q.Ref.IsZero() {
		args = append(args, q.Ref.String())
	}
	if q.Path != "" {
		if err := validPath(q.Path); err != nil {
			return nil, fmt.Errorf("archive: history: %w", err)
		}
		args = append(args, "--", q.Path)
	}
	out, err := a.run.out(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseChanges(out)
}

// parseChanges splits `log -z` logFormat output into Changes.
func parseChanges(out string) ([]Change, error) {
	var changes []Change
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 5)
		if len(f) != 5 {
			return nil, fmt.Errorf("archive: unparseable history record %q", rec)
		}
		id, err := ParseCheckpointID(f[0])
		if err != nil {
			return nil, fmt.Errorf("archive: history: %w", err)
		}
		epoch, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("archive: unparseable checkpoint time %q", f[1])
		}
		changes = append(changes, Change{
			ID:      id,
			Time:    time.Unix(epoch, 0).UTC(),
			Author:  f[2],
			Email:   f[3],
			Subject: f[4],
		})
	}
	return changes, nil
}

// ChangeWithDiff is show_change()'s answer: the checkpoint and its full
// unified diff against its parent.
type ChangeWithDiff struct {
	Change
	Diff string
}

// ShowChange resolves one checkpoint and returns it with its diff.
func (a *Archive) ShowChange(ctx context.Context, id CheckpointID) (ChangeWithDiff, error) {
	if id.IsZero() {
		return ChangeWithDiff{}, fmt.Errorf("archive: show_change: a checkpoint id is required")
	}
	if err := a.guardConfig(ctx); err != nil {
		return ChangeWithDiff{}, err
	}
	meta, err := a.run.out(ctx, "log", "-z", "--no-decorate", "-1", logFormat, id.String())
	if err != nil {
		return ChangeWithDiff{}, err
	}
	changes, err := parseChanges(meta)
	if err != nil {
		return ChangeWithDiff{}, err
	}
	if len(changes) != 1 {
		return ChangeWithDiff{}, fmt.Errorf("archive: show_change: %s did not resolve to one checkpoint", id)
	}
	// --format= suppresses the header (the metadata is already parsed);
	// -m with --first-parent keeps merge checkpoints showing a diff
	// against their first parent rather than nothing.
	diff, err := a.run.out(ctx, "show", "--format=", "--no-ext-diff", "--no-textconv",
		"--first-parent", "-m", id.String())
	if err != nil {
		return ChangeWithDiff{}, err
	}
	return ChangeWithDiff{Change: changes[0], Diff: diff}, nil
}

// FileAt returns one file's content at a revision without touching the
// working tree — how the corrector reads base/head context for a PR
// that is not checked out.
func (a *Archive) FileAt(ctx context.Context, ref Ref, path string) ([]byte, error) {
	if ref.IsZero() {
		return nil, fmt.Errorf("archive: file_at: a revision is required")
	}
	if err := validPath(path); err != nil {
		return nil, fmt.Errorf("archive: file_at: %w", err)
	}
	if err := a.guardConfig(ctx); err != nil {
		return nil, err
	}
	// cat-file with the explicit blob type: a rev:path that names a
	// directory (a tree) is an error, not a listing.  raw, not out —
	// file content keeps its trailing newlines.
	return a.run.raw(ctx, "cat-file", "blob", ref.String()+":"+path)
}
