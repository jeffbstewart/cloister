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

// Package manifest loads and validates the per-project agent-harness.yaml
// contract.  The project owner places the file at the root of the project's
// workspace (DefaultPath); it is the ENTIRE action menu: run arrays are exec
// argv (never a shell string), and agent-suppliable params are validated
// against anchored RE2 patterns — rejected, never sanitized.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jeffbstewart/cloister/internal/digest"
)

const (
	// SupportedVersion is the only harness contract version this server accepts.
	SupportedVersion = 1
	// MaxActions bounds the menu size.
	MaxActions = 16
	// MaxTimeout is the server-side hard cap on any action timeout.
	MaxTimeout = 60 * time.Minute
	// DefaultPath is the manifest's filename at the workspace root.
	DefaultPath = "agent-harness.yaml"
)

var (
	actionNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	paramNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	envNameRE    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// reservedNames are tool names an action may not shadow: the built-ins on
// this server, plus the names the scribe (workspace write path) and the
// scholar (web research) use.  Reserving them means a manifest cannot
// collide with — or spoof — those tools.
var reservedNames = map[string]bool{
	// built-in on this server
	"get_log":      true,
	"harness_info": true,
	// scribe: workspace file read/write path
	"read_file":   true,
	"write_file":  true,
	"apply_patch": true,
	// scholar: web retrieval + search
	"fetch":      true,
	"search":     true,
	"web_fetch":  true,
	"web_search": true,
}

// Duration is a time.Duration that unmarshals from Go duration syntax ("15m").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("timeout must be a duration string like \"15m\": %v", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("timeout %q: %v", s, err)
	}
	*d = Duration(v)
	return nil
}

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Manifest is the parsed, validated agent-harness.yaml.
type Manifest struct {
	Harness   int                `yaml:"harness"`
	Toolchain string             `yaml:"toolchain"`
	Actions   map[string]*Action `yaml:"actions"`
	Caches    []Cache            `yaml:"caches"`
}

// Action is one entry on the fixed menu.
type Action struct {
	Description string            `yaml:"description"`
	Run         []string          `yaml:"run"`
	Timeout     Duration          `yaml:"timeout"`
	Parser      string            `yaml:"parser"`
	Params      map[string]*Param `yaml:"params"`
}

// Param is the only agent-suppliable input: passed as two argv elements
// (flag, value) appended to Run, and only if the value matches Pattern.
type Param struct {
	Description string `yaml:"description"`
	Flag        string `yaml:"flag"`
	Pattern     string `yaml:"pattern"`

	re *regexp.Regexp // compiled full-match pattern, set by validate
}

// Cache declares a named volume the toolchain caches into.
type Cache struct {
	Volume string   `yaml:"volume"`
	Env    string   `yaml:"env"`
	Path   string   `yaml:"path"`
	Warmup []string `yaml:"warmup"`
}

// Load reads and validates the manifest at path.  A missing file surfaces as
// fs.ErrNotExist so callers can degrade gracefully.
func Load(path, toolchainID string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, toolchainID)
}

// Parse decodes and validates manifest bytes.  Unknown keys at any level are
// an error — fail loud, not loose.
func Parse(data []byte, toolchainID string) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("agent-harness.yaml: empty manifest")
		}
		return nil, fmt.Errorf("agent-harness.yaml: %v", err)
	}
	if err := m.validate(toolchainID); err != nil {
		return nil, fmt.Errorf("agent-harness.yaml: %v", err)
	}
	return &m, nil
}

func (m *Manifest) validate(toolchainID string) error {
	if m.Harness != SupportedVersion {
		return fmt.Errorf("harness: %d unsupported; this server supports only %d", m.Harness, SupportedVersion)
	}
	if m.Toolchain != toolchainID {
		return fmt.Errorf("toolchain %q does not match this builder image (%q)", m.Toolchain, toolchainID)
	}
	if len(m.Actions) == 0 {
		return errors.New("actions: at least one action required")
	}
	if len(m.Actions) > MaxActions {
		return fmt.Errorf("actions: %d exceeds the maximum of %d", len(m.Actions), MaxActions)
	}
	for name, a := range m.Actions {
		if a == nil {
			return fmt.Errorf("action %q: empty definition", name)
		}
		if !actionNameRE.MatchString(name) {
			return fmt.Errorf("action %q: name must match %s", name, actionNameRE)
		}
		if reservedNames[name] {
			return fmt.Errorf("action %q: name is reserved for a built-in tool", name)
		}
		if len(a.Run) == 0 || a.Run[0] == "" {
			return fmt.Errorf("action %q: run must be a non-empty exec array", name)
		}
		if a.Timeout <= 0 {
			return fmt.Errorf("action %q: timeout required", name)
		}
		if a.Timeout.Duration() > MaxTimeout {
			return fmt.Errorf("action %q: timeout %s exceeds the server cap of %s",
				name, a.Timeout.Duration(), MaxTimeout)
		}
		if !digest.Known(a.Parser) {
			return fmt.Errorf("action %q: unknown parser %q (want gradle, gotest, or generic)", name, a.Parser)
		}
		for pname, p := range a.Params {
			if p == nil {
				return fmt.Errorf("action %q param %q: empty definition", name, pname)
			}
			if !paramNameRE.MatchString(pname) {
				return fmt.Errorf("action %q param %q: name must match %s", name, pname, paramNameRE)
			}
			if p.Flag == "" {
				return fmt.Errorf("action %q param %q: flag required", name, pname)
			}
			if p.Pattern == "" {
				return fmt.Errorf("action %q param %q: pattern required", name, pname)
			}
			// Wrap so a match is always a full match regardless of anchors
			// in the manifest.  RE2: no backtracking surprises.
			re, err := regexp.Compile(`\A(?:` + p.Pattern + `)\z`)
			if err != nil {
				return fmt.Errorf("action %q param %q: bad pattern: %v", name, pname, err)
			}
			p.re = re
		}
	}
	for i, c := range m.Caches {
		if c.Env != "" && !envNameRE.MatchString(c.Env) {
			return fmt.Errorf("caches[%d]: bad env var name %q", i, c.Env)
		}
	}
	return nil
}

// ParserName returns the effective parser, defaulting to "generic".
func (a *Action) ParserName() string {
	if a.Parser == "" {
		return "generic"
	}
	return a.Parser
}

// Args validates agent-supplied params and returns the argv elements to
// append to Run, in deterministic (sorted) order.  A failing param rejects
// the whole call; values are never modified.
func (a *Action) Args(params map[string]string) ([]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(params))
	for n := range params {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		p, ok := a.Params[n]
		if !ok {
			return nil, fmt.Errorf("unknown param %q", n)
		}
		v := params[n]
		if !p.re.MatchString(v) {
			return nil, fmt.Errorf("param %q: value %q does not match pattern %q", n, v, p.Pattern)
		}
		out = append(out, p.Flag, v)
	}
	return out, nil
}
