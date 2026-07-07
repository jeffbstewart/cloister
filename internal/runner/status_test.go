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

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/cellstate"
)

func TestStatusFileLifecycle(t *testing.T) {
	r := newTestRunner(t)
	statusPath := filepath.Join(t.TempDir(), "status.json")
	r.StatusPath = statusPath

	dir := t.TempDir()
	resCh := make(chan *Result, 1)
	go func() {
		res, _ := r.Run(context.Background(), Request{
			Action:  "long",
			Argv:    helperArgv("sleep", "10s"),
			Dir:     dir,
			Timeout: 700 * time.Millisecond,
			Env:     map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		})
		resCh <- res
	}()

	// While running: busy, with the action, a real run id, and a start time.
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := cellstate.Read(statusPath)
		if err == nil && st.Busy {
			if st.Active == nil || st.Active.Action != "long" ||
				st.Active.RunID.IsZero() || st.Active.StartedAt.IsZero() {
				t.Fatalf("busy status incomplete: %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status.json never showed the run as busy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	res := <-resCh

	st, err := cellstate.Read(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Busy || st.Active != nil {
		t.Errorf("finished status must be idle with no active run: %+v", st)
	}
	if st.LastRun == nil || st.LastRun.RunID != res.RunID ||
		st.LastRun.Status != string(StatusTimeout) || st.LastRun.Action != "long" {
		t.Errorf("lastRun = %+v, want %s/long/timeout", st.LastRun, res.RunID)
	}
	if st.UpdatedAt.IsZero() {
		t.Error("updatedAt not stamped")
	}
}

func TestStatusDisabledWithoutPath(t *testing.T) {
	r := newTestRunner(t) // StatusPath unset, Sink nil
	res := run(t, r, time.Minute, "exit", "0")
	if res.Status != StatusOK {
		t.Fatalf("status=%q, want ok", res.Status)
	}
	entries, err := os.ReadDir(r.LogsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "status.json" || e.Name() == "status.json.tmp" {
			t.Errorf("unexpected %s written with StatusPath unset", e.Name())
		}
	}
}
