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

// Package runid provides the first-class run identifier shared by the
// runner, digest, audit, and MCP layers.
//
// An ID wraps a version-7 UUID (RFC 9562): 48 bits of unix-millisecond
// timestamp, a 12-bit monotonic counter (randomly seeded each tick), then
// 62 random bits from crypto/rand.  That keeps the properties a run
// identifier needs — sortable (time-prefixed, and strictly increasing
// within a process even inside one millisecond), meaningless,
// collision-free — and the canonical form uses only [0-9a-f-]: no shell
// metacharacters, no spaces, no path separators, so an ID is safe to embed
// in argv, file names, and log lines.
//
// The underlying string is private: the only ways to obtain a non-zero ID
// are the New factory and the validating Parse — a raw string can never be
// coerced into an ID.
package runid

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// ID is a run identifier.  The zero value means "no run" and is never
// produced by New or Parse.
type ID struct {
	s string
}

// idRE accepts exactly canonical lowercase UUIDv7 (version nibble 7,
// RFC 9562 variant).  The strict alphabet is the shell-safety and
// path-traversal guarantee for untrusted input.
var idRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// v7state makes ids strictly increasing within this process: consumers sort
// on the canonical string (e.g. oldest-first transcript pruning), and the
// 48-bit timestamp alone cannot order ids minted in the same millisecond.
// The rand_a field carries a 12-bit counter (RFC 9562 §6.2 method 1),
// reseeded from crypto/rand each new tick with the top bit clear so at
// least 2048 increments fit before overflow borrows the next millisecond.
var v7state struct {
	sync.Mutex
	ms  uint64
	seq uint16
}

// now is the wall clock behind New; tests swap it to pin the millisecond
// tick and to simulate clock regression.
var now = time.Now

// New returns a fresh ID.  Collision resistance comes from 62 bits of
// crypto/rand entropy per id plus the randomly seeded counter.  The error
// path exists for platforms whose entropy source is unusable; since Go 1.24
// crypto/rand documents that Read never fails, so in practice the error is
// always nil.
func New() (ID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ID{}, fmt.Errorf("runid: entropy source failed: %w", err)
	}
	seed := (uint16(b[6])&0x07)<<8 | uint16(b[7])
	tick := uint64(now().UnixMilli())
	v7state.Lock()
	if tick > v7state.ms {
		v7state.ms = tick
		v7state.seq = seed
	} else { // same tick, or the clock stepped back: count, never regress
		v7state.seq++
		if v7state.seq > 0x0fff {
			v7state.ms++
			v7state.seq = seed
		}
	}
	ms, seq := v7state.ms, v7state.seq
	v7state.Unlock()
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte(seq>>8)  // version 7 + counter high nibble
	b[7] = byte(seq)            // counter low byte
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 9562 variant
	return ID{s: fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])}, nil
}

// Parse validates an untrusted string (e.g. a get_log argument) and returns
// it as an ID. Anything outside canonical form is rejected, never coerced.
func Parse(s string) (ID, error) {
	if !idRE.MatchString(s) {
		return ID{}, fmt.Errorf("invalid run id %q", s)
	}
	return ID{s: s}, nil
}

// String returns the canonical form ("" for the zero ID).
func (id ID) String() string { return id.s }

// IsZero reports whether id is the "no run" zero value.
func (id ID) IsZero() bool { return id.s == "" }

// Shard returns the last two hex characters of the canonical form — a uniform
// 256-way bucket key.  A UUIDv7 keeps its millisecond timestamp in the LEADING
// bytes, so any content store must shard on the random TAIL, never a prefix, or
// everything hot-buckets for decades.  The zero ID shards to "00".
func (id ID) Shard() string {
	if id.s == "" {
		return "00"
	}
	return id.s[len(id.s)-2:]
}

// MarshalJSON emits the canonical form as a JSON string.
func (id ID) MarshalJSON() ([]byte, error) { return json.Marshal(id.s) }

// UnmarshalJSON accepts a canonical ID or the empty string (zero ID);
// anything else is rejected, so validation survives round-trips.
func (id *ID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*id = ID{}
		return nil
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*id = v
	return nil
}
