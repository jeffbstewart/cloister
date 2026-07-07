package sink

import (
	"sync"
	"time"
)

// bucket is a minimal token bucket.  Implemented here because the
// dependency policy rules out golang.org/x/time/rate.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens replenished per second
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(rate, burst float64) *bucket {
	return &bucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// refill credits tokens for elapsed time.  Callers hold b.mu.
func (b *bucket) refill(now time.Time) {
	b.tokens += b.rate * now.Sub(b.last).Seconds()
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
}

// allow consumes n tokens if available, without blocking — used for
// request-count limits where the answer is 429.
func (b *bucket) allow(n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(time.Now())
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// take blocks until n tokens have been consumed — used to pace log-stream
// bytes so a hostile writer cannot outrun the disk. n may exceed burst; it
// is drawn down in installments as tokens accrue.
func (b *bucket) take(n float64) {
	for n > 0 {
		b.mu.Lock()
		b.refill(time.Now())
		grab := b.tokens
		if grab > n {
			grab = n
		}
		if grab > 0 {
			b.tokens -= grab
			n -= grab
		}
		b.mu.Unlock()
		if n > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
}
