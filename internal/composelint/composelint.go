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

// Package composelint statically checks the compose files for the
// containment invariants: Check covers the per-project cell
// (docker/cell.yaml), CheckInfra the abbey — the machine's shared doors
// and its memory (docker/abbey.yaml, see infra.go and docs/abbey.md) —
// and Identify tells them apart.
//
// The abbey (docs/abbey.md): four doors, one machine.  The scholar is
// the research door — no `egress` network, no route to any workspace or
// to the archivists' wire, every network internal, and the kagi-relay
// pinned to kagi.com:443 (the static guard paired with the scholar's
// runtime fail-closed self-check).  The forge door is the pinned git
// relays, alias and resolution split across the two-hop.  Memory is one
// state service and one status window.  Exactly three containers on the
// whole machine hold `egress`.
//
// The cell (docs/grange.md M2–M4): two services.  The operator's HOST
// TREE appears nowhere — the agent's only workspace is the grange volume
// it shares with the archivist that provisions it — the agent is the
// ONLY service on a toolchain-bearing image (the workbench), the shared
// doors may not be re-declared per cell, and the mediators (scribe,
// librarian, builder) are gone.
//
// DNS discipline (both stacks): every service whose networks are all
// internal pins `dns: 127.0.0.1`, so the embedded resolver's upstream is
// dead and name resolution cannot become an exfiltration channel
// (CVE-2024-29018); only the relays, which hold a NAT-routed network,
// keep real DNS.
package composelint

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type compose struct {
	Services map[string]service    `yaml:"services"`
	Networks map[string]networkDef `yaml:"networks"`
}

// parse decodes a compose document, wrapping the error the way every
// Check* entry point reports it.
func parse(data []byte) (compose, error) {
	var c compose
	if err := yaml.Unmarshal(data, &c); err != nil {
		return compose{}, fmt.Errorf("parse compose: %w", err)
	}
	return c, nil
}

type service struct {
	Image       string       `yaml:"image"`
	Entrypoint  []string     `yaml:"entrypoint"`
	Command     []string     `yaml:"command"`
	Volumes     []string     `yaml:"volumes"`
	Networks    networkRefs  `yaml:"networks"`
	Environment []string     `yaml:"environment"`
	User        string       `yaml:"user"`
	DNS         stringOrList `yaml:"dns"`
	// NetworkMode is the sidecar form: `network_mode: "service:x"` puts the
	// container INSIDE x's network namespace instead of giving it networks
	// of its own.  Load-bearing for the packet tap — docker networks are
	// switched bridges, so a sniffer merely attached to a network sees none
	// of the unicast traffic on it.
	NetworkMode string `yaml:"network_mode"`
	// Profiles gates a service out of the default `up`: a diagnostic that
	// runs only when the operator asks for it, rather than continuously.
	Profiles []string `yaml:"profiles"`
	// CapAdd re-grants a capability cap_drop: [ALL] removed.  Every entry is
	// a deliberate hole and belongs in a checked list.
	CapAdd []string `yaml:"cap_add"`
	// Tmpfs is how a read-only rootfs gets a writable path — and, where the
	// path would otherwise land on a named volume, how state is made to die
	// with the container instead of outliving the workspace.
	Tmpfs []string `yaml:"tmpfs"`
}

// hasTmpfsAt reports whether the service mounts a tmpfs at exactly the given
// path.  Compose writes these as "path" or "path:opt=val,...".
func (s service) hasTmpfsAt(path string) bool {
	for _, t := range s.Tmpfs {
		if mount, _, _ := strings.Cut(t, ":"); mount == path {
			return true
		}
	}
	return false
}

// env returns the value of the named environment entry (list form, "K=V")
// and whether the service declares it at all.
func (s service) env(key string) (string, bool) {
	for _, e := range s.Environment {
		if name, val, ok := strings.Cut(e, "="); ok && name == key {
			return val, true
		}
	}
	return "", false
}

