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

package egress

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLedgerRecordAndCount(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	l, err := OpenLedger(filepath.Join(t.TempDir(), "l"), 48*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Record(now); err != nil {
			t.Fatal(err)
		}
	}
	dayStart := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	if got := l.CountSince(dayStart); got != 3 {
		t.Errorf("CountSince = %d, want 3", got)
	}
	// yesterday's cutoff still counts today's 3
	if got := l.CountSince(dayStart.Add(-24 * time.Hour)); got != 3 {
		t.Errorf("CountSince(yesterday) = %d, want 3", got)
	}
}

// TestLedgerSurvivesReload is the "caps hold across a simulated restart"
// acceptance: records persist to disk and reload into a fresh Ledger.
func TestLedgerSurvivesReload(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "l")
	l1, err := OpenLedger(path, 48*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := l1.Record(now); err != nil {
			t.Fatal(err)
		}
	}
	l2, err := OpenLedger(path, 48*time.Hour, now) // reopen == restart
	if err != nil {
		t.Fatal(err)
	}
	dayStart := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	if got := l2.CountSince(dayStart); got != 4 {
		t.Errorf("after reload CountSince = %d, want 4 (cap must survive a restart)", got)
	}
}

func TestLedgerPrunesOldOnLoad(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	// One recent, one ancient (unsorted on purpose).
	ancient := now.Add(-100 * time.Hour).Unix()
	recent := now.Add(-1 * time.Hour).Unix()
	path := filepath.Join(t.TempDir(), "l")
	// recent BEFORE ancient — unsorted on disk, to exercise the sort-on-load.
	body := strconv.FormatInt(recent, 10) + "\n" + strconv.FormatInt(ancient, 10) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(path, 48*time.Hour, now) // retention 48h drops the ancient one
	if err != nil {
		t.Fatal(err)
	}
	if got := l.CountSince(now.Add(-48 * time.Hour)); got != 1 {
		t.Errorf("CountSince = %d, want 1 (ancient pruned, order-independent)", got)
	}
}
