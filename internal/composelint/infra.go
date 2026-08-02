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

package composelint

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Stack identifies which compose file's invariant set applies to a parsed
// document, so the lint command can dispatch without trusting filenames.
type Stack string

const (
	// StackCell is the per-project cell stack (docker/cell.yaml),
	// recognized by its `archivist` service.
	StackCell Stack = "cell"
	// StackInfra is the abbey — the machine's shared doors and its memory
	// (docker/abbey.yaml, docs/abbey.md) — recognized by its `infer`
	// service.  The constant keeps its name: it is still "the shared
	// stack" as far as callers are concerned.
	StackInfra Stack = "infra"
)

// Identify reports which stack's invariants the compose document is subject
// to.  It fails closed: a document that matches neither sentinel — or both —
// is an error, never a silently unlinted file.
func Identify(data []byte) (Stack, error) {
	var c compose
	if err := yaml.Unmarshal(data, &c); err != nil {
		return "", fmt.Errorf("parse compose: %w", err)
	}
	_, cell := c.Services["archivist"]
	_, infra := c.Services["infer"]
	switch {
	case cell && infra:
		return "", fmt.Errorf("compose file defines both `archivist` and `infer` — the cell and the abbey must not merge")
	case cell:
		return StackCell, nil
	case infra:
		return StackInfra, nil
	default:
		return "", fmt.Errorf("compose file defines neither `archivist` nor `infer` — unknown stack, refusing to lint as clean")
	}
}

