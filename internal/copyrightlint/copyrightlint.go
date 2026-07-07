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

// Package copyrightlint checks that source files carry a copyright header,
// that the owner's header is current in every file a change touches, and
// that no third-party copyright enters the repository unobserved.
//
// The rules, driven by a strict YAML policy:
//   - Every covered file (not excluded, not binary, not empty) must contain
//     a copyright line in its head.
//   - A file bearing the owner's copyright must also carry the Apache-2.0
//     license reference, its year must be canonical ("YYYY" or
//     "YYYY-YYYY"), and — for files in the changed set — the latest year
//     must equal the year of submission.
//   - A copyright line naming anyone else is rejected unless the path
//     matches a third_party pattern; allowlisted third-party files satisfy
//     presence with any copyright line and are never year-checked.
package copyrightlint

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// headBytes bounds how much of a file is scanned for its header.  Headers
// live at the top (after at most a shebang or template directive), so 4 KiB
// is generous.
const headBytes = 4096

// Problem names one kind of header defect.
type Problem string

const (
	ProblemMissingHeader  Problem = "missing_header"    // no copyright line at all
	ProblemMissingLicense Problem = "missing_license"   // owner's copyright without the Apache reference
	ProblemMalformedYear  Problem = "malformed_year"    // owner's line present but years not canonical
	ProblemStaleYear      Problem = "stale_year"        // changed file whose latest year is not the submission year
	ProblemForeignHolder  Problem = "foreign_copyright" // someone else's copyright, not allowlisted
)

// Finding is one defect in one file.
type Finding struct {
	Path    string
	Problem Problem
	Detail  string // human-readable specifics
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %s: %s", f.Path, f.Problem, f.Detail)
}

// Config is the checked-in copyright policy.
type Config struct {
	// Owner is the copyright holder whose headers are enforced and
	// year-checked, e.g. "Jeffrey B. Stewart".
	Owner string `yaml:"owner"`
	// Exclude lists glob patterns (see Match) for paths that need no
	// header at all: docs, generated metadata, fixtures.
	Exclude []string `yaml:"exclude"`
	// ThirdParty lists glob patterns for paths allowed to bear another
	// holder's copyright.  Such files satisfy presence with any copyright
	// line and are never year-checked.
	ThirdParty []string `yaml:"third_party"`
}

// ParseConfig decodes a policy, failing closed: unknown keys and a missing
// owner are errors.
func ParseConfig(data []byte) (Config, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("copyright policy: %w", err)
	}
	if cfg.Owner == "" {
		return Config{}, fmt.Errorf("copyright policy: owner is required")
	}
	for _, p := range append(append([]string{}, cfg.Exclude...), cfg.ThirdParty...) {
		for _, seg := range strings.Split(p, "/") {
			if _, err := path.Match(seg, ""); err != nil {
				return Config{}, fmt.Errorf("copyright policy: bad pattern %q: %w", p, err)
			}
		}
	}
	return cfg, nil
}

// Match reports whether a slash-separated relative path matches a pattern.
// Within a segment, path.Match syntax applies; a "**" segment matches any
// number of segments, including none.  Patterns are anchored: "*.md"
// matches only at the root — use "**/*.md" for any depth.
func Match(pattern, p string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

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
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}

func matchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if Match(pat, p) {
			return true
		}
	}
	return false
}

// anyCopyright detects a copyright line of any shape or holder.
var anyCopyright = regexp.MustCompile(`(?i)\bcopyright\b`)

// licenseRef must accompany the owner's copyright line in every owned file.
const licenseRef = "Licensed under the Apache License, Version 2.0"

// Check applies the policy to the named files in fsys.  paths are
// slash-separated and relative to fsys; changed is the subset touched by the
// submission under review (year currency is enforced only there); year is
// the submission year (UTC — injected, never read from a clock in here).
// Binary and empty files are skipped.  Findings come back sorted by path.
func Check(cfg Config, fsys fs.FS, paths []string, changed map[string]bool, year int) ([]Finding, error) {
	ownedYearsRE := regexp.MustCompile(`Copyright\s+(\d{4})(?:-(\d{4}))?\s+` + regexp.QuoteMeta(cfg.Owner))
	ownedLooseRE := regexp.MustCompile(`(?i)copyright\s+[^A-Za-z]*` + regexp.QuoteMeta(cfg.Owner))

	var findings []Finding
	add := func(p string, problem Problem, detail string) {
		findings = append(findings, Finding{Path: p, Problem: problem, Detail: detail})
	}

	for _, p := range paths {
		if matchAny(cfg.Exclude, p) {
			continue
		}
		head, skip, err := readHead(fsys, p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if skip {
			continue
		}

		hasAny := anyCopyright.MatchString(head)
		owned := ownedYearsRE.FindStringSubmatch(head)
		ownedLoose := ownedLooseRE.MatchString(head)
		thirdParty := matchAny(cfg.ThirdParty, p)

		if !hasAny {
			add(p, ProblemMissingHeader, "no copyright line in the file head")
			continue
		}
		if hasAny && !ownedLoose && !thirdParty {
			add(p, ProblemForeignHolder, fmt.Sprintf("copyright holder is not %q and the path matches no third_party pattern", cfg.Owner))
			continue
		}
		if !ownedLoose {
			continue // allowlisted third party: presence satisfied, never year-checked
		}

		// The owner's copyright: license reference, canonical years, currency.
		if !strings.Contains(head, licenseRef) {
			add(p, ProblemMissingLicense, "owner's copyright without the Apache-2.0 license reference")
		}
		if owned == nil {
			add(p, ProblemMalformedYear, `years must be "YYYY" or "YYYY-YYYY"`)
			continue
		}
		first, _ := strconv.Atoi(owned[1])
		latest := first
		if owned[2] != "" {
			latest, _ = strconv.Atoi(owned[2])
			if latest <= first {
				add(p, ProblemMalformedYear, fmt.Sprintf("year range %d-%d does not ascend", first, latest))
				continue
			}
		}
		if changed[p] && latest != year {
			add(p, ProblemStaleYear, fmt.Sprintf("file is modified in %d but the header's latest year is %d", year, latest))
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Problem < findings[j].Problem
	})
	return findings, nil
}

// readHead returns the first headBytes of the file as a string, and whether
// the file should be skipped entirely (empty, or binary — a NUL byte in the
// head — since neither can carry a text header).
func readHead(fsys fs.FS, name string) (head string, skip bool, err error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	buf := make([]byte, headBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false, err
	}
	if n == 0 {
		return "", true, nil
	}
	if strings.IndexByte(string(buf[:n]), 0) >= 0 {
		return "", true, nil
	}
	return string(buf[:n]), false, nil
}
