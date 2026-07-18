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
//   - cell stack (docker/ai-workers.yaml): scholar off `egress` and off
//     builder/scribe nets, its networks internal, only kagi-relay on
//     `egress`, relay pinned to kagi.com:443, agent mount-free, librarian
//     read-only, and every consumer dialing the agency — never raw infer.
//   - inference stack (docker/inference.yaml): the agency is the sole
//     inference door — infer on `modelnet` alone, modelnet internal and
//     private to agency+infer, the localhost relay pinned to the agency,
//     no egress anywhere in the stack.
//
// CI runs it on every PR:
//
//	go run ./cmd/compose-lint docker/ai-workers.yaml docker/inference.yaml
//
// With no arguments it checks both committed files.
package main

import (
	"fmt"
	"os"

	"github.com/jeffbstewart/cloister/internal/composelint"
)

// okSummary is the one-line clean verdict printed per stack kind.
var okSummary = map[composelint.Stack]string{
	composelint.StackCell:  "scholar contained, egress pinned to kagi.com, agent mount-free, librarian read-only, consumers dial the agency",
	composelint.StackInfra: "infer behind the agency on a closed modelnet, relay fronts the door, no egress",
}

func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		paths = []string{"docker/ai-workers.yaml", "docker/inference.yaml"}
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
