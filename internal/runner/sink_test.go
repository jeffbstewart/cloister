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
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/cellstate"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// fakeSink records everything the runner sends, and can simulate a stream
// that rejects writes (to exercise the reconcile-from-spool path).
type fakeSink struct {
	mu         sync.Mutex
	streamBody bytes.Buffer
	reuploaded map[runid.ID][]byte
	finalized  []runid.ID
	statuses   []cellstate.Status
	failStream bool
}

func newFakeSink() *fakeSink {
	return &fakeSink{reuploaded: map[runid.ID][]byte{}}
}

type fakeStream struct {
	s      *fakeSink
	failed bool
}

func (s *fakeSink) StartRun(id runid.ID) io.WriteCloser {
	return &fakeStream{s: s, failed: s.failStream}
}

func (w *fakeStream) Write(p []byte) (int, error) {
	if w.failed {
		return 0, errors.New("stream down")
	}
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return w.s.streamBody.Write(p)
}

func (w *fakeStream) Close() error { return nil }

func (s *fakeSink) Reupload(id runid.ID, log io.Reader) error {
	b, _ := io.ReadAll(log)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reuploaded[id] = b
	return nil
}

func (s *fakeSink) Finalize(id runid.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = append(s.finalized, id)
	return nil
}

func (s *fakeSink) PutStatus(st cellstate.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, st)
	return nil
}

func TestSinkStreamsAndFinalizes(t *testing.T) {
	sink := newFakeSink()
	r := newTestRunner(t)
	r.Sink = sink

	res := run(t, r, time.Minute, "lines", "50")
	if res.Status != StatusOK {
		t.Fatalf("status=%q, want ok", res.Status)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !strings.Contains(sink.streamBody.String(), "line 50") {
		t.Errorf("sink did not receive streamed output; got %q", sink.streamBody.String())
	}
	if len(sink.finalized) != 1 || sink.finalized[0] != res.RunID {
		t.Errorf("finalized = %v, want [%s]", sink.finalized, res.RunID)
	}
	if _, ok := sink.reuploaded[res.RunID]; ok {
		t.Error("healthy stream should not trigger a reconcile reupload")
	}
	// Status: busy then idle, in that order.
	if len(sink.statuses) < 2 {
		t.Fatalf("want at least 2 status posts, got %d", len(sink.statuses))
	}
	if !sink.statuses[0].Busy {
		t.Error("first status post should be busy")
	}
	if sink.statuses[len(sink.statuses)-1].Busy {
		t.Error("last status post should be idle")
	}
}

// TestSinkReconcilesOnStreamFailure: when the live stream drops, the
// runner reuploads the complete local spool so durable history is whole.
func TestSinkReconcilesOnStreamFailure(t *testing.T) {
	sink := newFakeSink()
	sink.failStream = true
	r := newTestRunner(t)
	r.Sink = sink

	res := run(t, r, time.Minute, "lines", "30")
	if res.Status != StatusOK {
		t.Fatalf("status=%q, want ok", res.Status)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	body, ok := sink.reuploaded[res.RunID]
	if !ok {
		t.Fatal("stream failure did not trigger a reconcile reupload")
	}
	if !strings.Contains(string(body), "line 30") {
		t.Errorf("reuploaded spool incomplete: %q", body)
	}
	if len(sink.finalized) != 1 {
		t.Errorf("run not finalized after reconcile: %v", sink.finalized)
	}
}

// TestSinkBackpressureDoesNotStallBuild: a sink that blocks forever must
// not hang the build — the pump drops and the run still completes promptly.
func TestSinkBackpressureDoesNotStallBuild(t *testing.T) {
	r := newTestRunner(t)
	r.Sink = blockingSink{}

	start := time.Now()
	res := run(t, r, 30*time.Second, "lines", "5000")
	if res.Status != StatusOK {
		t.Fatalf("status=%q, want ok", res.Status)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("build took %s; a blocked sink stalled it", elapsed)
	}
}

type blockingSink struct{}

func (blockingSink) StartRun(runid.ID) io.WriteCloser   { return blockingStream{} }
func (blockingSink) Reupload(runid.ID, io.Reader) error { return nil }
func (blockingSink) Finalize(runid.ID) error            { return nil }
func (blockingSink) PutStatus(cellstate.Status) error   { return nil }

type blockingStream struct{}

func (blockingStream) Write(p []byte) (int, error) {
	select {} // never returns; the pump must not care
}
func (blockingStream) Close() error { return nil }
