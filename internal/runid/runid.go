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
// timestamp followed by 74 random bits from crypto/rand.  That keeps the
// properties a run identifier needs — sortable (time-prefixed), meaningless,
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

// New returns a fresh ID. Collision resistance comes from 74 bits of
// crypto/rand entropy per millisecond tick.  The error path exists for
// platforms whose entropy source is unusable; since Go 1.24 crypto/rand
// documents that Read never fails, so in practice the error is always nil.
func New() (ID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ID{}, fmt.Errorf("runid: entropy source failed: %w", err)
	}
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
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
