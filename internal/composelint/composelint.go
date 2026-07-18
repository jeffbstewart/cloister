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
// containment invariants: Check covers the cell stack
// (docker/ai-workers.yaml), CheckInfra the shared inference stack
// (docker/inference.yaml, see infra.go), and Identify tells them apart.
//
// Scholar: holds no `egress` network and no route to
// builder/scribe/workspace, every network it IS on is internal (no
// internet), only the kagi-relay holds `egress`, and the relay is pinned
// to kagi.com:443 — the static drift guard paired with the scholar's
// runtime fail-closed self-check.
//
// Read path (docs/librarian.md): the agent mounts NO workspace at all —
// reads go through the librarian, writes through the scribe — and the
// librarian's workspace mount is `:ro` with no egress-capable network.
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

type service struct {
	Image       string   `yaml:"image"`
	Entrypoint  []string `yaml:"entrypoint"`
	Command     []string `yaml:"command"`
	Volumes     []string `yaml:"volumes"`
	Networks    []string `yaml:"networks"`
	Environment []string `yaml:"environment"`
	User        string   `yaml:"user"`
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
}

// egressCapableNetworks are the cell networks with a path out of the cell:
// `egress` is the internet, `frontend` publishes to the host, and
// `kagiegress` leads to the kagi-relay (and through it to kagi.com).  Every
// no-egress assertion checks membership against this one list, naming any
// legitimate exception explicitly.
var egressCapableNetworks = []string{"egress", "frontend", "kagiegress"}

func (s service) hasNet(n string) bool {
	for _, x := range s.Networks {
		if x == n {
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
	for _, n := range sch.egressCapable("kagiegress") { // kagiegress IS its sanctioned route
		v = append(v, fmt.Sprintf("scholar holds %q — it must reach out ONLY through the kagi-relay", n))
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

	// The read path: the librarian exists, holds the workspace read-only,
	// and has no egress-capable network.
	lib, ok := c.Services["librarian"]
	if !ok {
		v = append(v, "no `librarian` service defined — the agent has no read path without it")
	} else {
		wsMounts := 0
		for _, vol := range lib.Volumes {
			if strings.Contains(vol, ":/workspace") {
				wsMounts++
				if !strings.HasSuffix(vol, ":ro") {
					v = append(v, "librarian workspace mount is not `:ro` — the reader must never write source")
				}
			}
		}
		if wsMounts == 0 {
			v = append(v, "librarian has no workspace mount — it has nothing to serve")
		}
		for _, n := range lib.egressCapable() {
			v = append(v, fmt.Sprintf("librarian holds %q — the reader gets no egress-capable network", n))
		}
		for _, n := range lib.Networks {
			if def, defined := c.Networks[n]; defined && !def.External && !def.Internal {
				v = append(v, fmt.Sprintf("librarian network %q is not `internal: true` — it may grant internet egress", n))
			}
		}
	}

	// Workspace-touching workers must run as a non-root user: root would bypass
	// the per-user 0700 workspace perms (reading any tree) and drop root-owned
	// files into a user's source.  The uid is a deploy-time var; this catches a
	// missing or hardcoded-root `user:`.
	for _, name := range []string{"librarian", "scribe", "builder"} {
		if svc, ok := c.Services[name]; ok && svc.runsAsRoot() {
			v = append(v, fmt.Sprintf("%s must run as a non-root user (the workspace owner's uid); user = %q", name, svc.User))
		}
	}

	// The image split (docs/toolchains.md): the builder is the ONLY worker
	// on a toolchain image; every other worker runs the slim toolchain-free
	// image.  The linter sees the raw ${VAR} text, so pinning the variable
	// NAME per service is the drift guard — a compiler can't quietly ride
	// back into the scholar via a shared image reference.
	for _, w := range []struct{ service, imageVar string }{
		{"builder", "TOOLCHAIN_IMAGE"},
		{"librarian", "WORKERS_IMAGE"},
		{"scholar", "WORKERS_IMAGE"},
		{"scribe", "WORKERS_IMAGE"},
		{"state", "WORKERS_IMAGE"},
	} {
		svc, ok := c.Services[w.service]
		if !ok {
			continue // presence is the concern of the checks above
		}
		if !strings.Contains(svc.Image, "${"+w.imageVar) {
			v = append(v, fmt.Sprintf("%s image must come from ${%s}; image = %q", w.service, w.imageVar, svc.Image))
		}
	}

	// Every worker container execs its own role link, so the topology file
	// says what each container is and no service can run another's role.
	for _, w := range []struct{ service, role string }{
		{"builder", "builder"}, {"librarian", "librarian"}, {"scholar", "scholar"},
		{"scribe", "scribe"}, {"state", "state-service"},
	} {
		v = append(v, wantsRoleEntrypoint(c, w.service, w.role)...)
	}

	// The inference door: every consumer's model endpoint is the agency
	// (docs/agency.md).  An env var dialing `infer` directly is drift back
	// to the pre-agency topology — it would bypass the door (and fail at
	// runtime, since infer no longer shares a network with any cell).
	var svcNames []string
	for name := range c.Services {
		svcNames = append(svcNames, name)
	}
	sort.Strings(svcNames)
	for _, name := range svcNames {
		for _, env := range c.Services[name].Environment {
			if strings.Contains(env, "//infer:") {
				v = append(v, fmt.Sprintf("%s dials `infer` directly (%s) — consumers reach models only through the agency", name, env))
			}
		}
	}

	// The agent cutover: no workspace mount of ANY kind — reads are the
	// librarian's, writes are the scribe's.
	agent, ok := c.Services["agent"]
	if !ok {
		v = append(v, "no `agent` service defined")
	} else {
		for _, vol := range agent.Volumes {
			if strings.Contains(vol, ":/workspace") {
				v = append(v, "agent mounts the workspace — the agent reads via the librarian and writes via the scribe, never directly")
			}
		}
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
