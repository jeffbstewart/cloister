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
// so a commit that breaks the scholar's containment fails the test suite.  It
// skips until the deployment migration lands docker/ai-workers.yaml, then
// guards it forever after.
func TestCommittedCellStackIsContained(t *testing.T) {
	path := filepath.Join("..", "..", "docker", "ai-workers.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("%s not yet migrated (arrives with the deployment PR)", path)
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	v, err := Check(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("committed ai-workers.yaml violates scholar containment:\n  - %s",
			strings.Join(v, "\n  - "))
	}
}

func TestCatchesViolations(t *testing.T) {
	base := func(scholarNets, scholarVols, relayCmd, agentVols, librarianYaml, extra string) string {
		return `
networks:
  researchnet: { internal: true }
  scholarstate: { internal: true }
  kagiegress: { internal: true }
  statenet: { internal: true }
  buildnet: { internal: true }
  egress: {}
services:
  scholar:
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/scholar"]
    networks: ` + scholarNets + `
    volumes: ` + scholarVols + `
  kagi-relay:
    command: ` + relayCmd + `
    networks: [kagiegress, egress]
  agent:
    networks: [buildnet]
    volumes: ` + agentVols + `
` + librarianYaml + extra
	}
	clean := `[researchnet, scholarstate, kagiegress]`
	noVols := `[]`
	agentClean := `["qwen_home:/home/node/.qwen"]`
	kagiCmd := `["TCP-LISTEN:8443,fork,reuseaddr", "TCP:kagi.com:443"]`
	librarianClean := `  librarian:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/librarian"]
    networks: [buildnet, statenet]
    volumes: ["/host:/workspace:ro"]
`
	cleanCompose := func() string {
		return base(clean, noVols, kagiCmd, agentClean, librarianClean, "")
	}

	cases := map[string]string{
		"scholar on egress":        base(`[researchnet, kagiegress, egress]`, noVols, kagiCmd, agentClean, librarianClean, ""),
		"scholar on statenet":      base(`[researchnet, statenet, kagiegress]`, noVols, kagiCmd, agentClean, librarianClean, ""),
		"scholar mounts workspace": base(clean, `["/host:/workspace:ro"]`, kagiCmd, agentClean, librarianClean, ""),
		"relay not pinned to kagi": base(clean, noVols, `["TCP-LISTEN:8443", "TCP:evil.example:443"]`, agentClean, librarianClean, ""),
		"scholar net not internal": `
networks:
  researchnet: { internal: true }
  scholarstate: { internal: true }
  kagiegress: {}
  egress: {}
services:
  scholar: { networks: [researchnet, scholarstate, kagiegress] }
  kagi-relay: { command: ["TCP:kagi.com:443"], networks: [kagiegress, egress] }`,
		"second egress holder": base(clean, noVols, kagiCmd, agentClean, librarianClean, `  sneaky:
    networks: [egress]`),
		// The read-path invariants (docs/librarian.md).
		"no librarian": base(clean, noVols, kagiCmd, agentClean, "", ""),
		"librarian workspace not ro": base(clean, noVols, kagiCmd, agentClean, `  librarian:
    networks: [buildnet, statenet]
    volumes: ["/host:/workspace"]
`, ""),
		"librarian holds egress-capable net": base(clean, noVols, kagiCmd, agentClean, `  librarian:
    networks: [buildnet, statenet, frontend]
    volumes: ["/host:/workspace:ro"]
`, ""),
		"librarian runs as root": base(clean, noVols, kagiCmd, agentClean, `  librarian:
    user: "0:0"
    networks: [buildnet, statenet]
    volumes: ["/host:/workspace:ro"]
`, ""),
		"agent mounts workspace": base(clean, noVols, kagiCmd, `["/host:/workspace:ro"]`, librarianClean, ""),
		// The multi-call cutover: a worker must exec its own role link.
		"librarian runs another role's link": base(clean, noVols, kagiCmd, agentClean, `  librarian:
    user: "1000:1000"
    entrypoint: ["/usr/local/bin/scribe"]
    networks: [buildnet, statenet]
    volumes: ["/host:/workspace:ro"]
`, ""),
		"scribe missing its role entrypoint": base(clean, noVols, kagiCmd, agentClean, librarianClean, `  scribe:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    networks: [buildnet, statenet]
`),
		// The image split: the builder is the only worker on a toolchain
		// image, and no other worker may share one.
		"builder on the workers image": base(clean, noVols, kagiCmd, agentClean, librarianClean, `  builder:
    user: "1000:1000"
    image: ${REGISTRY:-x}/${WORKERS_IMAGE}
    entrypoint: ["/usr/local/bin/builder"]
    networks: [buildnet, statenet]
`),
		"scholar on the toolchain image": strings.Replace(cleanCompose(),
			"scholar:\n    image: ${REGISTRY:-x}/${WORKERS_IMAGE}",
			"scholar:\n    image: ${REGISTRY:-x}/${TOOLCHAIN_IMAGE}", 1),
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

	// And the clean shape passes.
	if v, err := Check([]byte(cleanCompose())); err != nil || len(v) != 0 {
		t.Errorf("clean compose flagged: %v (err %v)", v, err)
	}
}
