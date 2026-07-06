// Package composelint statically checks the cell-stack compose file
// (docker/ai-workers.yaml) for the scholar containment invariants: the scholar
// holds no `egress` network and no route to builder/scribe/workspace, every
// network it IS on is internal (no internet), only the kagi-relay holds
// `egress`, and the relay is pinned to kagi.com:443.  It is the static drift
// guard paired with the scholar's runtime fail-closed self-check.
package composelint

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type compose struct {
	Services map[string]service    `yaml:"services"`
	Networks map[string]networkDef `yaml:"networks"`
}

type service struct {
	Command  []string `yaml:"command"`
	Volumes  []string `yaml:"volumes"`
	Networks []string `yaml:"networks"`
}

type networkDef struct {
	Internal bool `yaml:"internal"`
	External bool `yaml:"external"`
}

func (s service) hasNet(n string) bool {
	for _, x := range s.Networks {
		if x == n {
			return true
		}
	}
	return false
}

// Check returns the scholar-containment violations in the compose file; an empty
// slice means the file is clean.
func Check(data []byte) ([]string, error) {
	var c compose
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse compose: %w", err)
	}
	var v []string

	sch, ok := c.Services["scholar"]
	if !ok {
		return []string{"no `scholar` service defined"}, nil
	}
	if sch.hasNet("egress") {
		v = append(v, "scholar holds `egress` — it must reach the internet ONLY through kagi-relay")
	}
	if sch.hasNet("statenet") {
		v = append(v, "scholar holds `statenet` — use `scholarstate` so it gets no route to builder/scribe")
	}
	if sch.hasNet("buildnet") {
		v = append(v, "scholar holds `buildnet` — it must have no route to builder/scribe/agent")
	}
	for _, vol := range sch.Volumes {
		if strings.Contains(vol, "/workspace") {
			v = append(v, "scholar mounts /workspace — it must never see project source")
		}
	}
	// Every LOCAL network the scholar is on must be internal — a non-internal net
	// is an internet path that would bypass the relay. (External nets like
	// infernet are the infra stack's to guarantee; see its compose.)
	for _, n := range sch.Networks {
		if def, defined := c.Networks[n]; defined && !def.External && !def.Internal {
			v = append(v, fmt.Sprintf("scholar network %q is not `internal: true` — it may grant internet egress", n))
		}
	}

	var egressHolders []string
	for name, s := range c.Services {
		if s.hasNet("egress") {
			egressHolders = append(egressHolders, name)
		}
	}
	sort.Strings(egressHolders)
	if len(egressHolders) != 1 || egressHolders[0] != "kagi-relay" {
		v = append(v, fmt.Sprintf("exactly `kagi-relay` may hold `egress`; holders = %v", egressHolders))
	}

	relay, ok := c.Services["kagi-relay"]
	if !ok {
		v = append(v, "no `kagi-relay` service defined")
	} else if !targetsKagi(relay.Command) {
		v = append(v, fmt.Sprintf("kagi-relay is not pinned to kagi.com:443; command = %v", relay.Command))
	}
	return v, nil
}

func targetsKagi(command []string) bool {
	for _, arg := range command {
		if strings.Contains(arg, "kagi.com:443") {
			return true
		}
	}
	return false
}
