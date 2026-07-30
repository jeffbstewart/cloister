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
//   - disables global and system config (the repo-local file remains —
//     it is archivist-written until grange M3 — and the dangerous keys
//     it could carry are overridden per invocation below);
//   - runs with a scrubbed environment: an allowlist of platform
//     plumbing, never the parent's GIT_* or credential variables;
//   - pins author/committer dates from the injected clock, so recorded
//     times are the archivist's testimony, not git's wall clock.
//
// Diff-producing verbs additionally pass --no-ext-diff/--no-textconv so
// config-named external commands never run; ssh/git/ext transports are
// refused outright (https for the relays, plain paths for test rigs).
type runner struct {
	git   string
	dir   string // the worktree root; every command runs -C here
	hooks string // the empty hooks directory
	now   func() time.Time
}

// hardening is prepended to every invocation.  -c overrides beat the
// repo-local config, which is the only config file left readable.
var hardening = []string{
	"-c", "credential.helper=",
	"-c", "core.fsmonitor=false",
	"-c", "gc.auto=0",
	"-c", "protocol.ssh.allow=never",
	"-c", "protocol.git.allow=never",
	"-c", "protocol.ext.allow=never",
	"-c", "commit.gpgsign=false",
	"-c", "tag.gpgsign=false",
	"-c", "color.ui=false",
	"-c", "advice.detachedHead=false",
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

// command assembles one hardened invocation.
func (r *runner) command(ctx context.Context, args []string) *exec.Cmd {
	full := make([]string, 0, len(hardening)+len(args)+4)
	full = append(full, "-C", r.dir)
	full = append(full, hardening...)
	full = append(full, "-c", "core.hooksPath="+r.hooks)
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
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_DATE="+stamp,
	)
	return cmd
}

// exit runs git and returns trimmed stdout and the exit code; err is
// non-nil only when the process could not run at all (bad binary,
// context cancellation).  Callers that read specific exit codes as
// answers (merge-base --is-ancestor, symbolic-ref --quiet) use this;
// everyone else wants out.
func (r *runner) exit(ctx context.Context, args ...string) (string, int, error) {
	cmd := r.command(ctx, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", -1, fmt.Errorf("archive: git %s: %w", args[0], ctx.Err())
	}
	if err != nil {
		var xe *exec.ExitError
		if !errors.As(err, &xe) {
			return "", -1, fmt.Errorf("archive: running %s: %w", r.git, err)
		}
		return strings.TrimRight(stdout.String(), "\n"), xe.ExitCode(), nil
	}
	return strings.TrimRight(stdout.String(), "\n"), 0, nil
}

// raw runs git and returns stdout byte-for-byte — for file content,
// where a trimmed trailing newline would be corruption.
func (r *runner) raw(ctx context.Context, args ...string) ([]byte, error) {
	cmd := r.command(ctx, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("archive: git %s: %w", args[0], ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("archive: git %s: %s: %w", args[0], msg, err)
	}
	return stdout.Bytes(), nil
}

// out runs git and returns trimmed stdout, folding a non-zero exit into
// an error that carries git's stderr — the actionable text.
func (r *runner) out(ctx context.Context, args ...string) (string, error) {
	cmd := r.command(ctx, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("archive: git %s: %w", args[0], ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("archive: git %s: %s: %w", args[0], msg, err)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}
