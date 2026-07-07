// compose-lint fails (exit 1) if the cell-stack compose file violates the
// scholar containment invariants: scholar off `egress` and off builder/scribe
// nets, its networks internal, only kagi-relay on `egress`, relay pinned to
// kagi.com:443.  CI runs it on every PR:
//
//	go run ./cmd/compose-lint docker/ai-workers.yaml
package main

import (
	"fmt"
	"os"

	"github.com/jeffbstewart/cloister/internal/composelint"
)

func main() {
	path := "docker/ai-workers.yaml"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compose-lint:", err)
		os.Exit(2)
	}
	violations, err := composelint.Check(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compose-lint:", err)
		os.Exit(2)
	}
	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "compose-lint: %s FAILS scholar containment:\n", path)
		for _, x := range violations {
			fmt.Fprintln(os.Stderr, "  -", x)
		}
		os.Exit(1)
	}
	fmt.Printf("compose-lint: %s OK — scholar contained, egress pinned to kagi.com\n", path)
}
