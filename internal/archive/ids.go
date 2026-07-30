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
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The identifier types below follow the repo's domain-ID rule: a struct
// wrapping a private string, obtainable only through a validating
// parser, so "this value exists" means "this value was validated".
// Every parser doubles as the argv-injection guard: nothing that parses
// may start with '-', so an identifier can never be read as a git flag.

// BranchName names a line of work.  The accepted alphabet is a strict
// subset of what git's check-ref-format allows — enough for real branch
// names (agent/fix-thing, release/1.2) while excluding every shell and
// revision-syntax metacharacter.
type BranchName struct {
	s string
}

var branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ParseBranchName validates an untrusted branch name.
func ParseBranchName(s string) (BranchName, error) {
	switch {
	case s == "" || len(s) > 200:
		return BranchName{}, fmt.Errorf("invalid branch name %q: empty or over 200 bytes", s)
	case !branchRE.MatchString(s):
		return BranchName{}, fmt.Errorf("invalid branch name %q: allowed are letters, digits, '.', '_', '/', '-', not starting with '.' or '-'", s)
	case strings.Contains(s, ".."), strings.Contains(s, "//"), strings.Contains(s, "/."),
		strings.HasSuffix(s, "/"), strings.HasSuffix(s, "."), strings.HasSuffix(s, ".lock"):
		return BranchName{}, fmt.Errorf("invalid branch name %q: '..', '//', '/.', and trailing '/', '.', '.lock' are refused", s)
	}
	return BranchName{s: s}, nil
}

// String returns the validated name ("" for the zero value).
func (b BranchName) String() string { return b.s }

// IsZero reports whether b is the "no branch" zero value.
func (b BranchName) IsZero() bool { return b.s == "" }

// CheckpointID identifies one recorded checkpoint — in git, a commit
// hash, full or abbreviated to no fewer than 4 hex digits.
type CheckpointID struct {
	s string
}

var checkpointRE = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// ParseCheckpointID validates an untrusted checkpoint id.  Uppercase is
// rejected, not folded: one canonical form keeps ids comparable as
// strings.
func ParseCheckpointID(s string) (CheckpointID, error) {
	if !checkpointRE.MatchString(s) {
		return CheckpointID{}, fmt.Errorf("invalid checkpoint id %q: want 4-64 lowercase hex digits", s)
	}
	return CheckpointID{s: s}, nil
}

// String returns the validated id ("" for the zero value).
func (c CheckpointID) String() string { return c.s }

// IsZero reports whether c is the "no checkpoint" zero value.
func (c CheckpointID) IsZero() bool { return c.s == "" }

// Ref is a read-only revision designation for history and file_at: a
// branch name, a checkpoint id, HEAD, a remote-tracking name like
// origin/main, with optional ~N/^ suffixes.  Ranges ('..'), reflog
// syntax ('@{...}'), and ':' are refused — the verbs that take a Ref
// answer questions about one revision, not spans, and ':' would splice
// into git's rev:path syntax.
type Ref struct {
	s string
}

var refRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*(?:[~^][0-9]*)*$`)

// ParseRef validates an untrusted revision designation.
func ParseRef(s string) (Ref, error) {
	if s == "" || len(s) > 200 {
		return Ref{}, fmt.Errorf("invalid revision %q: empty or over 200 bytes", s)
	}
	if strings.Contains(s, "..") {
		return Ref{}, fmt.Errorf("invalid revision %q: ranges are not accepted here", s)
	}
	if !refRE.MatchString(s) {
		return Ref{}, fmt.Errorf("invalid revision %q: want a branch name, checkpoint id, or HEAD, optionally with ~N/^ suffixes", s)
	}
	return Ref{s: s}, nil
}

// String returns the validated designation ("" for the zero value).
func (r Ref) String() string { return r.s }

// IsZero reports whether r is the "no revision" zero value.
func (r Ref) IsZero() bool { return r.s == "" }

// Ref converts the already-validated branch name into a revision
// designation.
func (b BranchName) Ref() Ref { return Ref{s: b.s} }

// Ref converts the already-validated checkpoint id into a revision
// designation.
func (c CheckpointID) Ref() Ref { return Ref{s: c.s} }

// driveRE spots an absolute Windows path ("C:...") handed to validPath.
var driveRE = regexp.MustCompile(`^[A-Za-z]:`)

// validPath vets a workspace-relative path used as a pathspec or in
// rev:path syntax: forward slashes only, relative, no '..', no leading
// '-', no control bytes.  Less a correctness filter than an injection
// guard — git decides whether the path exists.
//
// Paths are NOT restricted to ASCII: a repository may legitimately hold
// UTF-8 filenames, and refusing them would be a correctness bug dressed
// as security.  What is refused is every byte that could confuse a
// display, a log line, or a NUL-delimited parse: the C0 and C1 control
// ranges and DEL, plus text that is not valid UTF-8 at all.
func validPath(s string) error {
	switch {
	case s == "" || len(s) > 4096:
		return fmt.Errorf("invalid path %q: empty or over 4096 bytes", s)
	case strings.HasPrefix(s, "-"):
		return fmt.Errorf("invalid path %q: may not start with '-'", s)
	case strings.Contains(s, "\\"):
		return fmt.Errorf("invalid path %q: use forward slashes", s)
	case strings.HasPrefix(s, "/"), driveRE.MatchString(s):
		return fmt.Errorf("invalid path %q: must be workspace-relative", s)
	case s == ".." || strings.HasPrefix(s, "../") || strings.HasSuffix(s, "/..") || strings.Contains(s, "/../"):
		return fmt.Errorf("invalid path %q: '..' is refused", s)
	case !utf8.ValidString(s):
		return fmt.Errorf("invalid path %q: not valid UTF-8", s)
	case hasControl(s):
		return fmt.Errorf("invalid path %q: control characters are refused", s)
	case strings.HasPrefix(s, ":"):
		// A leading ':' is git pathspec-magic syntax (":(glob)", ":!").
		return fmt.Errorf("invalid path %q: pathspec magic is refused", s)
	}
	return nil
}

// hasControl reports whether s carries a C0 control byte, DEL, or a C1
// control rune.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

// messageMax bounds a checkpoint message.  A checkpoint subject and a
// paragraph of reasoning fit; a megabyte of model output does not, and
// the message travels into a commit object the archivist testifies about.
const messageMax = 2000

// attributionRE spots the git trailers that assign credit or sign-off.
// The agent may describe its work; it may not attribute it.  Identity is
// pinned from the endpoint table, and a message carrying
// "Signed-off-by: <a human>" or "Co-authored-by: <a human>" would put
// words in that human's mouth in the published history — the same
// spoofing the pinned author/committer exists to prevent, one field over.
var attributionRE = regexp.MustCompile(`(?im)^[ \t]*(signed-off-by|co-authored-by|acked-by|reviewed-by|tested-by|reported-by)[ \t]*:`)

// validMessage vets an agent-supplied checkpoint message: printable
// ASCII plus newline, bounded, non-blank, no attribution trailers.
//
// ASCII rather than UTF-8: a message is free text the agent composes, it
// lands in a commit object that operators, the corrector, and the forge
// all re-render, and there is no legitimate need for direction-override
// marks, zero-width joiners, or homoglyphs in a machine's checkpoint
// message.  Refusing the whole non-ASCII range is one rule with no
// interesting edges, where "allow UTF-8 but not confusables" is a
// permanent maintenance burden.
func validMessage(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("a message is required")
	}
	if len(s) > messageMax {
		return fmt.Errorf("message is %d bytes, over the %d-byte limit", len(s), messageMax)
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '\n' && (c < 0x20 || c > 0x7e) {
			return fmt.Errorf("message byte %d (%#x) is not printable ASCII; checkpoint messages are ASCII text plus newlines", i, c)
		}
	}
	if m := attributionRE.FindString(s); m != "" {
		return fmt.Errorf("message carries the attribution trailer %q; the archivist pins who a checkpoint is credited to and the agent may not add credit for anyone", strings.TrimSpace(m))
	}
	return nil
}

// validIdentity vets the bot identity from the endpoint table.  It is
// operator config rather than agent input, but it is interpolated into
// `-c user.name=` and lands in a commit's ident line, where a newline or
// an angle bracket would corrupt the object git writes — so it is
// checked with the same rule as a message: printable ASCII, no control
// bytes, no ident metacharacters.
func validIdentity(id Identity) error {
	if id.Name == "" || id.Email == "" {
		return fmt.Errorf("archive: identity requires both name and email")
	}
	for _, f := range []struct{ what, val string }{{"name", id.Name}, {"email", id.Email}} {
		if len(f.val) > 200 {
			return fmt.Errorf("archive: identity %s is over 200 bytes", f.what)
		}
		for i := 0; i < len(f.val); i++ {
			if c := f.val[i]; c < 0x20 || c > 0x7e {
				return fmt.Errorf("archive: identity %s byte %d (%#x) is not printable ASCII", f.what, i, c)
			}
		}
		if strings.ContainsAny(f.val, "<>\n") {
			return fmt.Errorf("archive: identity %s may not contain '<' or '>'", f.what)
		}
	}
	if !strings.Contains(id.Email, "@") {
		return fmt.Errorf("archive: identity email %q is not an address", id.Email)
	}
	return nil
}
