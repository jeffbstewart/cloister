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

// The repo-local config is untrusted input.  Global and system config are
// redirected to /dev/null and the archivist never reads identity from
// config, but `.git/config` itself stays where it is — and after grange
// M3 the agent writes it freely from its own container.
//
// That file is an arbitrary-code-execution surface, not merely a
// misconfiguration surface.  Confirmed against git 2.54 under the full
// hardening set:
//
//   - `filter.<any>.clean` / `.smudge` / `.process` plus a one-line
//     `.gitattributes` runs the named command on `checkpoint`'s
//     `add -A` and on every `restore`;
//   - `remote.origin.uploadpack` runs the named command on `fetch` once
//     the remote URL is a local path (and a `-c` override of that key
//     does NOT reliably win);
//   - `merge.verifySignatures` makes `sync_from_upstream` exec
//     `gpg.program`;
//   - `diff.<any>.command` / `.textconv` run on any diff-producing verb
//     that forgets --no-ext-diff/--no-textconv;
//   - `core.worktree` relocates the tree git reads and writes.
//
// Overriding with -c cannot be the answer: the driver sections are named
// by whoever writes the config, so the set of dangerous key names is not
// enumerable.  The answer is the reverse — an allowlist of the keys a
// grange clone legitimately carries, and a refusal to run git at all
// when anything else is present.  Fail closed: an unrecognized key means
// either tampering or a git version writing something new, and both want
// an operator, not a guess.
//
// Two honest limits:
//
//   - It is check-then-act.  The agent can rewrite `.git/config`
//     between the guard's read and the verb's own invocations.  The
//     guard turns a silent, always-available execution primitive into a
//     race the attacker has to win against a specific verb, and every
//     refusal is a loud, auditable event.  The structural fix is that
//     the archivist's `.git` must not be agent-writable at all (the
//     phase-3 uid split); this package cannot impose that alone.
//   - It guards `.git/config` only.  Object and ref content is the
//     agent's by design; the guard's job is the config file that turns
//     content into execution.
//
// ErrHostileConfig reports a workspace whose repo-local git config
// carries a key the archivist does not recognize.  The archivist runs no
// verb in such a workspace: the config file can name programs for git to
// execute, so an unrecognized key is treated as tampering.
var ErrHostileConfig = errors.New("archive: the workspace's git config carries an unrecognized key")

// configAllow lists the repo-local config keys a grange workspace may
// carry.  Subsectioned keys (remote.<name>.url, branch.<name>.merge) are
// matched by their section and variable with the subsection elided, so
// the map key reads "remote..url".  Keys are compared lowercased, as git
// reports them: section and variable are case-insensitive in git, only
// the subsection is not, and no entry here is subsection-sensitive.
//
// Deliberately absent, and therefore refused: every key that names a
// program (filter.*, diff.*, merge.*, gpg.*, core.hooksPath,
// core.askpass, core.sshCommand, remote.*.uploadpack, ...), every key
// that relocates the repository (core.worktree, core.bare's friends in
// extensions.*), and include.path / includeIf.* — which would pull in
// another config file and hollow out the guard.
//
// Extending this list is an operator decision: when a git version starts
// writing a new benign key, the refusal names it, and it is added here
// with a note about why it is inert.
var configAllow = map[string]bool{
	// Written by `git clone` / `git init` and describing the format of
	// the repository, not git's behavior toward the outside world.
	"core.repositoryformatversion": true,
	"core.filemode":                true,
	"core.bare":                    true,
	"core.logallrefupdates":        true,
	"core.symlinks":                true,
	"core.ignorecase":              true,
	"core.precomposeunicode":       true,
	"core.autocrlf":                true,
	"core.eol":                     true,
	"core.longpaths":               true,
	"extensions.objectformat":      true,

	// The repo-local author identity provision writes.  Harmless because
	// every checkpoint pins user.name/user.email on the command line
	// anyway; kept legible so provision's own write does not trip the
	// guard.
	"user.name":  true,
	"user.email": true,

	// The remote and the branch bookkeeping push -u writes.  Note what is
	// NOT here: remote..uploadpack, remote..receivepack, remote..proxy,
	// url..insteadOf.
	"remote..url":         true,
	"remote..fetch":       true,
	"branch..remote":      true,
	"branch..merge":       true,
	"branch..rebase":      true,
	"branch..description": true,
}

// configKeyClass folds one config key as git reports it into the form
// configAllow is keyed by: "section.variable" for a plain key,
// "section..variable" for a subsectioned one.
func configKeyClass(key string) string {
	// git reports section and variable lowercased; a subsection keeps its
	// case.  Splitting on the first and last '.' isolates the subsection
	// whatever it contains — including further dots, as in
	// remote.origin.example.com.url.
	first := strings.Index(key, ".")
	last := strings.LastIndex(key, ".")
	if first < 0 {
		return strings.ToLower(key)
	}
	section := strings.ToLower(key[:first])
	variable := strings.ToLower(key[last+1:])
	if first == last {
		return section + "." + variable
	}
	return section + ".." + variable
}

// guardConfig reads the repo-local config and refuses the workspace when
// it carries a key outside configAllow, or a fetch refspec that would
// force-update a local ref.  Every exported verb calls it before it
// touches git; the archivist would rather refuse a workspace than run
// git inside one whose config it does not recognize.
func (a *Archive) guardConfig(ctx context.Context) error {
	// --list --local reads exactly one file: .git/config.  --name-only
	// keeps values (which may name paths, and in a tampered workspace
	// arbitrary text) out of this process and out of error messages.
	out, err := a.run.raw(ctx, "config", "--list", "--local", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("archive: reading the workspace's git config: %w", err)
	}
	for _, key := range strings.Split(string(out), "\x00") {
		if key == "" {
			continue
		}
		if !configAllow[configKeyClass(key)] {
			return fmt.Errorf("%w: %s — the archivist refuses to run git in this workspace; a config key can name programs for git to execute, so an unrecognized key is treated as tampering (an operator adds a legitimately new key to configAllow)",
				ErrHostileConfig, key)
		}
	}
	return a.guardFetchRefspec(ctx)
}

// guardFetchRefspec requires every configured fetch refspec to land in
// refs/remotes/.  A clone's own refspec does exactly that
// (+refs/heads/*:refs/remotes/origin/*) and its leading '+' is harmless
// there — remote-tracking refs are a cache, not history.  A refspec
// pointed at refs/heads/ is a different animal: it lets a fetch move
// local branches, and with a '+' it moves them non-fast-forward, waiving
// the refusal that keeps published history append-only.
func (a *Archive) guardFetchRefspec(ctx context.Context) error {
	out, err := a.run.raw(ctx, "config", "--get-all", "--local", "-z", "remote."+originRemote+".fetch")
	if err != nil {
		// No such key is the common case (exit 1); nothing to guard.
		return nil
	}
	for _, spec := range strings.Split(string(out), "\x00") {
		if spec == "" {
			continue
		}
		_, dst, ok := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if !ok || !strings.HasPrefix(dst, "refs/remotes/") {
			return fmt.Errorf("%w: remote.%s.fetch is %q, which does not land in refs/remotes/; a fetch refspec that writes local branches can rewrite history the archivist has published",
				ErrHostileConfig, originRemote, spec)
		}
	}
	return nil
}
