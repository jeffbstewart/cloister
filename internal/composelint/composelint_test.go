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
	path := filepath.Join("..", "..", "docker", "cell.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	v, err := Check(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("committed cell.yaml violates cell containment:\n  - %s",
			strings.Join(v, "\n  - "))
	}
}

// cellBase is a clean cell: two services on the abbey's four doors, plus
// the cell-private archivistnet.  Cases below mutate one thing about it.
func cellBase(agentYaml, archivistYaml, extra string) string {
	return `
networks:
  infernet:
    external: true
    name: infernet
  researchnet:
    external: true
    name: researchnet
  statenet:
    external: true
    name: statenet
  gitegress:
    external: true
    name: gitegress
  archivistnet:
    internal: true
services:
` + agentYaml + archivistYaml + extra
}

const (
	cellAgent = `  agent:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKBENCH_IMAGE}
    dns: "127.0.0.1"
    networks: [infernet, archivistnet, researchnet]
    volumes: ["grange:/grange", "qwen_home:/home/agent/.qwen"]
`
	cellArchivist = `  archivist:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/archivist"]
    dns: "127.0.0.1"
    networks: [archivistnet, statenet, gitegress]
    volumes: ["grange:/grange"]
`
)

func TestCatchesViolations(t *testing.T) {
	clean := func() string { return cellBase(cellAgent, cellArchivist, "") }

	cases := map[string]string{
		// The shared doors are the abbey's — one per machine.
		"scholar re-declared per cell": cellBase(cellAgent, cellArchivist, `  scholar:
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    dns: "127.0.0.1"
    networks: [researchnet]
`),
		"state re-declared per cell": cellBase(cellAgent, cellArchivist, `  state:
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    dns: "127.0.0.1"
    networks: [statenet]
`),
		"kagi-relay re-declared per cell": cellBase(cellAgent, cellArchivist, `  kagi-relay:
    command: ["TCP-LISTEN:8443,fork,reuseaddr", "TCP:kagi.com:443"]
    networks: [statenet]
`),
		"github relay re-declared per cell": cellBase(cellAgent, cellArchivist, `  github-relay:
    command: ["TCP-LISTEN:443,fork,reuseaddr", "TCP:github-egress:443"]
    networks: [gitegress]
`),
		// The retired mediators stay retired.
		"scribe rides back in": cellBase(cellAgent, cellArchivist, `  scribe:
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    dns: "127.0.0.1"
    networks: [statenet]
`),
		"librarian rides back in": cellBase(cellAgent, cellArchivist, `  librarian:
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    dns: "127.0.0.1"
    networks: [statenet]
`),
		"builder rides back in": cellBase(cellAgent, cellArchivist, `  builder:
    image: ${REGISTRY:-x}/${TOOLCHAIN_IMAGE}
    dns: "127.0.0.1"
    networks: [statenet]
`),
		// No egress, no host tree, no agency_status in a cell.
		"a cell service holds egress": cellBase(cellAgent, cellArchivist, `  sneaky:
    networks: [egress]
`),
		"host workspace bind": strings.Replace(clean(),
			`"grange:/grange", "qwen_home:/home/agent/.qwen"`,
			`"grange:/grange", "${WORKSPACE}:/workspace"`, 1),
		"host workspace bind on an outsider": cellBase(cellAgent, cellArchivist, `  sneaky:
    dns: "127.0.0.1"
    networks: [statenet]
    volumes: ["/host/tree:/workspace"]
`),
		"cell reads agency_status": strings.Replace(clean(),
			`"qwen_home:/home/agent/.qwen"`, `"agency_status:/agency-status:ro"`, 1),
		// The grange: agent + archivist only, exactly one mount, rw.
		"outsider mounts the grange": cellBase(cellAgent, cellArchivist, `  helper:
    dns: "127.0.0.1"
    networks: [archivistnet]
    volumes: ["grange:/grange"]
`),
		"agent has no grange": strings.Replace(clean(),
			`volumes: ["grange:/grange", "qwen_home:/home/agent/.qwen"]`,
			`volumes: ["qwen_home:/home/agent/.qwen"]`, 1),
		"agent grange read-only": strings.Replace(clean(),
			`"grange:/grange", "qwen_home`, `"grange:/grange:ro", "qwen_home`, 1),
		"agent grange not the dedicated volume": strings.Replace(clean(),
			`"grange:/grange", "qwen_home`, `"/host/tree:/grange", "qwen_home`, 1),
		"archivist grange read-only": strings.Replace(clean(),
			"volumes: [\"grange:/grange\"]\n", "volumes: [\"grange:/grange:ro\"]\n", 1),
		// Exact network membership on both services, and the doors are
		// joined, never re-declared locally.
		"agent networks wrong": strings.Replace(clean(),
			`networks: [infernet, archivistnet, researchnet]`,
			`networks: [infernet, archivistnet, researchnet, statenet]`, 1),
		"archivist networks wrong": strings.Replace(clean(),
			`networks: [archivistnet, statenet, gitegress]`,
			`networks: [archivistnet, statenet]`, 1),
		"a door declared cell-local": strings.Replace(clean(),
			"  researchnet:\n    external: true\n    name: researchnet",
			"  researchnet:\n    internal: true", 1),
		"a door missing entirely": strings.Replace(clean(),
			"  gitegress:\n    external: true\n    name: gitegress\n", "", 1),
		"archivistnet not internal": strings.Replace(clean(),
			"  archivistnet:\n    internal: true",
			"  archivistnet:\n    external: true\n    name: archivistnet", 1),
		"outsider on archivistnet": cellBase(cellAgent, cellArchivist, `  sneaky:
    dns: "127.0.0.1"
    networks: [archivistnet]
`),
		// Identity, images, roles, DNS, and the agency door.
		"agent runs as root": strings.Replace(clean(),
			"  agent:\n    user: \"1000:1000\"", "  agent:\n    user: \"0:0\"", 1),
		"agent on the workers image": strings.Replace(clean(),
			`image: ${REGISTRY:-x}/${WORKBENCH_IMAGE}`,
			`image: ${REGISTRY:-x}/${WORKERS_IMAGE}`, 1),
		"archivist on the workbench image": strings.Replace(clean(),
			"archivist:\n    user: \"1000:1000\"\n    image: ${REGISTRY:-x}/${WORKERS_IMAGE}",
			"archivist:\n    user: \"1000:1000\"\n    image: ${REGISTRY:-x}/${WORKBENCH_IMAGE}", 1),
		"archivist runs another role's link": strings.Replace(clean(),
			`entrypoint: ["/usr/local/bin/archivist"]`,
			`entrypoint: ["/usr/local/bin/scholar"]`, 1),
		"jailed service missing the dns pin": strings.Replace(clean(),
			"    dns: \"127.0.0.1\"\n    networks: [archivistnet, statenet, gitegress]",
			"    networks: [archivistnet, statenet, gitegress]", 1),
		"dns not the dead loopback": strings.Replace(clean(),
			`dns: "127.0.0.1"`, `dns: "8.8.8.8"`, 1),
		"consumer dials infer directly": strings.Replace(clean(),
			"  agent:\n    user: \"1000:1000\"",
			"  agent:\n    environment: [\"OPENAI_BASE_URL=http://infer:11434/v1\"]\n    user: \"1000:1000\"", 1),
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

	if v, err := Check([]byte(clean())); err != nil || len(v) != 0 {
		t.Errorf("clean cell flagged: %v (err %v)", v, err)
	}
}
