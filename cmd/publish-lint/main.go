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

// publish-lint fails (exit 1) if a workflow would publish an image that may
// not be redistributed.
//
// A build context declares itself unpublishable by carrying a
// DO-NOT-PUBLISH.md, which also explains why.  This command refuses any
// docker/build-push-action step that pushes such a context, and any raw
// `docker push` in a shell step — the latter because it would route around
// the former.
//
// It exists because the comment version of the rule failed: images.yml
// asserted that GHCR packages are created private, that default was never
// verified, and the first run published a proprietary CLI to a world-
// readable package.  See docker/workbench-claude/DO-NOT-PUBLISH.md.
//
//	go run ./cmd/publish-lint
package main

import (
	"fmt"
	"os"

	"github.com/jeffbstewart/cloister/internal/publishlint"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	marked, err := publishlint.MarkedContexts(root, "docker")
	if err != nil {
		fmt.Fprintln(os.Stderr, "publish-lint:", err)
		os.Exit(2)
	}
	// A guard protecting nothing has usually stopped working — a renamed
	// directory or a deleted marker would otherwise leave this command
	// passing cleanly forever while enforcing nothing at all.
	if len(marked) == 0 {
		fmt.Fprintf(os.Stderr, "publish-lint: no build context carries %s.\n", publishlint.MarkerName)
		fmt.Fprintln(os.Stderr, "  Either the marker was removed or a context moved.  This lint has")
		fmt.Fprintln(os.Stderr, "  nothing to protect, which is not the state this repository is in:")
		fmt.Fprintln(os.Stderr, "  docker/workbench-claude carries a proprietary CLI.")
		os.Exit(1)
	}

	v, err := publishlint.CheckDir(root, root+"/.github/workflows")
	if err != nil {
		fmt.Fprintln(os.Stderr, "publish-lint:", err)
		os.Exit(2)
	}
	if len(v) > 0 {
		fmt.Fprintln(os.Stderr, "publish-lint: a workflow would publish an image that may not be redistributed:")
		for _, x := range v {
			fmt.Fprintln(os.Stderr, "  -", x)
		}
		os.Exit(1)
	}
	fmt.Printf("publish-lint: OK — no workflow publishes %v\n", marked)
}
