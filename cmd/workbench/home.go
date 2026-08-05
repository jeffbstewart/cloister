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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Resetting HOME at dispose, and why that is this program's job.
//
// The grange is destroyed when a task ends.  `/home/agent` was not, and
// there was never a design rationale for the difference: the volume exists
// so that dotfiles and shell state never cross CELLS (docs/ARCHITECTURE.md),
// which is an argument about scoping.  Durability came along for the ride
// because a named volume is how you get per-project scoping, and named
// volumes happen to persist.
//
// That leaves a channel wider than it first looks.  It is not only somewhere
// to stash bytes between tasks: `useradd --create-home` seeds HOME from
// /etc/skel, docker copies that into the volume on first use, and from then
// on the volume's copy wins and is agent-writable.  `~/.bashrc` and
// `~/.profile` are EXECUTED by every interactive bash afterwards — the
// operator's shell, and the agent's own tool-spawned shells.  So an agent
// that writes a dotfile has arbitrary code running in every later task in
// that cell, outliving the workspace destruction the whole design turns on.
// (The image's own banner is in /etc/bash.bashrc, root-owned on the
// read-only rootfs.  It is the home-volume copy that is the hole.)
//
// WORKBENCH, NOT THE ARCHIVIST.  The obvious owner is the archivist, since
// it owns `dispose`.  But it would need a mount it does not have — write
// access to the agent's HOME is a new adjacency, and compose-lint refuses
// one today for good reasons.  It also runs at the same uid this program
// does, so it would buy no privilege advantage.  Meanwhile the session
// manager already runs inside the agent container, already owns "a
// workspace's lifetime is a session's lifetime", and is already the thing
// that calls dispose.  Same guarantee, no new adjacency.
//
// What this is NOT: a defence against an agent actively fighting the
// cleaner in real time. Both run as uid 1000, so neither can bind the other.
// The threat being closed is state SURVIVING a task boundary, which this
// closes completely — whatever the agent wrote is gone before the next task
// starts.

// resetHome returns HOME to the image's skeleton after a task ends:
// everything the agent could have written is removed, then /etc/skel is laid
// back down so the next task starts from a known state rather than an empty
// one.
//
// preserve names the direct children of home that must survive — they are
// the cell's OTHER mounts, and they are found rather than hard-coded (see
// mountedChildren).  This matters more than it might seem: `~/caches` is the
// operator's per-user warmed dependency bind, shared across every project,
// and an `os.RemoveAll` that walked into it would silently destroy hours of
// warmed Gradle and Go caches belonging to work this cell never touched.
func resetHome(home, skel string, preserve map[string]bool) (removed []string, err error) {
	// Rails first.  This function deletes recursively, so a caller that
	// passed something empty, relative, or shallow must fail loudly and not
	// get a best-effort attempt.
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("refusing to reset a non-absolute HOME %q", home)
	}
	if clean := filepath.Clean(home); clean == "/" || strings.Count(clean, string(filepath.Separator)) < 2 {
		return nil, fmt.Errorf("refusing to reset %q — too shallow to be an agent HOME", home)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", home, err)
	}

	var failed []string
	for _, e := range entries {
		if preserve[e.Name()] {
			continue
		}
		target := filepath.Join(home, e.Name())
		if err := os.RemoveAll(target); err != nil {
			// Keep going.  A single undeletable entry must not leave the
			// rest of HOME carrying the agent's state into the next task,
			// and the operator gets told what stayed.
			failed = append(failed, fmt.Sprintf("%s (%v)", e.Name(), err))
			continue
		}
		removed = append(removed, e.Name())
	}

	if err := reseed(skel, home); err != nil {
		failed = append(failed, fmt.Sprintf("restoring %s (%v)", skel, err))
	}
	if len(failed) > 0 {
		return removed, fmt.Errorf("could not fully reset %s: %s", home, strings.Join(failed, "; "))
	}
	return removed, nil
}

// reseed copies the skeleton dotfiles back into home.  A missing skeleton is
// not an error: an empty HOME is a perfectly usable one, and refusing the
// whole reset because /etc/skel is absent would leave the agent's files in
// place — trading the property we care about for one we do not.
func reseed(skel, home string) error {
	entries, err := os.ReadDir(skel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // /etc/skel is dotfiles in practice; a tree here is not ours to interpret
		}
		data, err := os.ReadFile(filepath.Join(skel, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(home, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// mountedChildren returns the direct children of home that are themselves
// mount points, read from a /proc/self/mountinfo stream.
//
// Discovered rather than listed, deliberately.  A hard-coded set of names
// (caches, .qwen, .claude-session) is a second place the topology lives, and
// the failure mode when compose adds a mount and this list does not is that
// `resetHome` walks into a bind mount and deletes the operator's data.  The
// kernel already knows the answer.
func mountedChildren(mountinfo io.Reader, home string) map[string]bool {
	home = filepath.Clean(home)
	out := map[string]bool{}
	sc := bufio.NewScanner(mountinfo)
	// Mount tables are short, but a pathological line must not be a panic.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// mountinfo: id parent major:minor root MOUNTPOINT options...
		// The mount point is field 5 (1-indexed); octal-escaped for spaces.
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		point := filepath.Clean(unescapeMountPoint(fields[4]))
		rel, err := filepath.Rel(home, point)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		// Only the FIRST component: a mount at ~/caches/gradle means
		// ~/caches must survive, which is the same answer.
		out[strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]] = true
	}
	return out
}

// unescapeMountPoint undoes the octal escaping the kernel applies to space,
// tab, newline and backslash in mountinfo paths.
func unescapeMountPoint(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var n int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%o", &n); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// resetHomeAfterDispose is the wiring: refuse unless we are demonstrably
// inside a cell, find the mounts to spare, reset, and say what happened.
// Reported rather than silent, because a cleaner nobody can see is one
// nobody notices has stopped working.
//
// THE IMAGE-MARKER GUARD IS NOT PARANOIA.  This function recursively deletes
// $HOME, and cmd/workbench is an ordinary Go program that a developer or a
// CI job can run.  On a GitHub runner $HOME is /home/runner — absolute,
// three levels deep, and therefore straight through every structural rail in
// resetHome.  The only thing that reliably separates "inside the cell" from
// "on someone's laptop" is a file that exists solely in the image, so that
// is what is checked, and the failure is a no-op.
func (m *manager) resetHomeAfterDispose() {
	if _, err := os.Stat(imageMarkerPath); err != nil {
		m.printf("note: not in a cloister image (%s absent) — leaving HOME alone\n", imageMarkerPath)
		return
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/agent"
	}
	mi, err := os.Open(mountInfoPath)
	if err != nil {
		// Without the mount table we cannot tell the agent's dotfiles from
		// the operator's warmed caches, and guessing risks deleting the
		// latter.  Skipping is the safe failure.
		m.printf("note: %s is unreadable (%v) — leaving %s alone rather than risk deleting a mounted cache\n", mountInfoPath, err, home)
		return
	}
	preserve := mountedChildren(mi, home)
	_ = mi.Close()

	removed, err := resetHome(home, skelPath, preserve)
	if err != nil {
		m.printf("note: %v\n", err)
	}
	m.printf("reset %s — %d entr%s the agent could have written removed, %d mount(s) kept\n",
		home, len(removed), plural(len(removed), "y", "ies"), len(preserve))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

const (
	mountInfoPath = "/proc/self/mountinfo"
	skelPath      = "/etc/skel"
	// Written by docker/workbench/Dockerfile; exists in no other context.
	imageMarkerPath = "/etc/cloister-worker/toolchain"
)
