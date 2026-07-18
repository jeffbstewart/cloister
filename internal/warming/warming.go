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

// Package warming gates builder actions on the toolchain's offline
// dependency cache having been primed through the airlock
// (docs/toolchains.md).
//
// The contract: a toolchain image that requires warming bakes an
// instructions file (DefaultInstructionsPath) whose CONTENT is that
// toolchain's operator how-to.  A successful airlock run records a marker
// in the per-user cache home via Mark (the airlock script calls
// `builder -mark-warmed`).  While the instructions file exists and the
// marker does not, Check fails with those instructions — so an unwarmed
// cell refuses the first build with a fix, instead of dying inside it
// with a network-shaped mystery.
//
// Scope: the marker means "the airlock has warmed this toolchain into
// this cache home at least once" — per user and toolchain, not per
// project.  A second project with unwarmed dependencies still fails
// during offline resolution, exactly as before; the marker cleans up the
// never-warmed case only.
package warming

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultInstructionsPath is where a toolchain image bakes its warming
// instructions.  A toolchain that needs no warming simply ships no file.
const DefaultInstructionsPath = "/etc/cloister-worker/warming"

// Config locates the pieces of the warming handshake.
type Config struct {
	// InstructionsPath is the toolchain-baked how-to file; its absence
	// means this toolchain requires no warming and Check always passes.
	InstructionsPath string
	// CacheHome is the per-user cache home the marker lives under — the
	// builder's HOME, on the BUILD_HOME bind, so the marker survives
	// restarts alongside the caches it vouches for.
	CacheHome string
	// ToolchainID names the marker file, so toolchains sharing one cache
	// home warm independently.
	ToolchainID string
}

// MarkerPath is where a completed warm is recorded.
func (c Config) MarkerPath() string {
	return filepath.Join(c.CacheHome, ".cloister-warmed", c.ToolchainID)
}

// validate rejects a config that cannot name a marker.  The toolchain id
// becomes a filename, so a path separator inside it would silently change
// which file the handshake reads.
func (c Config) validate() error {
	if c.CacheHome == "" {
		return errors.New("warming: CacheHome is required")
	}
	if c.ToolchainID == "" {
		return errors.New("warming: ToolchainID is required")
	}
	if strings.ContainsAny(c.ToolchainID, `/\`) {
		return fmt.Errorf("warming: toolchain id %q must not contain a path separator", c.ToolchainID)
	}
	return nil
}

// Check returns nil when this toolchain needs no warming or the marker
// exists.  Otherwise it returns an error carrying the toolchain's baked
// instructions, fit to hand straight back to the caller.  It fails
// closed: an unreadable instructions file or marker is a refusal, not a
// pass.
func (c Config) Check() error {
	instructions, err := os.ReadFile(c.InstructionsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // this toolchain requires no warming
	}
	if err != nil {
		return fmt.Errorf("warming: read instructions %s: %w", c.InstructionsPath, err)
	}
	if err := c.validate(); err != nil {
		return err
	}
	switch _, err := os.Stat(c.MarkerPath()); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("toolchain %s has not been warmed for offline builds (marker %s is missing).\n\n%s",
			c.ToolchainID, c.MarkerPath(), strings.TrimSpace(string(instructions)))
	default:
		return fmt.Errorf("warming: stat marker %s: %w", c.MarkerPath(), err)
	}
}

// Mark records a completed warm and returns the marker path.  It is
// idempotent: re-warming an already-marked toolchain rewrites the marker.
func (c Config) Mark() (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	p := c.MarkerPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("warming: %w", err)
	}
	// Content is informational; the handshake reads only presence (the
	// file's mtime is the when).
	if err := os.WriteFile(p, []byte("warmed: "+c.ToolchainID+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("warming: %w", err)
	}
	return p, nil
}
