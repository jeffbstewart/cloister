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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedCellStackIsContained runs the lint against the real repo file,
// so a commit that breaks the cell's containment fails the test suite.
func TestCommittedCellStackIsContained(t *testing.T) {
	path := filepath.Join("..", "..", "docker", "ai-workers.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	v, err := Check(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("committed ai-workers.yaml violates cell containment:\n  - %s",
			strings.Join(v, "\n  - "))
	}
}

func TestCatchesViolations(t *testing.T) {
	base := func(scholarNets, scholarVols, relayCmd, agentYaml, extra string) string {
		return `
networks:
  researchnet: { internal: true }
  scholarstate: { internal: true }
  kagiegress: { internal: true }
  gitegress: { internal: true }
  gitforward: { internal: true }
  statenet: { internal: true }
  archivistnet: { internal: true }
  infernet: { external: true }
  egress: {}
services:
  scholar:
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/scholar"]
    dns: "127.0.0.1"
    networks: ` + scholarNets + `
    volumes: ` + scholarVols + `
  kagi-relay:
    command: ` + relayCmd + `
    networks: [kagiegress, egress]
  archivist:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/archivist"]
    dns: "127.0.0.1"
    networks: [archivistnet, statenet, gitegress]
    volumes: ["grange:/grange"]
  github-relay:
    command: ["TCP-LISTEN:443,fork,reuseaddr", "TCP:github-egress:443"]
    dns: "127.0.0.1"
    networks:
      gitegress: { aliases: [github.com] }
      gitforward: {}
  github-egress:
    command: ["TCP-LISTEN:443,fork,reuseaddr", "TCP:github.com:443"]
    networks:
      gitforward: {}
      egress: {}
  github-api-relay:
    command: ["TCP-LISTEN:443,fork,reuseaddr", "TCP:api.github.com:443"]
    networks:
      gitegress: {}
      egress: {}
` + agentYaml + extra
	}
	clean := `[researchnet, scholarstate, kagiegress]`
	noVols := `[]`
	kagiCmd := `["TCP-LISTEN:8443,fork,reuseaddr", "TCP:kagi.com:443"]`
	agentClean := `  agent:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKBENCH_IMAGE}
    dns: "127.0.0.1"
    networks: [infernet, archivistnet, researchnet]
    volumes: ["grange:/grange", "qwen_home:/home/agent/.qwen"]
`
	cleanCompose := func() string {
		return base(clean, noVols, kagiCmd, agentClean, "")
	}

	cases := map[string]string{
		"scholar on egress":       base(`[researchnet, kagiegress, egress]`, noVols, kagiCmd, agentClean, ""),
		"scholar on statenet":     base(`[researchnet, statenet, kagiegress]`, noVols, kagiCmd, agentClean, ""),
		"scholar on archivistnet": base(`[researchnet, archivistnet, kagiegress]`, noVols, kagiCmd, agentClean, ""),
		"scholar mounts the grange": strings.Replace(cleanCompose(),
			"    volumes: []", `    volumes: ["grange:/grange:ro"]`, 1),
		"relay not pinned to kagi": base(clean, noVols, `["TCP-LISTEN:8443", "TCP:evil.example:443"]`, agentClean, ""),
		"scholar net not internal": `
networks:
  researchnet: { internal: true }
  scholarstate: { internal: true }
  kagiegress: {}
  egress: {}
services:
  scholar: { networks: [researchnet, scholarstate, kagiegress] }
  kagi-relay: { command: ["TCP:kagi.com:443"], networks: [kagiegress, egress] }`,
		"second egress holder": base(clean, noVols, kagiCmd, agentClean, `  sneaky:
    networks: [egress]`),
		// The grange cutover: the retired mediators must not ride back in,
		// and the host tree appears nowhere.
		"scribe rides back in": base(clean, noVols, kagiCmd, agentClean, `  scribe:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    networks: [statenet]
`),
		"librarian rides back in": base(clean, noVols, kagiCmd, agentClean, `  librarian:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    networks: [statenet]
`),
		"builder rides back in": base(clean, noVols, kagiCmd, agentClean, `  builder:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${TOOLCHAIN_IMAGE}
    networks: [statenet]
`),
		"agent mounts a host workspace": strings.Replace(cleanCompose(),
			`volumes: ["grange:/grange", "qwen_home:/home/agent/.qwen"]`,
			`volumes: ["grange:/grange", "${WORKSPACE}:/workspace"]`, 1),
		"outsider mounts a host workspace": base(clean, noVols, kagiCmd, agentClean, `  sneaky:
    dns: "127.0.0.1"
    networks: [statenet]
    volumes: ["/host/tree:/workspace"]
`),
		// The agent's grange: exactly one mount, the dedicated volume, rw.
		"agent missing the grange": strings.Replace(cleanCompose(),
			`volumes: ["grange:/grange", "qwen_home:/home/agent/.qwen"]`,
			`volumes: ["qwen_home:/home/agent/.qwen"]`, 1),
		"agent grange read-only": strings.Replace(cleanCompose(),
			`"grange:/grange"`, `"grange:/grange:ro"`, 1),
		"agent grange not the dedicated volume": strings.Replace(cleanCompose(),
			`"grange:/grange"`, `"/host/tree:/grange"`, 1),
		"agent networks wrong": strings.Replace(cleanCompose(),
			`networks: [infernet, archivistnet, researchnet]`,
			`networks: [infernet, archivistnet, researchnet, statenet]`, 1),
		"agent runs as root": strings.Replace(cleanCompose(),
			"  agent:\n    user: \"1000:1000\"", "  agent:\n    user: \"0:0\"", 1),
		// The image split: only the agent carries a toolchain (the
		// workbench); no worker may share one.
		"agent on the workers image": strings.Replace(cleanCompose(),
			"agent:\n    user: \"1000:1000\"\n    image: ${REGISTRY:-x}/${WORKBENCH_IMAGE}",
			"agent:\n    user: \"1000:1000\"\n    image: ${REGISTRY:-x}/${WORKERS_IMAGE}", 1),
		"scholar on the workbench image": strings.Replace(cleanCompose(),
			"scholar:\n    image: ${REGISTRY:-x}/${WORKERS_IMAGE}",
			"scholar:\n    image: ${REGISTRY:-x}/${WORKBENCH_IMAGE}", 1),
		// The multi-call cutover: a worker must exec its own role link.
		"archivist runs another role's link": strings.Replace(cleanCompose(),
			`entrypoint: ["/usr/local/bin/archivist"]`,
			`entrypoint: ["/usr/local/bin/scholar"]`, 1),
		// The agency's status volume: in the cell, only the state service
		// reads it, and only read-only.
		"agent mounts agency_status": strings.Replace(cleanCompose(),
			`"qwen_home:/home/agent/.qwen"`, `"agency_status:/agency-status:ro"`, 1),
		"state agency_status mount not ro": base(clean, noVols, kagiCmd, agentClean, `  state:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/state-service"]
    networks: [statenet]
    volumes: ["state:/state", "agency_status:/agency-status"]
`),
		"state missing agency_status mount": base(clean, noVols, kagiCmd, agentClean, `  state:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/state-service"]
    networks: [statenet]
    volumes: ["state:/state"]
`),
		// The DNS discipline: an all-internal service must pin the dead
		// upstream, and pin it to exactly the loopback black hole.
		"jailed worker missing the dns pin": strings.Replace(cleanCompose(),
			"  archivist:\n    user: \"1000:1000\"\n    image: ${REGISTRY:-x}/${WORKERS_IMAGE}\n    entrypoint: [\"/usr/local/bin/archivist\"]\n    dns: \"127.0.0.1\"",
			"  archivist:\n    user: \"1000:1000\"\n    image: ${REGISTRY:-x}/${WORKERS_IMAGE}\n    entrypoint: [\"/usr/local/bin/archivist\"]", 1),
		"jailed worker dns not the dead loopback": strings.Replace(cleanCompose(),
			`dns: "127.0.0.1"`, `dns: "8.8.8.8"`, 1),
		// The git jail (docs/archivist.md): grange volume only, exact
		// network membership, literal relay pins, hostname aliases.
		"archivist mounts the host workspace": strings.Replace(cleanCompose(),
			`volumes: ["grange:/grange"]
  github-relay:`, `volumes: ["${WORKSPACE}:/workspace"]
  github-relay:`, 1),
		"archivist holds egress": strings.Replace(cleanCompose(),
			`networks: [archivistnet, statenet, gitegress]`, `networks: [archivistnet, statenet, gitegress, egress]`, 1),
		"git relay destination not literal": strings.Replace(cleanCompose(),
			`"TCP:github.com:443"`, `"TCP:${GH_HOST}:443"`, 1),
		"git relay missing the hostname alias": strings.Replace(cleanCompose(),
			`gitegress: { aliases: [github.com] }`, `gitegress: {}`, 1),
		"outsider on gitegress": base(clean, noVols, kagiCmd, agentClean, `  sneaky:
    dns: "127.0.0.1"
    networks: [gitegress]
`),
		// The rename-evasion gaps: a mediator reborn under a fresh name
		// must not reach the grange or the pinned wires.
		"renamed mediator mounts the grange": base(clean, noVols, kagiCmd, agentClean, `  helper:
    dns: "127.0.0.1"
    networks: [archivistnet]
    volumes: ["grange:/grange"]
`),
		"outsider on archivistnet": base(clean, noVols, kagiCmd, agentClean, `  sneaky:
    dns: "127.0.0.1"
    networks: [archivistnet]
`),
		"outsider on statenet": base(clean, noVols, kagiCmd, agentClean, `  sneaky:
    dns: "127.0.0.1"
    networks: [statenet]
`),
		"archivist grange read-only": strings.Replace(cleanCompose(),
			`volumes: ["grange:/grange"]
  github-relay:`, `volumes: ["grange:/grange:ro"]
  github-relay:`, 1),
		"outsider on gitforward": base(clean, noVols, kagiCmd, agentClean, `  sneaky:
    dns: "127.0.0.1"
    networks: [gitforward]
`),
		"consumer dials infer directly": strings.Replace(cleanCompose(),
			"  agent:\n    user: \"1000:1000\"", "  agent:\n    environment: [\"OPENAI_BASE_URL=http://infer:11434/v1\"]\n    user: \"1000:1000\"", 1),
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := Check([]byte(yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(v) == 0 {
				t.Errorf("expected a containment violation, got none")
			}
		})
	}

	// And the clean shape passes — including with a well-formed state
	// service holding its read-only agency_status mount.
	if v, err := Check([]byte(cleanCompose())); err != nil || len(v) != 0 {
		t.Errorf("clean compose flagged: %v (err %v)", v, err)
	}
	withState := base(clean, noVols, kagiCmd, agentClean, `  state:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/state-service"]
    dns: "127.0.0.1"
    networks: [statenet]
    volumes: ["state:/state", "agency_status:/agency-status:ro"]
`)
	if v, err := Check([]byte(withState)); err != nil || len(v) != 0 {
		t.Errorf("clean compose with state flagged: %v (err %v)", v, err)
	}
}
