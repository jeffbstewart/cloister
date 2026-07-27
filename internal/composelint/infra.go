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
	// StackCell is the per-project cell stack (docker/ai-workers.yaml),
	// recognized by its `scholar` service.
	StackCell Stack = "cell"
	// StackInfra is the shared inference stack (docker/inference.yaml),
	// recognized by its `infer` service.
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
	_, cell := c.Services["scholar"]
	_, infra := c.Services["infer"]
	switch {
	case cell && infra:
		return "", fmt.Errorf("compose file defines both `scholar` and `infer` — cell and infra stacks must not merge")
	case cell:
		return StackCell, nil
	case infra:
		return StackInfra, nil
	default:
		return "", fmt.Errorf("compose file defines neither `scholar` nor `infer` — unknown stack, refusing to lint as clean")
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
			if def, defined := c.Networks[n]; defined && !def.External && !def.Internal {
				v = append(v, fmt.Sprintf("agency network %q is not `internal: true` — it may grant internet egress", n))
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
	// And nothing else in this stack may touch the status volume at all.
	var statusHolders []string
	for name, s := range c.Services {
		if name == "agency" {
			continue
		}
		for _, vol := range s.Volumes {
			if strings.HasPrefix(vol, "agency_status:") {
				statusHolders = append(statusHolders, name)
			}
		}
	}
	sort.Strings(statusHolders)
	for _, name := range statusHolders {
		v = append(v, fmt.Sprintf("%s mounts agency_status — only the agency writes the status volume", name))
	}

	// The model server retreats behind the door: modelnet only, so no
	// consumer (nothing on infernet) can dial it.
	infer, ok := c.Services["infer"]
	if !ok {
		v = append(v, "no `infer` service defined")
	} else if len(infer.Networks) != 1 || infer.Networks[0] != "modelnet" {
		v = append(v, fmt.Sprintf("infer must sit on `modelnet` alone (reachable only via the agency); networks = %v", infer.Networks))
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
		relayNets := append([]string(nil), relay.Networks...)
		sort.Strings(relayNets)
		if !slices.Equal(relayNets, []string{"infernet_big", "lanegress"}) {
			v = append(v, fmt.Sprintf("deepthink-relay must hold exactly [infernet_big lanegress]; networks = %v", relay.Networks))
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

	// Nothing in this stack touches the internet.
	var egressHolders []string
	for name, s := range c.Services {
		if s.hasNet("egress") {
			egressHolders = append(egressHolders, name)
		}
	}
	sort.Strings(egressHolders)
	for _, name := range egressHolders {
		v = append(v, fmt.Sprintf("%s holds `egress` — nothing in the inference stack may reach the internet", name))
	}
	v = append(v, dnsPinViolations(c)...)
	return v, nil
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
