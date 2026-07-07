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

package cellstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	in := Status{
		Busy:   true,
		Active: &ActiveRun{RunID: id, Action: "test", StartedAt: time.Now().UTC()},
	}
	if err := WriteFile(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Busy || out.Active == nil || out.Active.RunID != id || out.Active.Action != "test" {
		t.Errorf("round trip mismatch: %+v", out)
	}
	if out.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not stamped by WriteFile")
	}
}

// TestWriterStampsUpdatedAt: a client-supplied UpdatedAt must be replaced
// with the writer's clock — times in the status document are never trusted
// from the producer.  The injected Clock makes the constraint exact: the
// stored value must equal the clock's instant, not the forged one.
func TestWriterStampsUpdatedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	forged := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	instant := time.Date(2026, 7, 2, 18, 4, 11, 0, time.UTC)
	fixed := Clock(func() time.Time { return instant })

	if err := WriteFileWithClock(path, Status{UpdatedAt: forged}, fixed); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !out.UpdatedAt.Equal(instant) {
		t.Errorf("UpdatedAt = %s, want the writer clock's %s (client value must be discarded)",
			out.UpdatedAt, instant)
	}
}

func TestWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFile(filepath.Join(dir, "status.json"), Status{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "status.json" {
		t.Errorf("dir contents = %v, want only status.json", entries)
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "status.json")); err == nil {
		t.Error("Read of a missing file must error so callers can degrade")
	}
}
