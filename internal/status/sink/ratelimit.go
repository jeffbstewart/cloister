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
