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

// copyright-lint verifies the repository's copyright headers against the
// embedded policy (copyright.yaml, baked in via go:embed): every covered
// file carries a header, the owner's headers are current in every file the
// submission touches, and no third-party copyright enters unobserved.
//
// Usage:
//
//	copyright-lint [-]
//
// The tracked file set comes from `git ls-files`.  With "-", newline-
// separated paths of the files changed by the submission under review are
// read from stdin (the pre-commit hook pipes the staged names; CI pipes
// the PR diff names); year currency is enforced only on those.
package main

import (
	"bufio"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/copyrightlint"
)

//go:embed copyright.yaml
var policy []byte

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "copyright-lint:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "-") {
		return fmt.Errorf("usage: copyright-lint [-]")
	}

	cfg, err := copyrightlint.ParseConfig(policy)
	if err != nil {
		return err
	}

	tracked, err := gitLsFiles()
	if err != nil {
		return err
	}

	changed := map[string]bool{}
	if len(args) == 1 {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if p := strings.TrimSpace(sc.Text()); p != "" {
				changed[p] = true
			}
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("read changed paths: %w", err)
		}
	}

	// The submission year, UTC per house convention.
	year := time.Now().UTC().Year()

	findings, err := copyrightlint.Check(cfg, os.DirFS("."), tracked, changed, year)
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, f)
		}
		return fmt.Errorf("%d finding(s)", len(findings))
	}
	fmt.Printf("copyright-lint: %d tracked files OK (%d year-checked)\n", len(tracked), len(changed))
	return nil
}

// gitLsFiles returns the repository's tracked paths, slash-separated,
// skipping paths gone from the worktree (staged deletes) so Check never
// opens a vanished file.
func gitLsFiles() ([]string, error) {
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		if _, statErr := os.Lstat(p); statErr != nil {
			continue
		}
		paths = append(paths, p)
	}
	return paths, nil
}
