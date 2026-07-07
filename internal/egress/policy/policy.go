// Package policy is the egress subsystem's operator-owned configuration
// leaf: the fail-closed policy file (LoadPolicy — every field required, no
// code-side defaults), the never-extract deny-list engine, internal-host
// hygiene refusal, and the built-in search-engine-results deny set.  It is a
// leaf so the core egress package and its providers can all import it
// without cycles.
package policy

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Engine names the search backend.
type Engine string

const (
	EngineKagi  Engine = "kagi"
	EngineBrave Engine = "brave"
)

// MaxResultCount is the hard ceiling on web_search results.  This is a
// code-enforced invariant, not a tunable — distinct from the policy fields,
// which the operator MUST set explicitly (there are deliberately no defaults).
const MaxResultCount = 10

// Duration is a time.Duration parsed from a YAML duration STRING (e.g. "30s",
// "2m") so a policy field carries its unit explicitly — never a bare number
// whose unit survives only in the field name.
type Duration time.Duration

// UnmarshalYAML parses a duration string via time.ParseDuration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like %q: %w", "30s", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the standard-library time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Policy is the operator-owned egress configuration, mounted read-only.  Every
// field is REQUIRED: LoadPolicy refuses a file that omits any of them.
// There are no code-side defaults — an operator's leash must be explicit, not
// inherited from a constant they never saw.
type Policy struct {
	Search struct {
		Engine   Engine `yaml:"engine"`   // kagi | brave
		DailyCap int    `yaml:"dailyCap"` // search queries/UTC-day; must be > 0
		// DenySearchEnginePages activates the code-maintained SERP deny set
		// (searchengines.go).  A *bool so "unset" is distinguishable from false —
		// the operator must choose explicitly, like every other field.
		DenySearchEnginePages *bool `yaml:"denySearchEnginePages"`
	} `yaml:"search"`
	Extract struct {
		DailyCap int         `yaml:"dailyCap"` // extract calls/UTC-day; must be > 0
		Deny     []DenyEntry `yaml:"deny"`     // never-extract hosts; must list >= 1
	} `yaml:"extract"`
	Limits struct {
		MaxResponseBytes int64    `yaml:"maxResponseBytes"` // must be > 0
		Timeout          Duration `yaml:"timeout"`          // e.g. "30s"; must be > 0
	} `yaml:"limits"`
}

// DenyEntry is one never-extract host rule: an exact host or a single leading
// "*." label wildcard, optionally narrowed by a path prefix.  Host is required;
// PathPrefix is the one optional field.
type DenyEntry struct {
	Host       string `yaml:"host"`
	PathPrefix string `yaml:"pathPrefix,omitempty"`
}

// LoadPolicy reads and validates the policy file.  It is FAIL-CLOSED: a missing
// or unparseable file, an unknown key, or any unset/invalid field is an error —
// the caller aborts startup rather than run on a leash it did not choose.
func LoadPolicy(path string) (*Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("egress: read policy %q: %w", path, err)
	}
	var p Policy
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a typo'd key is a fail-closed error, not a silent no-op
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("egress: parse policy %q: %w", path, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("egress: invalid policy %q: %w", path, err)
	}
	return &p, nil
}

// validate insists that every field is populated.  A zero value is treated as
// "unset" and rejected — the operator cannot fall into a permissive default by
// forgetting a line. (An operator who genuinely wants a very high cap sets a
// very high number; there is no "0 = unlimited" footgun.)
func (p *Policy) validate() error {
	switch p.Search.Engine {
	case EngineKagi, EngineBrave:
	case "":
		return fmt.Errorf("search.engine is required (kagi or brave)")
	default:
		return fmt.Errorf("search.engine %q: want kagi or brave", p.Search.Engine)
	}
	if p.Search.DailyCap <= 0 {
		return fmt.Errorf("search.dailyCap is required and must be > 0")
	}
	if p.Search.DenySearchEnginePages == nil {
		return fmt.Errorf("search.denySearchEnginePages is required (true or false)")
	}
	if p.Extract.DailyCap <= 0 {
		return fmt.Errorf("extract.dailyCap is required and must be > 0")
	}
	if p.Limits.MaxResponseBytes <= 0 {
		return fmt.Errorf("limits.maxResponseBytes is required and must be > 0")
	}
	if p.Limits.Timeout <= 0 {
		return fmt.Errorf("limits.timeout is required and must be > 0 (e.g. \"30s\")")
	}
	if len(p.Extract.Deny) == 0 && !*p.Search.DenySearchEnginePages {
		return fmt.Errorf("extract.deny must list at least one host, or enable search.denySearchEnginePages")
	}
	for i, d := range p.Extract.Deny {
		if d.Host == "" {
			return fmt.Errorf("extract.deny[%d]: host is required", i)
		}
		if err := validateDenyHost(d.Host); err != nil {
			return fmt.Errorf("extract.deny[%d] host %q: %w", i, d.Host, err)
		}
	}
	return nil
}
