// Package cellstate defines the live-status document of a builder cell —
// shared by the runner (producer), the state service (owner), and the
// status pages (renderer).  It is a leaf package so all three can import it
// without cycles.
package cellstate

import (
	"encoding/json"
	"os"
	"time"

	"github.com/jeffbstewart/cloister/internal/runid"
)

// ActiveRun describes the run currently occupying the queue.
type ActiveRun struct {
	RunID     runid.ID  `json:"runId"`
	Action    string    `json:"action"`
	StartedAt time.Time `json:"startedAt"`
}

// RunSummary is one line of run history.  It carries the action name
// because the ID itself is deliberately meaningless.
type RunSummary struct {
	RunID  runid.ID `json:"runId"`
	Action string   `json:"action"`
	Status string   `json:"status"`
}

// Status is the queue state written on every transition, so observers can
// watch builds without any network path into the jail.
type Status struct {
	Busy      bool        `json:"busy"`
	Active    *ActiveRun  `json:"active,omitempty"`
	LastRun   *RunSummary `json:"lastRun,omitempty"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// Clock supplies the current instant.  Production code uses SystemClock;
// tests inject a fixed one to pin timestamp behavior.
type Clock func() time.Time

// SystemClock is the production Clock.
func SystemClock() time.Time { return time.Now().UTC() }

// WriteFile atomically replaces the status file (write temp + rename) so a
// reader never sees a torn document, stamping UpdatedAt with the writer's
// clock — the receiving side never trusts a client-supplied time.
func WriteFile(path string, st Status) error {
	return WriteFileWithClock(path, st, SystemClock)
}

// WriteFileWithClock is WriteFile with an injected Clock.
func WriteFileWithClock(path string, st Status, clock Clock) error {
	st.UpdatedAt = clock().UTC()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Read loads a status file written by WriteFile.  Callers decide how to
// treat a missing file (a cell that has never run is not an error).
func Read(path string) (Status, error) {
	var st Status
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}
