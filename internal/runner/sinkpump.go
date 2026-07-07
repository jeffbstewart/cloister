package runner

import (
	"errors"
	"io"
	"sync"
	"time"
)

// errSinkIncomplete signals that the sink's live copy of a log is missing
// bytes (dropped under backpressure, or a stream write failed), so the
// caller must reconcile from the complete local spool.
var errSinkIncomplete = errors.New("sink log copy incomplete")

// livePump forwards build output to the sink stream WITHOUT ever blocking
// the build.  Writes that outpace the sink (a slow, paced, or broken sink)
// are dropped rather than backpressured onto the build's output pipe; the
// complete local spool is reconciled at finalize.  So the live tail is
// best-effort and the finalized log is authoritative — exactly the
// observability contract: never fail or stall a build for a watcher.
type livePump struct {
	w       io.WriteCloser
	buf     chan []byte
	wg      sync.WaitGroup
	mu      sync.Mutex
	dropped bool
}

// pumpDepth bounds buffered chunks (~64 KB each from os/exec) → a few MB.
const pumpDepth = 64

// pumpDrainGrace bounds how long Close waits for the pump goroutine to
// finish.  A sink stream whose Write wedges (dead peer) must not stall run
// completion: past the grace, the goroutine is abandoned and the run is
// reconciled from the local spool.
const pumpDrainGrace = 2 * time.Second

func newLivePump(w io.WriteCloser) *livePump {
	p := &livePump{w: w, buf: make(chan []byte, pumpDepth)}
	p.wg.Add(1)
	go p.run()
	return p
}

func (p *livePump) run() {
	defer p.wg.Done()
	for b := range p.buf {
		if _, err := p.w.Write(b); err != nil {
			p.markDropped()
			for range p.buf { // keep draining so Write never blocks
			}
			return
		}
	}
}

// Write copies p (os/exec reuses its buffer) and hands it to the pump,
// dropping on a full queue.  It never returns an error: a failed sink must
// not fail the run.
func (p *livePump) Write(b []byte) (int, error) {
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case p.buf <- cp:
	default:
		p.markDropped()
	}
	return len(b), nil
}

func (p *livePump) markDropped() {
	p.mu.Lock()
	p.dropped = true
	p.mu.Unlock()
}

// Close drains the pump, closes the underlying stream, and returns
// errSinkIncomplete if anything was dropped or the stream errored — the
// signal to reconcile from the local spool.  Closing the stream first
// unblocks a well-behaved pending Write (e.g. an io.Pipe); a wedged stream
// is abandoned after pumpDrainGrace so run completion never hangs.
func (p *livePump) Close() error {
	close(p.buf)
	streamErr := p.w.Close()

	drained := make(chan struct{})
	go func() { p.wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(pumpDrainGrace):
		p.markDropped() // goroutine still wedged in Write; abandon it
	}

	p.mu.Lock()
	dropped := p.dropped
	p.mu.Unlock()
	if streamErr != nil || dropped {
		return errSinkIncomplete
	}
	return nil
}
