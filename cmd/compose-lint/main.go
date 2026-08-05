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

// compose-lint fails (exit 1) if a committed compose file violates its
// stack's containment invariants.  Each file is identified by content and
// checked against the matching invariant set:
//
//   - cell (docker/cell.yaml): two services on the abbey's doors — no
//     host workspace anywhere (the grange volume is the only workspace,
//     held by agent + archivist alone), no `egress` holder, no shared
//     door or retired mediator re-declared per cell, archivistnet
//     internal and private to the pair, only the agent on a toolchain
//     image, and every consumer dialing the agency — never raw infer.
//   - abbey (docker/abbey.yaml, docs/abbey.md): the machine's doors and
//     its memory — the agency the sole inference door with infer on
//     `modelnet` alone behind it; the scholar contained to the kagi-relay
//     and off the archivists' wire; the forge relays pinned with the
//     github.com alias split from its resolution across the two-hop; the
//     state service owning the record and reading the agency's snapshot
//     one-way; the deep-think path env-pinned (never a committed LAN
//     address); and exactly three containers holding `egress`.
//   - the jailed-claude OVERLAYS (docker/abbey-claude.yaml and
//     docker/cell-claude.yaml, docs/JAILED_CLAUDE.md): the inspected
//     Anthropic door.  Separate files because they trade away invariants
//     the two above assert absolutely — merging one is meant to be an act,
//     not an inheritance — so they carry their own set: exactly one new
//     internet holder, exactly one new agent edge, the credential in
//     exactly one container, the request-gate addon loaded, and a
//     placeholder token in the cell.
//
// CI runs it on every PR:
//
//	go run ./cmd/compose-lint docker/cell.yaml docker/abbey.yaml \
//	  docker/cell-claude.yaml docker/abbey-claude.yaml
//
// With no arguments it checks all four committed files.
package main

import (
	"fmt"
	"os"

	"github.com/jeffbstewart/cloister/internal/composelint"
)

// okSummary is the one-line clean verdict printed per stack kind.
var okSummary = map[composelint.Stack]string{
	composelint.StackCell:        "two services, no host tree, no egress, no shared doors re-declared; agent on the grange + workbench, archivist jailed on grange + gitegress",
	composelint.StackInfra:       "infer behind the agency on a closed modelnet, scholar contained, forge relays pinned, memory one-way, egress held by exactly the three relays",
	composelint.StackInfraClaude: "three hops, one new internet holder; the gate addon loaded, the plaintext segment two-ended, the credential in claude-egress alone, the tap profile-gated",
	composelint.StackCellClaude:  "one new edge (claudenet), a placeholder credential, no base-URL redirect, the single-certificate trust store, no new mounts",
}

func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		paths = []string{
			"docker/cell.yaml", "docker/abbey.yaml",
			"docker/cell-claude.yaml", "docker/abbey-claude.yaml",
		}
	}
	exit := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "compose-lint:", err)
			os.Exit(2)
		}
		stack, err := composelint.Identify(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compose-lint: %s: %v\n", path, err)
			os.Exit(2)
		}
		var violations []string
		switch stack {
		case composelint.StackCell:
			violations, err = composelint.Check(data)
		case composelint.StackInfra:
			violations, err = composelint.CheckInfra(data)
		case composelint.StackInfraClaude:
			violations, err = composelint.CheckInfraClaude(data)
		case composelint.StackCellClaude:
			violations, err = composelint.CheckCellClaude(data)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "compose-lint: %s: %v\n", path, err)
			os.Exit(2)
		}
		if len(violations) > 0 {
			fmt.Fprintf(os.Stderr, "compose-lint: %s FAILS %s containment:\n", path, stack)
			for _, x := range violations {
				fmt.Fprintln(os.Stderr, "  -", x)
			}
			exit = 1
			continue
		}
		fmt.Printf("compose-lint: %s OK — %s\n", path, okSummary[stack])
	}
	os.Exit(exit)
}