// CheckInfra returns the shared-inference-stack violations (docker/
// inference.yaml); an empty slice means the file is clean.  The invariants
// are the agency topology of docs/agency.md: the agency is the sole
// inference door, `infer` sits behind it on `modelnet` with no
// consumer-reachable network, the localhost relay fronts the agency — so no
// path to the model server bypasses the door — and the deep-think node is
// reachable only through the agency's blind `deepthink-relay`, whose target
// is the env-provided ${DEEPTHINK_ADDR} so no LAN address is committed.
func CheckInfra(data []byte) ([]string, error) {
	var c compose
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse compose: %w", err)
	}
	var v []string

	agency, ok := c.Services["agency"]
	if !ok {
		v = append(v, "no `agency` service defined — the sole inference door is missing")
	} else {
		v = append(v, wantsRoleEntrypoint(c, "agency", "agency")...)
		// infernet_big is the sanctioned exception: internal, and its only
		// other member is the blind deepthink-relay (asserted below).
		for _, n := range agency.egressCapable("infernet_big") {
			v = append(v, fmt.Sprintf("agency holds %q — the inference door gets no egress-capable network", n))
		}
		for _, n := range agency.Networks {
			if def, defined := c.Networks[n.Name]; defined && !def.External && !def.Internal {
				v = append(v, fmt.Sprintf("agency network %q is not `internal: true` — it may grant internet egress", n.Name))
			}
		}
		// The cutover invariant: the deployed door routes engine classes
		// (policy config).  A -upstream command is pass-through drift that
		// would forward class names to raw ollama as model tags — every
		// consumer's ask becomes model-not-found.
		for _, arg := range agency.Command {
			if strings.Contains(arg, "-upstream") {
				v = append(v, "agency command uses -upstream — the deployed door routes engine classes, never pass-through")
			}
		}
		// The status volume: the agency is its ONE writer (docs/agency.md).
		statusMounts := 0
		for _, vol := range agency.Volumes {
			if strings.HasPrefix(vol, "agency_status:") {
				statusMounts++
				if strings.HasSuffix(vol, ":ro") {
					v = append(v, "agency's agency_status mount is `:ro` — the snapshot writer needs to write it")
				}
			}
		}
		if statusMounts == 0 {
			v = append(v, "agency has no agency_status mount — the status snapshots have nowhere to land (`agency_status:/status`)")
		}
	}
	// The volume's OTHER end — the state service reading it `:ro`, and
	// nobody else touching it at all — is memoryDoorViolations' business
	// (the snapshot is one-way glass: agency writes, state renders).

	// The model server retreats behind the door: modelnet only, so no
	// consumer (nothing on infernet) can dial it.
	infer, ok := c.Services["infer"]
	if !ok {
		v = append(v, "no `infer` service defined")
	} else if len(infer.Networks) != 1 || infer.Networks[0].Name != "modelnet" {
		v = append(v, fmt.Sprintf("infer must sit on `modelnet` alone (reachable only via the agency); networks = %v", infer.Networks.names()))
	}

	if def, ok := c.Networks["modelnet"]; !ok {
		v = append(v, "no `modelnet` network defined — infer has no private net to retreat to")
	} else if !def.Internal {
		v = append(v, "`modelnet` is not `internal: true` — the model server's net must have no route out")
	}
	var modelnetHolders []string
	for name, s := range c.Services {
		if s.hasNet("modelnet") && name != "agency" && name != "infer" {
			modelnetHolders = append(modelnetHolders, name)
		}
	}
	sort.Strings(modelnetHolders)
	for _, name := range modelnetHolders {
		v = append(v, fmt.Sprintf("%s holds `modelnet` — only the agency and infer may share the model server's net", name))
	}

	// The localhost relay fronts the AGENCY: a relay pinned to raw ollama
	// would hand the host (and anything that reaches the host port) the
	// unfiltered model-server API around the door.
	proxy, ok := c.Services["proxy"]
	if !ok {
		v = append(v, "no `proxy` service defined — the localhost relay is missing")
	} else if !targetsHost(proxy.Command, "agency:11434") {
		v = append(v, fmt.Sprintf("proxy is not pinned to agency:11434 — the relay must front the door, not raw ollama; command = %v", proxy.Command))
	}

	// The deep-think relay (docs/deepthink.md): the agency's ONLY path to
	// the LAN node.  Blind socat pinned to the env-provided
	// ${DEEPTHINK_ADDR} — a literal target here would commit a LAN address
	// — holding exactly its two nets: the internal agency-facing
	// infernet_big and the NAT-routed lanegress.  Anything more (infernet
	// above all) would hand consumers the node's raw API around the door.
	relay, ok := c.Services["deepthink-relay"]
	if !ok {
		v = append(v, "no `deepthink-relay` service defined — the agency has no path to the deep-think node")
	} else {
		if !targetsEnvAddr(relay.Command, "DEEPTHINK_ADDR") {
			v = append(v, fmt.Sprintf("deepthink-relay must forward to the env-provided ${DEEPTHINK_ADDR}, never a committed address; command = %v", relay.Command))
		}
		relayNets := relay.Networks.names()
		sort.Strings(relayNets)
		if !slices.Equal(relayNets, []string{"infernet_big", "lanegress"}) {
			v = append(v, fmt.Sprintf("deepthink-relay must hold exactly [infernet_big lanegress]; networks = %v", relay.Networks.names()))
		}
		// The routes dial the node as deepthink.internal — a name the
		// loopback-bound ollama's DNS-rebinding guard accepts in the Host
		// header, where the bare service name draws a 403.  Load-bearing
		// topology: without the alias every probe is refused and the node
		// is forever absent.
		if !slices.Contains(relay.Networks.aliasesOn("infernet_big"), "deepthink.internal") {
			v = append(v, "deepthink-relay must carry the `deepthink.internal` alias on infernet_big — the node's ollama 403s Host headers outside its allowlist, so the bare service name can never probe present")
		}
	}
	if def, ok := c.Networks["infernet_big"]; !ok {
		v = append(v, "no `infernet_big` network defined — the agency has no private net to the deep-think relay")
	} else if !def.Internal {
		v = append(v, "`infernet_big` is not `internal: true` — the agency's net to the relay must have no route out")
	}
	var bigHolders []string
	for name, s := range c.Services {
		if s.hasNet("infernet_big") && name != "agency" && name != "deepthink-relay" {
			bigHolders = append(bigHolders, name)
		}
	}
	sort.Strings(bigHolders)
	for _, name := range bigHolders {
		v = append(v, fmt.Sprintf("%s holds `infernet_big` — only the agency and the deepthink-relay may share it", name))
	}
	var lanHolders []string
	for name, s := range c.Services {
		if s.hasNet("lanegress") && name != "deepthink-relay" {
			lanHolders = append(lanHolders, name)
		}
	}
	sort.Strings(lanHolders)
	for _, name := range lanHolders {
		v = append(v, fmt.Sprintf("%s holds `lanegress` — only the deepthink-relay may reach the LAN", name))
	}

	// The internet is reachable by exactly three blind relays — the whole
	// machine's egress surface, and the reason the doors are shared.
	if h := holdersOf(c, "egress"); !slices.Equal(h, []string{"github-api-relay", "github-egress", "kagi-relay"}) {
		v = append(v, fmt.Sprintf("only the pinned relays may hold `egress` (kagi-relay, github-egress, github-api-relay); holders = %v", h))
	}

	v = append(v, researchDoorViolations(c)...)
	v = append(v, forgeDoorViolations(c)...)
	v = append(v, memoryDoorViolations(c)...)

	// Every worker container execs its own role link, so the topology file
	// says what each container is and no service can run another's role;
	// and the image split holds — no toolchain-bearing image in the abbey
	// (the workbench is the agent's alone, and the agent is a cell's).
	for _, w := range []struct{ service, role string }{
		{"scholar", "scholar"}, {"state", "state-service"},
	} {
		v = append(v, wantsRoleEntrypoint(c, w.service, w.role)...)
	}
	for _, name := range []string{"agency", "scholar", "state"} {
		svc, ok := c.Services[name]
		if !ok {
			continue
		}
		imageVar := "WORKERS_IMAGE"
		if name == "agency" {
			imageVar = "AGENCY_IMAGE"
		}
		if !strings.Contains(svc.Image, "${"+imageVar) {
			v = append(v, fmt.Sprintf("%s image must come from ${%s}; image = %q", name, imageVar, svc.Image))
		}
		if svc.runsAsRoot() {
			v = append(v, fmt.Sprintf("%s must run as a non-root user; user = %q", name, svc.User))
		}
	}
	v = append(v, dockerSocketViolations(c)...)
	v = append(v, dnsPinViolations(c)...)
	return v, nil
}

