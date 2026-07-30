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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runner executes the real git binary with no ambient trust
// (docs/archivist.md, "Hardened git execution").  Every invocation:
//
//   - points core.hooksPath at an empty directory we own, so
//     repository-supplied hooks never execute;
//   - pins the git dir and the work tree, so no config key can relocate
//     what git reads or writes;
//   - disables global and system config, overrides the program-naming
//     keys below, and — because that override list can never be
//     complete — refuses outright (guardConfig) to run in a workspace
//     whose repo-local config carries an unrecognized key;
//   - runs with a scrubbed environment: an allowlist of platform
//     plumbing, never the parent's GIT_* or credential variables;
//   - pins author/committer dates from the injected clock, so recorded
//     times are the archivist's testimony, not git's wall clock.
//
// Diff-producing verbs additionally pass --no-ext-diff/--no-textconv so
// config-named external commands never run; ssh/git/ext transports are
// refused outright (https for the relays, plain paths for test rigs).
type runner struct {
	git    string
	dir    string // the worktree root; every command runs -C here
	gitDir string // dir/.git, pinned with --git-dir on every command
	hooks  string // the empty hooks directory
	now    func() time.Time
}

// hardening is prepended to every invocation.  -c overrides beat the
// repo-local config, which is the only config file left readable.
//
// This list is defense in depth, NOT the containment boundary: -c can
// only override keys whose names are known ahead of time, and the
// exec-capable part of git's config space is open-ended (a filter,
// diff, or merge driver is named by the attacker: filter.<any>.clean).
// The boundary is guardConfig in config.go, which refuses to run git at
// all in a workspace whose repo-local config carries a key outside the
// allowlist.  Keys appear here anyway so that a config the guard has
// not yet learned about still lands on a safe default.
var hardening = []string{
	"-c", "credential.helper=",
	"-c", "core.fsmonitor=false",
	"-c", "gc.auto=0",
	"-c", "protocol.ssh.allow=never",
	"-c", "protocol.git.allow=never",
	"-c", "protocol.ext.allow=never",
	// A redirect is a way off the relay: the wire host resolves to the
	// relay by alias, and following a Location header would carry the
	// request (and its auth header) to wherever the response says.
	// NOTE: http.extraheader is deliberately NOT pinned here — the
	// remote overlay injects it via GIT_CONFIG_* env, and a -c would
	// override the env pair.
	"-c", "http.followRedirects=false",
	"-c", "commit.gpgsign=false",
	"-c", "tag.gpgsign=false",
	"-c", "color.ui=false",
	"-c", "advice.detachedHead=false",
	// Config keys that name a program to run.  core.editor and
	// core.pager are pointed at "false"/"cat" rather than emptied so a
	// verb that unexpectedly wants one fails closed instead of opening
	// whatever the repository named.
	"-c", "core.askpass=",
	"-c", "core.sshCommand=",
	"-c", "core.alternateRefsCommand=",
	"-c", "core.editor=false",
	"-c", "sequence.editor=false",
	"-c", "diff.external=",
	"-c", "uploadpack.packObjectsHook=",
	// Signature verification is the only thing that runs gpg.program;
	// turn the triggers off and leave the program unset.
	"-c", "merge.verifySignatures=false",
	"-c", "pull.verifySignatures=false",
	"-c", "log.showSignature=false",
	"-c", "gpg.program=",
	// Submodules are not part of the contract, and a submodule update
	// honors `submodule.<name>.update = !command` from the repo-local
	// config.  Never recurse, never fetch them.
	"-c", "submodule.recurse=false",
	"-c", "fetch.recurseSubmodules=no",
}

// envKeep is the environment allowlist: platform plumbing git and its
// subprocesses need, and nothing else — in particular no inherited
// GIT_*, no credential material, no proxy settings.  The Windows names
// are harmless surplus on Linux and vice versa.
var envKeep = []string{
	"PATH", "HOME", "TZ", "TMPDIR",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
	"SystemRoot", "SystemDrive", "ComSpec", "PATHEXT", "TEMP", "TMP",
}

// overlay is what one remote-touching invocation carries beyond the
// hardened baseline: the wire mapping as extra -c pairs, and the
// credential as GIT_CONFIG_* environment entries — env, never argv,
// because argv is world-readable in /proc.
type overlay struct {
	cfg []string // extra -c pairs, appended after the hardening list
	env []string // extra environment entries
}

