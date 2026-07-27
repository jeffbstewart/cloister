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
	"sync"
	"time"
)

// extractCache remembers recent successful extractions, so a research loop
// that revisits a page is served from memory: no provider spend, and a raw
// URL the operator already approved is not re-prompted while its content
// lives here.  Only SUCCESSES are stored — a denied or refused target can
// never be served from the cache, and callers re-run the policy gates
// before consulting it, so a deny added after a fetch wins immediately.
// Refusals are likewise never remembered: a previously denied request
// re-prompts every time.
type extractCache struct {
	mu       sync.Mutex
	maxBytes int64         // total content budget; <= 0 disables the cache
	ttl      time.Duration // entry lifetime
	entries  map[string]*cacheEntry
	total    int64
}

type cacheEntry struct {
	ext      Extracted
	storedAt time.Time // expiry is measured from the fetch, not last use
	lastUsed time.Time // the byte budget evicts least-recently-used first
	bytes    int64
}

func newExtractCache(maxBytes int64, ttl time.Duration) *extractCache {
	return &extractCache{maxBytes: maxBytes, ttl: ttl, entries: map[string]*cacheEntry{}}
}

// get returns the live cached extraction for url, marked Cached, or reports
// a miss.  An expired entry is dropped on sight.
func (c *extractCache) get(url string, now time.Time) (Extracted, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[url]
	if !ok {
		return Extracted{}, false
	}
	if now.Sub(e.storedAt) >= c.ttl {
		c.total -= e.bytes
		delete(c.entries, url)
		return Extracted{}, false
	}
	e.lastUsed = now
	ext := e.ext
	ext.Cached = true
	return ext, true
}

// put stores a successful extraction, evicting least-recently-used entries
// until the byte budget holds.  Content larger than the whole budget is
// simply not cached.
func (c *extractCache) put(url string, ext Extracted, now time.Time) {
	bytes := int64(len(ext.Markdown) + len(ext.FinalURL))
	if c.maxBytes <= 0 || bytes > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[url]; ok {
		c.total -= old.bytes
	}
	c.entries[url] = &cacheEntry{ext: ext, storedAt: now, lastUsed: now, bytes: bytes}
	c.total += bytes
	for c.total > c.maxBytes {
		var oldestURL string
		var oldest *cacheEntry
		for u, e := range c.entries {
			if u == url {
				continue // never evict what we just stored
			}
			if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
				oldestURL, oldest = u, e
			}
		}
		if oldest == nil {
			return
		}
		c.total -= oldest.bytes
		delete(c.entries, oldestURL)
	}
}
