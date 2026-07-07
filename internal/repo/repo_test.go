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

package repo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeffbstewart/cloister/internal/shield"
)

const testBudget = 1 << 20

// newWorkspace materializes files under a temp root and opens a Repo.
func newWorkspace(t *testing.T, files map[string]string) (string, *Repo) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		write(t, root, rel, content)
	}
	r, err := New(root, Config{Budget: testBudget, MaxFileSize: 64 << 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return root, r
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadServesResidentContent(t *testing.T) {
	_, r := newWorkspace(t, map[string]string{
		"src/main.go": "package main\n",
	})
	got, err := r.Read("src/main.go")
	if err != nil || string(got) != "package main\n" {
		t.Fatalf("Read = %q, %v", got, err)
	}
	e, err := r.Stat("src/main.go")
	if err != nil || !e.Resident || e.LineCount != 1 || e.SHA256 == "" {
		t.Fatalf("Stat = %+v, %v", e, err)
	}
}

// TestReadAdmitsNewlyCreatedFile is the review finding from PR draft:
// a bare re-stat of known entries misses files created since the last
// rescan.  Read must admit an on-disk path the model has never seen.
func TestReadAdmitsNewlyCreatedFile(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{"seed.txt": "s"})
	write(t, root, "brand/new/file.txt", "fresh\n")

	got, err := r.Read("brand/new/file.txt")
	if err != nil || string(got) != "fresh\n" {
		t.Fatalf("Read(new file) = %q, %v; want content with no rescan", got, err)
	}
	// The admitted ancestors make the new file listable immediately.
	entries, err := r.List("brand/new")
	if err != nil || len(entries) != 1 || entries[0].Path != "brand/new/file.txt" {
		t.Fatalf("List(brand/new) = %+v, %v", entries, err)
	}
}

func TestStatAdmitsNewlyCreatedFile(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{"seed.txt": "s"})
	write(t, root, "later.txt", "content here")
	e, err := r.Stat("later.txt")
	if err != nil || e.Size != int64(len("content here")) {
		t.Fatalf("Stat(new file) = %+v, %v", e, err)
	}
}

func TestNewFileMatchingAiignoreDeniesButStats(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{
		".aiignore": "*.secret\n",
		"seed.txt":  "s",
	})
	write(t, root, "prod.secret", "hunter2")
	if _, err := r.Read("prod.secret"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Read(new shielded file) err = %v, want ErrForbidden", err)
	}
	e, err := r.Stat("prod.secret")
	if err != nil || e.Visibility != shield.Stripped || e.Resident {
		t.Fatalf("Stat(new shielded file) = %+v, %v; want Stripped metadata", e, err)
	}
}

func TestHiddenPathsDenyWhetherOrNotTheyExist(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{
		".gitignore": "build/\n",
		"seed.txt":   "s",
	})
	write(t, root, "build/out.jar", "\x00binary")
	if _, err := r.Read("build/out.jar"); !errors.Is(err, ErrForbidden) {
		t.Errorf("Read(existing hidden) err = %v, want ErrForbidden", err)
	}
	if _, err := r.Read("build/never-created.txt"); !errors.Is(err, ErrForbidden) {
		t.Errorf("Read(absent hidden) err = %v, want ErrForbidden (no existence oracle)", err)
	}
	if _, err := r.Read("truly-absent.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read(absent visible) err = %v, want ErrNotFound", err)
	}
}

func TestListOmitsHiddenShowsStripped(t *testing.T) {
	_, r := newWorkspace(t, map[string]string{
		".gitignore": "gen/\n",
		".aiignore":  "notes.md\n",
		"gen/x.go":   "x",
		"notes.md":   "private",
		"visible.go": "v",
	})
	entries, err := r.List(".")
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if _, ok := byPath["gen"]; ok {
		t.Error("hidden dir listed")
	}
	if e, ok := byPath["notes.md"]; !ok || e.Visibility != shield.Stripped {
		t.Errorf("stripped file entry = %+v, want present+Stripped", e)
	}
	if e, ok := byPath["visible.go"]; !ok || e.Visibility != shield.Visible {
		t.Errorf("visible file entry = %+v", e)
	}
}

func TestReadRevalidatesChangedFile(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{"f.txt": "v1"})
	if _, err := r.Read("f.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "f.txt", "v2 is longer") // size change: no mtime-resolution luck needed
	got, err := r.Read("f.txt")
	if err != nil || string(got) != "v2 is longer" {
		t.Fatalf("Read after host edit = %q, %v; want fresh content", got, err)
	}
}

func TestReadDropsVanishedFile(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{"gone.txt": "x", "stay.txt": "y"})
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read("gone.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(vanished) err = %v, want ErrNotFound", err)
	}
}

func TestBinaryFilesAreMetadataOnly(t *testing.T) {
	_, r := newWorkspace(t, map[string]string{"img.png": "\x89PNG\x00\x00"})
	if _, err := r.Read("img.png"); !errors.Is(err, ErrNotText) {
		t.Fatalf("Read(binary) err = %v, want ErrNotText", err)
	}
	if e, err := r.Stat("img.png"); err != nil || e.Resident {
		t.Fatalf("Stat(binary) = %+v, %v; want non-resident metadata", e, err)
	}
}