// mountsInto reports whether any of the service's volumes lands on the given
// container path, and whether every such mount is read-only.
func (s service) mountsInto(containerPath string) (mounted, readOnly bool) {
	readOnly = true
	for _, vol := range s.Volumes {
		if !strings.Contains(vol, ":"+containerPath) {
			continue
		}
		mounted = true
		if !strings.HasSuffix(vol, ":ro") {
			readOnly = false
		}
	}
	return mounted, readOnly
}

// networkRef is one entry of a service's `networks`: the network's name,
// plus the aliases the service answers to on it (mapping form only).
type networkRef struct {
	Name    string
	Aliases []string
}

// networkRefs accepts the two shapes compose allows for a service's
// `networks`: a sequence of names, or a mapping of name -> per-network
// config (possibly empty) carrying fields like `aliases`.
type networkRefs []networkRef

func (l *networkRefs) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		var names []string
		if err := node.Decode(&names); err != nil {
			return err
		}
		for _, n := range names {
			*l = append(*l, networkRef{Name: n})
		}
		return nil
	case yaml.MappingNode:
		// Content flattens the mapping to key, value, key, value, ...
		for i := 0; i+1 < len(node.Content); i += 2 {
			ref := networkRef{Name: node.Content[i].Value}
			if node.Content[i+1].Kind == yaml.MappingNode {
				var cfg struct {
					Aliases []string `yaml:"aliases"`
				}
				if err := node.Content[i+1].Decode(&cfg); err != nil {
					return err
				}
				ref.Aliases = cfg.Aliases
			}
			*l = append(*l, ref)
		}
		return nil
	default:
		return fmt.Errorf("networks must be a sequence of names or a name->config mapping, got yaml kind %v", node.Kind)
	}
}

// names returns just the network names, in file order.
func (l networkRefs) names() []string {
	names := make([]string, len(l))
	for i, ref := range l {
		names[i] = ref.Name
	}
	return names
}

// aliasesOn returns the aliases the service declares on the named network.
func (l networkRefs) aliasesOn(network string) []string {
	for _, ref := range l {
		if ref.Name == network {
			return ref.Aliases
		}
	}
	return nil
}

// stringOrList accepts a compose field that YAML allows as either a scalar
// or a sequence (`dns: 127.0.0.1` and `dns: [127.0.0.1]` are both legal).
type stringOrList []string

func (l *stringOrList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*l = stringOrList{node.Value}
		return nil
	case yaml.SequenceNode:
		var s []string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*l = stringOrList(s)
		return nil
	default:
		return fmt.Errorf("expected scalar or sequence, got yaml kind %v", node.Kind)
	}
}

// wantsRoleEntrypoint checks that a worker service execs its own role link
// of the multi-call binary — the compose file must SAY what each container
// is, and the wrong link would parse the wrong flag set.
func wantsRoleEntrypoint(c compose, serviceName, role string) []string {
	svc, ok := c.Services[serviceName]
	if !ok {
		return nil // presence is the concern of the per-stack checks
	}
	want := "/usr/local/bin/" + role
	if len(svc.Entrypoint) != 1 || svc.Entrypoint[0] != want {
		return []string{fmt.Sprintf("%s must exec its role link [%q]; entrypoint = %v", serviceName, want, svc.Entrypoint)}
	}
	return nil
}

type networkDef struct {
	Internal bool `yaml:"internal"`
	External bool `yaml:"external"`
	// Name is compose's stable-name escape from project-scoped prefixing.
	// It is what makes a network joinable across stacks: the abbey publishes
	// `name: claudenet`, a cell joins `external: true, name: claudenet`, and
	// the two are the same wire.  Unset, they would not be.
	Name string `yaml:"name"`
}