// command assembles one hardened invocation.
func (r *runner) command(ctx context.Context, o overlay, args []string) *exec.Cmd {
	full := make([]string, 0, len(hardening)+len(args)+8)
	// The repository location is pinned per invocation, never discovered:
	// --work-tree beats a repo-local `core.worktree`, which would
	// otherwise relocate the tree git reads and writes to any path the
	// archivist can reach (post-M3 the agent writes .git/config, so
	// discovery would mean `checkpoint` could be aimed at, say, the
	// mounted credential).  --git-dir keeps the search from walking up
	// out of the mount.  New verifies both paths once; these flags make
	// every later invocation independent of what .git/config now says.
	full = append(full, "-C", r.dir, "--git-dir="+r.gitDir, "--work-tree="+r.dir)
	full = append(full, hardening...)
	full = append(full, "-c", "core.hooksPath="+r.hooks)
	full = append(full, o.cfg...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, r.git, full...)
	env := make([]string, 0, len(envKeep)+8)
	for _, k := range envKeep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	stamp := fmt.Sprintf("@%d +0000", r.now().Unix())
	cmd.Env = append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"LC_ALL=C",
		// No system-wide attributes file, so the only attributes are the
		// repository's own (which the config guard's refusal of filter
		// and diff drivers renders inert).
		"GIT_ATTR_NOSYSTEM=1",
		// refs/replace/* in an agent-written .git could substitute one
		// object for another, making every read — history, show_change,
		// file_at — testify about content the checkpoint never held.
		"GIT_NO_REPLACE_OBJECTS=1",
		// Note: GIT_LITERAL_PATHSPECS is deliberately NOT set.  It would
		// also disable the magic git uses internally — `stash push
		// --include-untracked` stops removing untracked files, silently —
		// and pathspec magic in an agent-supplied path is already refused
		// at the door by validPath's leading-':' rule.
		// The endpoint's token will arrive through an askpass helper this
		// runner sets deliberately (phase 3); until then no helper at all.
		"GIT_ASKPASS=",
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_DATE="+stamp,
	)
	cmd.Env = append(cmd.Env, o.env...)
	return cmd
}

// run is the shared execution core: stdout, stderr, and the exit code.
// err is non-nil only when the process could not run at all (bad
// binary, context cancellation) — a non-zero exit is a code, not an
// error, and each wrapper decides what it means.
func (r *runner) run(ctx context.Context, args []string) (stdout []byte, stderr string, code int, err error) {
	return r.runWith(ctx, overlay{}, args)
}

// runWith is run with a remote overlay applied.
func (r *runner) runWith(ctx context.Context, o overlay, args []string) (stdout []byte, stderr string, code int, err error) {
	cmd := r.command(ctx, o, args)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, "", -1, fmt.Errorf("archive: git %s: %w", args[0], ctx.Err())
	}
	if runErr != nil {
		var xe *exec.ExitError
		if !errors.As(runErr, &xe) {
			return nil, "", -1, fmt.Errorf("archive: running %s: %w", r.git, runErr)
		}
		return so.Bytes(), se.String(), xe.ExitCode(), nil
	}
	return so.Bytes(), se.String(), 0, nil
}

// exit runs git and returns trimmed stdout and the exit code.  Callers
// that read specific exit codes as answers (merge-base --is-ancestor,
// symbolic-ref --quiet) use this; everyone else wants out.
func (r *runner) exit(ctx context.Context, args ...string) (string, int, error) {
	stdout, _, code, err := r.run(ctx, args)
	if err != nil {
		return "", -1, err
	}
	return strings.TrimRight(string(stdout), "\n"), code, nil
}

// raw runs git and returns stdout byte-for-byte — for file content,
// where a trimmed trailing newline would be corruption.  A non-zero
// exit folds into an error carrying git's stderr — the actionable text.
func (r *runner) raw(ctx context.Context, args ...string) ([]byte, error) {
	return r.rawWith(ctx, overlay{}, args...)
}

// rawWith is raw with a remote overlay applied.
func (r *runner) rawWith(ctx context.Context, o overlay, args ...string) ([]byte, error) {
	stdout, stderr, code, err := r.runWith(ctx, o, args)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(string(stdout))
		}
		return nil, fmt.Errorf("archive: git %s: %s: exit status %d", args[0], msg, code)
	}
	return stdout, nil
}

// out runs git and returns trimmed stdout, with raw's error contract.
func (r *runner) out(ctx context.Context, args ...string) (string, error) {
	return r.outWith(ctx, overlay{}, args...)
}

// outWith is out with a remote overlay applied — the network-touching
// verbs' path.
func (r *runner) outWith(ctx context.Context, o overlay, args ...string) (string, error) {
	stdout, err := r.rawWith(ctx, o, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(stdout), "\n"), nil
}