// researchDoorViolations checks the shared scholar (docs/abbey.md): it
// handles UNTRUSTED web content for every cell, so its only route out is
// the kagi-relay, it never shares the archivists' wire, and it holds no
// workspace of any kind.
func researchDoorViolations(c compose) []string {
	var v []string
	sch, ok := c.Services["scholar"]
	if !ok {
		return []string{"no `scholar` service defined — the machine has no research door"}
	}
	for _, n := range sch.egressCapable("kagiegress") { // kagiegress IS its sanctioned route
		v = append(v, fmt.Sprintf("scholar holds %q — it must reach out ONLY through the kagi-relay", n))
	}
	if sch.hasNet("statenet") {
		v = append(v, "scholar holds `statenet` — use `scholarstate` so it never shares the archivists' wire")
	}
	if sch.hasNet("gitegress") {
		v = append(v, "scholar holds `gitegress` — the research door gets no route to the forge")
	}
	for _, vol := range sch.Volumes {
		if strings.Contains(vol, ":/grange") || strings.Contains(vol, ":/workspace") {
			v = append(v, fmt.Sprintf("scholar mounts a workspace (%q) — web content and project source never meet", vol))
		}
	}
	for _, n := range sch.Networks {
		if def, defined := c.Networks[n.Name]; defined && !def.External && !def.Internal {
			v = append(v, fmt.Sprintf("scholar network %q is not `internal: true` — it may grant internet egress", n.Name))
		}
	}
	relay, ok := c.Services["kagi-relay"]
	if !ok {
		v = append(v, "no `kagi-relay` service defined")
	} else if !targetsHost(relay.Command, "kagi.com:443") {
		v = append(v, fmt.Sprintf("kagi-relay is not pinned to kagi.com:443; command = %v", relay.Command))
	}
	if h := holdersOf(c, "kagiegress"); !slices.Equal(h, []string{"kagi-relay", "scholar"}) {
		v = append(v, fmt.Sprintf("kagiegress membership must be exactly scholar+kagi-relay; got %v", h))
	}
	return v
}