// egressCapableNetworks are the networks with a path out of their stack:
// `egress` is the internet, `frontend` publishes to the host, `kagiegress`
// leads to the kagi-relay (and through it to kagi.com), `gitegress` leads
// to the git relays (and through them to the forges), `lanegress` is the
// deepthink-relay's LAN path, `infernet_big` leads to that relay (and
// through it to the deep-think node), and the two claude nets lead to the
// jailed-claude door (and through it to api.anthropic.com).  Every
// no-egress assertion checks membership against this one list, naming any
// legitimate exception explicitly.
//
// `internal: true` is NOT the test.  Every one of these is internal; what
// makes them egress-capable is the thing waiting on the other end.
var egressCapableNetworks = []string{
	"egress", "frontend", "kagiegress", "gitegress", "lanegress",
	"infernet_big", "claudenet", "claudeplain",
}

func (s service) hasNet(n string) bool {
	for _, x := range s.Networks {
		if x.Name == n {
			return true
		}
	}
	return false
}

// runsAsRoot reports whether the service would run as root: an unset user (the
// image default, often root) or an explicit uid/name of 0/root.  A deploy-time
// ${WORKSPACE_UID:?...} reference reads as non-root, which is the point.
func (s service) runsAsRoot() bool {
	id := s.User
	if i := strings.IndexByte(id, ':'); i >= 0 {
		id = id[:i]
	}
	return id == "" || id == "0" || id == "root"
}

// dockerSocketViolations refuses the docker control plane inside either
// stack.  A socket mount is root-equivalent control of the HOST: with it
// a container can start a privileged sibling, mount any host path, or
// read every other container's secrets — every containment claim in this
// repository would become advisory.  It is why the archivist provisions
// workspace CONTENTS while volumes are created and destroyed host-side,
// and why the update watcher (docs/watchtower.md) lives beside the
// stacks rather than in them, like the Portainer agent.
func dockerSocketViolations(c compose) []string {
	var v []string
	for _, name := range serviceNames(c) {
		for _, vol := range c.Services[name].Volumes {
			if strings.Contains(vol, "docker.sock") || strings.Contains(vol, "/var/run/docker") {
				v = append(v, fmt.Sprintf("%s mounts the docker socket (%q) — that is root-equivalent control of the host; nothing in a cell or the abbey may hold the control plane", name, vol))
			}
		}
	}
	return v
}

// dnsPinViolations enforces the DNS discipline: any service whose networks
// are all internal (or external — the shared infernet, which the abbey's
// own lint keeps internal) must pin `dns: 127.0.0.1`, a dead upstream, so
// the daemon-side embedded resolver can never forward an external lookup.
// Name resolution alone is an exfiltration channel from an internal
// network (CVE-2024-29018); container-name resolution is answered
// authoritatively by the embedded resolver and never consults the
// upstream, so it is unaffected.  A service holding a NAT-routed network
// (egress, frontend) has legitimate DNS and is exempt by construction.
func dnsPinViolations(c compose) []string {
	var names []string
	for name := range c.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	var v []string
	for _, name := range names {
		s := c.Services[name]
		if len(s.Networks) == 0 {
			continue
		}
		jailed := true
		for _, n := range s.Networks {
			def, defined := c.Networks[n.Name]
			if !defined || (!def.Internal && !def.External) {
				jailed = false // a NAT-routed net: real DNS is legitimate here
			}
		}
		if !jailed {
			continue
		}
		if len(s.DNS) != 1 || s.DNS[0] != "127.0.0.1" {
			v = append(v, fmt.Sprintf("%s has only internal networks but dns = %v — pin `dns: 127.0.0.1` (dead upstream) so the embedded resolver cannot forward external lookups", name, []string(s.DNS)))
		}
	}
	return v
}