func TestPerFileCap(t *testing.T) {
	root := t.TempDir()
	write(t, root, "big.txt", strings.Repeat("a", 2048))
	write(t, root, "ok.txt", "fine")
	r, err := New(root, Config{Budget: testBudget, MaxFileSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read("big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read(oversized) err = %v, want ErrTooLarge", err)
	}
	if _, err := r.Read("ok.txt"); err != nil {
		t.Fatalf("Read(ok) err = %v", err)
	}
}

func TestBootOverBudgetRefusesNamingOffenders(t *testing.T) {
	root := t.TempDir()
	write(t, root, "huge-one.txt", strings.Repeat("a", 600))
	write(t, root, "huge-two.txt", strings.Repeat("b", 600))
	_, err := New(root, Config{Budget: 1000, MaxFileSize: 800})
	if err == nil {
		t.Fatal("New over budget succeeded; want refusal")
	}
	if !strings.Contains(err.Error(), "budget") || !strings.Contains(err.Error(), "huge-") {
		t.Errorf("refusal must name the budget and an offender; got: %v", err)
	}
}

func TestMidSessionGrowthDeniesNewFileOnly(t *testing.T) {
	root := t.TempDir()
	write(t, root, "resident.txt", strings.Repeat("a", 400))
	r, err := New(root, Config{Budget: 500, MaxFileSize: 450})
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "grown.txt", strings.Repeat("b", 400)) // would blow the budget
	if _, err := r.Read("grown.txt"); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("Read(over-budget newcomer) err = %v, want ErrOverBudget", err)
	}
	if _, err := r.Read("resident.txt"); err != nil {
		t.Fatalf("existing resident must keep serving: %v", err)
	}
}

func TestRescanAppliesShieldChangesSilently(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{
		".aiignore": "# nothing yet\n",
		"soon.txt":  "readable for now",
	})
	if _, err := r.Read("soon.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, ".aiignore", "soon.txt\n")
	if err := r.Rescan(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read("soon.txt"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Read after reshield err = %v, want ErrForbidden", err)
	}
	if e, _ := r.Stat("soon.txt"); e.Visibility != shield.Stripped {
		t.Errorf("post-reshield visibility = %v, want Stripped", e.Visibility)
	}
}

func TestInvalidateAdmitsAndReloads(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{"a.txt": "one"})
	// Unknown path: a CREATE event.
	write(t, root, "b.txt", "two")
	r.Invalidate("b.txt")
	if got, err := r.Read("b.txt"); err != nil || string(got) != "two" {
		t.Fatalf("Read after Invalidate(create) = %q, %v", got, err)
	}
	// Known path: a MODIFY/MOVED_TO event.
	write(t, root, "a.txt", "one-changed")
	r.Invalidate("a.txt")
	if got, err := r.Read("a.txt"); err != nil || string(got) != "one-changed" {
		t.Fatalf("Read after Invalidate(modify) = %q, %v", got, err)
	}
}

func TestForEachResidentSkipsShieldedAndBinary(t *testing.T) {
	_, r := newWorkspace(t, map[string]string{
		".aiignore":  "secret.txt\n",
		"a.go":       "package a",
		"secret.txt": "no",
		"blob.bin":   "\x00\x01",
	})
	var seen []string
	if err := r.ForEachResident(func(rel string, content []byte) error {
		seen = append(seen, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(seen, ",")
	if strings.Contains(joined, "secret.txt") || strings.Contains(joined, "blob.bin") {
		t.Errorf("ForEachResident leaked shielded/binary content: %v", seen)
	}
	if !strings.Contains(joined, "a.go") || !strings.Contains(joined, ".aiignore") {
		t.Errorf("expected resident text files in %v", seen)
	}
}

func TestRescanCarriesUnchangedContentWithoutReload(t *testing.T) {
	root, r := newWorkspace(t, map[string]string{"keep.txt": "stable"})
	before, _ := r.Stat("keep.txt")
	if err := r.Rescan(); err != nil {
		t.Fatal(err)
	}
	after, err := r.Stat("keep.txt")
	if err != nil || !after.Resident || after.SHA256 != before.SHA256 {
		t.Fatalf("carried entry = %+v, %v; want identical resident state", after, err)
	}
	_ = root
}

func TestReportNamesLargestResidentExcludesRest(t *testing.T) {
	_, r := newWorkspace(t, map[string]string{
		".aiignore":  "secret.txt\n",
		"big.go":     strings.Repeat("a", 3000),
		"small.go":   strings.Repeat("b", 100),
		"medium.go":  strings.Repeat("c", 1500),
		"secret.txt": strings.Repeat("d", 5000), // shielded: heaviest on disk, never resident
		"blob.bin":   "\x00\x01\x02",            // binary: never resident
	})
	rep := r.Report(2)
	// Resident: big.go, small.go, medium.go, and .aiignore itself (ignore
	// files are always readable).  Excluded: the shielded secret and the
	// binary blob.
	if rep.Files != 4 {
		t.Fatalf("resident files = %d, want 4 (shielded + binary excluded, .aiignore kept)", rep.Files)
	}
	if len(rep.Largest) != 2 {
		t.Fatalf("topN not applied: %d entries", len(rep.Largest))
	}
	if rep.Largest[0].Path != "big.go" || rep.Largest[1].Path != "medium.go" {
		t.Fatalf("largest order = %v, want [big.go medium.go]", []string{rep.Largest[0].Path, rep.Largest[1].Path})
	}
	for _, e := range rep.Largest {
		if e.Path == "secret.txt" || e.Path == "blob.bin" {
			t.Errorf("Report leaked non-resident file %q", e.Path)
		}
	}
	if rep.Bytes <= 0 || rep.Budget != testBudget {
		t.Errorf("report totals = %+v", rep)
	}
}

func TestConfigFailsClosed(t *testing.T) {
	root := t.TempDir()
	if _, err := New(root, Config{Budget: 0, MaxFileSize: 100}); err == nil {
		t.Error("zero budget accepted")
	}
	if _, err := New(root, Config{Budget: 100, MaxFileSize: 0}); err == nil {
		t.Error("zero per-file cap accepted")
	}
	if _, err := New(root, Config{Budget: 100, MaxFileSize: 200}); err == nil {
		t.Error("per-file cap above budget accepted")
	}
}
