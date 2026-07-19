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

// TestCommittedInfraStackIsContained runs the lint against the real repo
// file, so a commit that lets anything bypass the agency fails the suite.
func TestCommittedInfraStackIsContained(t *testing.T) {
	path := filepath.Join("..", "..", "docker", "inference.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	v, err := CheckInfra(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("committed inference.yaml violates agency containment:\n  - %s",
			strings.Join(v, "\n  - "))
	}
}

func TestIdentify(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		want    Stack
		wantErr bool
	}{
		{"cell", "services:\n  scholar: {}\n", StackCell, false},
		{"infra", "services:\n  infer: {}\n", StackInfra, false},
		{"both", "services:\n  scholar: {}\n  infer: {}\n", "", true},
		{"neither", "services:\n  mystery: {}\n", "", true},
		{"unparseable", ":\t:bad", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Identify([]byte(tc.yaml))
			if tc.wantErr != (err != nil) {
				t.Fatalf("Identify err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("Identify = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCatchesInfraViolations(t *testing.T) {
	base := func(agencyNets, inferNets, proxyCmd, modelnetDef, extra string) string {
		return `
networks:
  infernet: { internal: true }
` + modelnetDef + `
  frontend: {}
services:
  agency:
    entrypoint: ["/usr/local/bin/agency"]
    networks: ` + agencyNets + `
    volumes: ["agency_status:/status"]
  infer:
    networks: ` + inferNets + `
  proxy:
    command: ` + proxyCmd + `
    networks: [infernet, frontend]
` + extra
	}
	agencyClean := `[infernet, modelnet]`
	inferClean := `[modelnet]`
	proxyClean := `["TCP-LISTEN:11434,fork,reuseaddr", "TCP:agency:11434"]`
	modelnetClean := "  modelnet: { internal: true }"
	cleanCompose := base(agencyClean, inferClean, proxyClean, modelnetClean, "")

	cases := map[string]string{
		"no agency": `
networks:
  modelnet: { internal: true }
services:
  infer: { networks: [modelnet] }
  proxy: { command: ["TCP:agency:11434"] }`,
		"agency holds frontend":   base(`[infernet, modelnet, frontend]`, inferClean, proxyClean, modelnetClean, ""),
		"agency net not internal": base(`[infernet, modelnet, opennet]`, inferClean, proxyClean, modelnetClean+"\n  opennet: {}", ""),
		"infer still on infernet": base(agencyClean, `[infernet, modelnet]`, proxyClean, modelnetClean, ""),
		"infer off modelnet":      base(agencyClean, `[infernet]`, proxyClean, modelnetClean, ""),
		"modelnet missing":        base(agencyClean, inferClean, proxyClean, "", ""),
		"modelnet not internal":   base(agencyClean, inferClean, proxyClean, "  modelnet: {}", ""),
		"stranger on modelnet": base(agencyClean, inferClean, proxyClean, modelnetClean, `  sneaky:
    networks: [modelnet]`),
		"proxy fronts raw ollama": base(agencyClean, inferClean, `["TCP-LISTEN:11434,fork,reuseaddr", "TCP:infer:11434"]`, modelnetClean, ""),
		"proxy missing":           strings.Replace(cleanCompose, "proxy:", "not-the-proxy:", 1),
		"egress inside infra stack": base(agencyClean, inferClean, proxyClean, modelnetClean, `  leaky:
    networks: [egress]`),
		"agency not on its role link": strings.Replace(cleanCompose,
			`entrypoint: ["/usr/local/bin/agency"]`, `entrypoint: ["/usr/local/bin/scholar"]`, 1),
		// The status volume: exactly one writer, the agency.
		"agency status mount is ro": strings.Replace(cleanCompose,
			`volumes: ["agency_status:/status"]`, `volumes: ["agency_status:/status:ro"]`, 1),
		"agency missing status mount": strings.Replace(cleanCompose,
			`volumes: ["agency_status:/status"]`, `volumes: []`, 1),
		"stranger mounts status volume": base(agencyClean, inferClean, proxyClean, modelnetClean, `  sneaky:
    volumes: ["agency_status:/peek"]`),
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := CheckInfra([]byte(yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(v) == 0 {
				t.Errorf("expected a containment violation, got none")
			}
		})
	}

	// And the clean shape passes.
	if v, err := CheckInfra([]byte(cleanCompose)); err != nil || len(v) != 0 {
		t.Errorf("clean compose flagged: %v (err %v)", v, err)
	}
}

// TestCellConsumersDialTheAgency: an OPENAI_BASE_URL (or any env var)
// pointing at raw `infer` is pre-agency drift the cell check must catch.
func TestCellConsumersDialTheAgency(t *testing.T) {
	yaml := `
networks:
  researchnet: { internal: true }
  scholarstate: { internal: true }
  kagiegress: { internal: true }
  statenet: { internal: true }
  buildnet: { internal: true }
  egress: {}
services:
  scholar:
    networks: [researchnet, scholarstate, kagiegress]
    environment:
      - OPENAI_BASE_URL=http://infer:11434/v1
  kagi-relay:
    command: ["TCP-LISTEN:8443,fork,reuseaddr", "TCP:kagi.com:443"]
    networks: [kagiegress, egress]
  agent:
    networks: [buildnet]
  librarian:
    user: "1000:1000"
    networks: [buildnet, statenet]
    volumes: ["/host:/workspace:ro"]
`
	v, err := Check([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range v {
		if strings.Contains(x, "dials `infer` directly") {
			found = true
		}
	}
	if !found {
		t.Errorf("scholar dialing infer directly not flagged; violations = %v", v)
	}
}