// serviceNames returns the compose file's service names, sorted.
func serviceNames(c compose) []string {
	var names []string
	for name := range c.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// egressCapable returns which egress-capable networks s holds, excluding
// the named allowed exceptions.
func (s service) egressCapable(allowed ...string) []string {
	var held []string
	for _, n := range egressCapableNetworks {
		if slices.Contains(allowed, n) {
			continue
		}
		if s.hasNet(n) {
			held = append(held, n)
		}
	}
	return held
}

// Check returns the CELL stack's violations (docker/cell.yaml); an empty
// slice means the file is clean.  A cell is two services — the agent and
// its archivist — attached to the abbey's doors (docs/abbey.md); the
// shared services and every egress holder are the abbey's business, and
// this checker refuses them here.
func Check(data []byte) ([]string, error) {
	var c compose
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse compose: %w", err)
	}
	var v []string

	// The retired mediators (grange cutover) must not ride back in, and
	// neither may the abbey's shared doors: one scholar, one state
	// service, one set of forge relays per MACHINE, or the fleet loses
	// the single burn ledger, the single audit trail, and the minimal
	// egress surface those doors exist to provide.
	for _, name := range []string{"builder", "scribe", "librarian"} {
		if _, defined := c.Services[name]; defined {
			v = append(v, fmt.Sprintf("`%s` is defined — the mediators retired with the grange cutover (the PR gate is the boundary)", name))
		}
	}
	for _, name := range []string{"scholar", "state", "status", "kagi-relay", "github-relay", "github-egress", "github-api-relay"} {
		if _, defined := c.Services[name]; defined {
			v = append(v, fmt.Sprintf("`%s` belongs to the abbey, not a cell — shared doors are one per machine (docs/abbey.md)", name))
		}
	}

	// No egress holder, ever: a cell reaches the internet only through the
	// abbey's pinned relays, on the far side of a door.
	if h := holdersOf(c, "egress"); len(h) != 0 {
		v = append(v, fmt.Sprintf("cell services hold `egress` (%v) — only the abbey's relays touch the internet", h))
	}
	// The agency's status volume is the abbey's one-way glass; nothing in
	// a cell reads or writes it (the abbey's state service renders it).
	for _, name := range serviceNames(c) {
		for _, vol := range c.Services[name].Volumes {
			if strings.HasPrefix(vol, "agency_status:") {
				v = append(v, fmt.Sprintf("%s mounts agency_status — the abbey's state service is its only reader", name))
			}
			// The operator's HOST TREE appears nowhere: no ${WORKSPACE}
			// indirection, no /workspace mount.  The grange volume is the
			// cell's only workspace, and only its two services hold it.
			if strings.Contains(vol, "${WORKSPACE") || strings.Contains(vol, ":/workspace") {
				v = append(v, fmt.Sprintf("%s mounts a host workspace (%q) — the operator's tree never enters a cell; the grange volume is the only workspace", name, vol))
			}
			if strings.Contains(vol, ":/grange") && name != "agent" && name != "archivist" {
				v = append(v, fmt.Sprintf("%s mounts the grange — only the agent and the archivist hold the workspace", name))
			}
		}
	}

	// The four doors are the ABBEY's networks, joined by name; a cell that
	// defines one locally would silently get its own private copy and
	// reach nothing.  archivistnet is the cell's own and must stay
	// internal + private to the two services.
	for _, n := range []string{"infernet", "researchnet", "statenet", "gitegress"} {
		def, defined := c.Networks[n]
		if !defined {
			v = append(v, fmt.Sprintf("network %q is not declared — a cell joins the abbey's doors by name", n))
			continue
		}
		if !def.External {
			v = append(v, fmt.Sprintf("network %q must be `external: true` — it is the abbey's door, not a cell-local network", n))
		}
	}
	if def, defined := c.Networks["archivistnet"]; !defined || !def.Internal || def.External {
		v = append(v, "archivistnet must be declared `internal: true` and cell-LOCAL — the agent->archivist MCP edge never leaves the cell")
	}
	if h := holdersOf(c, "archivistnet"); !slices.Equal(h, []string{"agent", "archivist"}) {
		v = append(v, fmt.Sprintf("archivistnet membership must be exactly agent+archivist; got %v", h))
	}

	// The agent works in the grange: exactly one mount, the dedicated
	// volume, writable — and exactly its three sanctioned edges.
	agent, ok := c.Services["agent"]
	if !ok {
		v = append(v, "no `agent` service defined")
	} else {
		v = append(v, grangeMountViolations("agent", agent)...)
		nets := agent.Networks.names()
		sort.Strings(nets)
		if !slices.Equal(nets, []string{"archivistnet", "infernet", "researchnet"}) {
			v = append(v, fmt.Sprintf("agent networks must be exactly infernet+archivistnet+researchnet; got %v", nets))
		}
	}

	// The git jail (docs/archivist.md): the archivist holds exactly
	// archivistnet + statenet + gitegress and works only in the grange.
	arc, ok := c.Services["archivist"]
	if !ok {
		v = append(v, "no `archivist` service defined — the cell's version control has no jailed owner without it")
	} else {
		nets := arc.Networks.names()
		sort.Strings(nets)
		if !slices.Equal(nets, []string{"archivistnet", "gitegress", "statenet"}) {
			v = append(v, fmt.Sprintf("archivist networks must be exactly archivistnet+statenet+gitegress; got %v", nets))
		}
		v = append(v, grangeMountViolations("archivist", arc)...)
		v = append(v, wantsRoleEntrypoint(c, "archivist", "archivist")...)
	}

	// Grange-touching services run non-root: root would bypass the
	// volume's uid-1000 ownership and drop root-owned files in the tree.
	for _, name := range []string{"agent", "archivist"} {
		if svc, ok := c.Services[name]; ok && svc.runsAsRoot() {
			v = append(v, fmt.Sprintf("%s must run as a non-root user (the grange volume's uid); user = %q", name, svc.User))
		}
	}

	// The image split (grange M4): the AGENT is the only service on a
	// toolchain-bearing image.  The linter sees raw ${VAR} text, so
	// pinning the variable NAME per service is the drift guard.
	for _, w := range []struct{ service, imageVar string }{
		{"agent", "WORKBENCH_IMAGE"},
		{"archivist", "WORKERS_IMAGE"},
	} {
		svc, ok := c.Services[w.service]
		if !ok {
			continue
		}
		if !strings.Contains(svc.Image, "${"+w.imageVar) {
			v = append(v, fmt.Sprintf("%s image must come from ${%s}; image = %q", w.service, w.imageVar, svc.Image))
		}
	}

	v = append(v, directInferDials(c)...)
	v = append(v, dockerSocketViolations(c)...)
	v = append(v, dnsPinViolations(c)...)
	return v, nil
}

