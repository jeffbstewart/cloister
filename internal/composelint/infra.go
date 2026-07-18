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
	"sort"

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
// consumer-reachable network, and the localhost relay fronts the agency —
// so no path to the model server bypasses the door.
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
		for _, n := range agency.egressCapable() {
			v = append(v, fmt.Sprintf("agency holds %q — the inference door gets no egress-capable network", n))
		}
		for _, n := range agency.Networks {
			if def, defined := c.Networks[n]; defined && !def.External && !def.Internal {
				v = append(v, fmt.Sprintf("agency network %q is not `internal: true` — it may grant internet egress", n))
			}
		}
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
	return v, nil
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
