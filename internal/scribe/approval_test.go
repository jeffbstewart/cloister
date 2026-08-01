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

package scribe

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeffbstewart/cloister/internal/workspace"
)

// TestApplyStagedRefusesAbsentRoot: an approval resolved after a dispose
// (or before a provision) fails with the lifecycle error instead of
// letting MkdirAll materialize parents under a root that is not there —
// a bare tree in a grange root reads as CORRUPT to the archivist.
func TestApplyStagedRefusesAbsentRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tree") // never created
	root, err := workspace.At(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Version: "test", Root: root})

	err = s.applyStaged(stagedOp{Path: "sub/x.txt", Content: []byte("x"), Perm: 0o644})
	if !errors.Is(err, workspace.ErrNotProvisioned) {
		t.Fatalf("applyStaged on an absent root = %v, want ErrNotProvisioned", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("applyStaged materialized the absent root: stat = %v", statErr)
	}
}