// grangeMountViolations asserts the one-workspace rule for a service that
// legitimately holds it: exactly one mount, the dedicated `grange` volume,
// writable (provision clones into it; the agent edits and builds there).
func grangeMountViolations(name string, s service) []string {
	var v []string
	mounts := 0
	for _, vol := range s.Volumes {
		if !strings.Contains(vol, ":/grange") {
			continue
		}
		mounts++
		if !strings.HasPrefix(vol, "grange:") {
			v = append(v, fmt.Sprintf("%s /grange must be the dedicated grange volume; mount = %q", name, vol))
		}
		if strings.HasSuffix(vol, ":ro") {
			v = append(v, fmt.Sprintf("%s grange mount is `:ro` — the workspace is written by both holders", name))
		}
	}
	if mounts != 1 {
		v = append(v, fmt.Sprintf("%s must mount exactly one grange volume (the workspace root); found %d", name, mounts))
	}
	return v
}

// directInferDials catches a consumer env var dialing the model server
// around the agency — drift back to the pre-agency topology (and a
// runtime failure, since infer shares a network with nothing else).
func directInferDials(c compose) []string {
	var v []string
	for _, name := range serviceNames(c) {
		for _, env := range c.Services[name].Environment {
			if strings.Contains(env, "//infer:") {
				v = append(v, fmt.Sprintf("%s dials `infer` directly (%s) — consumers reach models only through the agency", name, env))
			}
		}
	}
	return v
}

// holdersOf returns the sorted names of the services attached to net.
func holdersOf(c compose, net string) []string {
	var held []string
	for name, s := range c.Services {
		if s.hasNet(net) {
			held = append(held, name)
		}
	}
	sort.Strings(held)
	return held
}
