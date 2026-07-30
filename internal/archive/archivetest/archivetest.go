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

// Package archivetest is the git plumbing shared by the test rigs that
// drive an Archive against real repositories: raw git execution outside
// the code under test, and a seeded bare-origin + workspace-clone pair.
// It stays free of any archive import so every layer above the engine
// can use it.
package archivetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RequireGit skips the test when no git binary is available.
func RequireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

// GitRun runs raw git for rig setup and independent verification — NOT
// the hardened runner, so tests never assume the code under test.  The
// host's config is suppressed the same way the runner suppresses it.
func GitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{
		"-c", "user.name=Seeder",
		"-c", "user.email=seeder@cloister.test",
		"-c", "protocol.file.allow=always",
		"-c", "commit.gpgsign=false",
	}
	if dir != "" {
		base = append([]string{"-C", dir}, base...)
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rig: git %v: %v\n%s", args, err, out)
	}
	return strings.TrimRight(string(out), "\n")
}

// Seed builds the fake remote under tmp: a bare origin on main holding
// one commit (README.md + keep.txt), and a workspace clone of it.  It
// returns the origin and workspace paths; git availability is checked
// (and the test skipped) first.
func Seed(t *testing.T, tmp string) (origin, workspace string) {
	t.Helper()
	RequireGit(t)
	origin = filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")
	GitRun(t, "", "init", "--bare", "-b", "main", origin)
	GitRun(t, "", "init", "-b", "main", seed)
	for name, content := range map[string]string{"README.md": "hello\n", "keep.txt": "constant\n"} {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	GitRun(t, seed, "add", "-A")
	GitRun(t, seed, "commit", "-m", "seed")
	GitRun(t, seed, "push", origin, "main:main")

	workspace = filepath.Join(tmp, "ws")
	GitRun(t, "", "clone", origin, workspace)
	return origin, workspace
}
