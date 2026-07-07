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

package shield

import (
	"go/build"
	"strings"
	"testing"
)

// TestStdlibOnlyNoSubprocess is the CI enforcement of the librarian's
// construction assertion (docs/librarian.md): this package and everything
// it pulls in is pure standard library, and nothing in the graph can
// shell out.  Extend the roots slice as librarian packages land.
func TestStdlibOnlyNoSubprocess(t *testing.T) {
	roots := []string{
		"github.com/jeffbstewart/cloister/internal/shield",
		"github.com/jeffbstewart/cloister/internal/repo",
		"github.com/jeffbstewart/cloister/internal/watch",
	}
	seen := map[string]bool{}
	var walk func(importPath, from string)
	walk = func(importPath, from string) {
		if seen[importPath] || importPath == "C" {
			return
		}
		seen[importPath] = true
		if importPath == "os/exec" {
			t.Errorf("os/exec reachable via %s — the librarian graph must not be able to shell out", from)
			return
		}
		pkg, err := build.Default.Import(importPath, "", 0)
		if err != nil {
			t.Fatalf("import %s (via %s): %v", importPath, from, err)
		}
		isRoot := false
		for _, r := range roots {
			if importPath == r {
				isRoot = true
			}
		}
		if !pkg.Goroot && !isRoot && !strings.HasPrefix(importPath, "github.com/jeffbstewart/cloister/internal/") {
			t.Errorf("non-stdlib dependency %s reachable via %s", importPath, from)
			return
		}
		if !pkg.Goroot && !isRoot {
			// A cloister-internal dependency: legal only if IT stays
			// stdlib-pure too, so keep walking it the same way.
			t.Logf("note: internal dependency %s (via %s) joins the stdlib-only graph", importPath, from)
		}
		for _, imp := range pkg.Imports {
			walk(imp, importPath)
		}
	}
	for _, r := range roots {
		walk(r, "(root)")
	}
}