// forgeDoorViolations checks the shared git relays (docs/archivist.md):
// literal socat destinations (no ${} a deploy could repoint), and the
// alias/target DECOUPLED so no container both holds the github.com alias
// and resolves it — the front carries the alias git dials (TLS verifies
// the real cert against the dialed name) and pipes to a separately-named
// egress hop.  The api relay needs no alias: the archivist's Go client
// dials it by service name with SNI api.github.com.
func forgeDoorViolations(c compose) []string {
	var v []string
	for _, n := range []string{"gitegress", "gitforward"} {
		if def, defined := c.Networks[n]; !defined || !def.Internal {
			v = append(v, fmt.Sprintf("%s must be defined `internal: true` — it is a hop between the archivists and the relays, never the internet", n))
		}
	}
	// The archivists live in the CELLS and join gitegress as an external
	// network, so on this side the door is exactly its two dialable
	// relays; gitforward is the git two-hop alone.
	if h := holdersOf(c, "gitegress"); !slices.Equal(h, []string{"github-api-relay", "github-relay"}) {
		v = append(v, fmt.Sprintf("gitegress membership in the abbey must be exactly github-relay and github-api-relay; got %v", h))
	}
	if h := holdersOf(c, "gitforward"); !slices.Equal(h, []string{"github-egress", "github-relay"}) {
		v = append(v, fmt.Sprintf("gitforward membership must be exactly github-relay and github-egress (the git two-hop); got %v", h))
	}
	for _, r := range []struct {
		service, target, alias string
	}{
		{"github-relay", "github-egress:443", "github.com"},
		{"github-egress", "github.com:443", ""},
		{"github-api-relay", "api.github.com:443", ""},
	} {
		relay, ok := c.Services[r.service]
		if !ok {
			v = append(v, fmt.Sprintf("no `%s` service defined — the forge door is incomplete without it", r.service))
			continue
		}
		if !targetsHost(relay.Command, r.target) {
			v = append(v, fmt.Sprintf("%s must pipe to the literal TCP:%s; command = %v", r.service, r.target, relay.Command))
		}
		if r.alias != "" && !slices.Contains(relay.Networks.aliasesOn("gitegress"), r.alias) {
			v = append(v, fmt.Sprintf("%s must carry the network alias %q on gitegress (git dials it for end-to-end TLS)", r.service, r.alias))
		}
	}
	return v
}

// memoryDoorViolations checks the fleet's state service and its status
// relay (docs/abbey.md): the state service owns the durable record, reads
// the agency's snapshot through one-way glass (`:ro`, no network edge),
// and is reachable only on its three wires — the status relay is the one
// thing that publishes to a host port.
func memoryDoorViolations(c compose) []string {
	var v []string
	st, ok := c.Services["state"]
	if !ok {
		return []string{"no `state` service defined — the fleet has no audit trail or approvals desk"}
	}
	nets := st.Networks.names()
	sort.Strings(nets)
	if !slices.Equal(nets, []string{"scholarstate", "statenet", "statepub"}) {
		v = append(v, fmt.Sprintf("state networks must be exactly statenet+scholarstate+statepub; got %v", nets))
	}
	mounted := false
	for _, vol := range st.Volumes {
		if strings.HasPrefix(vol, "agency_status:") {
			mounted = true
			if !strings.HasSuffix(vol, ":ro") {
				v = append(v, "state's agency_status mount is not `:ro` — the snapshot is read, never written, by its reader")
			}
		}
	}
	if !mounted {
		v = append(v, "state has no agency_status mount — the dashboard's Inference panel reads `agency_status:/agency-status:ro`")
	}
	// Only the agency writes the snapshot; only the state service reads it.
	for _, name := range serviceNames(c) {
		if name == "state" || name == "agency" {
			continue
		}
		for _, vol := range c.Services[name].Volumes {
			if strings.HasPrefix(vol, "agency_status:") {
				v = append(v, fmt.Sprintf("%s mounts agency_status — the agency writes it and the state service reads it, nobody else", name))
			}
		}
	}
	if h := holdersOf(c, "statepub"); !slices.Equal(h, []string{"state", "status"}) {
		v = append(v, fmt.Sprintf("statepub membership must be exactly state+status; got %v", h))
	}
	if h := holdersOf(c, "scholarstate"); !slices.Equal(h, []string{"scholar", "state"}) {
		v = append(v, fmt.Sprintf("scholarstate membership must be exactly scholar+state; got %v", h))
	}
	if _, ok := c.Services["status"]; !ok {
		v = append(v, "no `status` service defined — the operator has no window on the record")
	}
	return v
}

// targetsEnvAddr reports whether a socat-style command forwards to an
// address supplied by the named compose variable (with any default), e.g.
// "TCP:${DEEPTHINK_ADDR:-127.0.0.1:1}".
func targetsEnvAddr(command []string, varName string) bool {
	for _, arg := range command {
		if strings.HasPrefix(arg, "TCP:${"+varName) {
			return true
		}
	}
	return false
}

// targetsHost reports whether a socat-style command forwards to the given
// host:port.
func targetsHost(command []string, hostPort string) bool {
	for _, arg := range command {
		if arg == "TCP:"+hostPort {
			return true
		}
	}
	return false
}
