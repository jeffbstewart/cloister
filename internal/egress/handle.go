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
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Handle is an opaque, session-scoped reference to one search result's URL.
// The model receives a handle in place of a URL it could mutate; extracting a
// handle resolves — inside the scholar — to the exact result URL, so no
// attacker-chosen data can ride an extract.  The "h_" prefix lets Extract
// tell a handle from a raw URL without ambiguity.  Handles live only in the
// request-scoped Session map; they are meaningless in any other session.
type Handle struct{ s string }

// newHandle mints a fresh opaque handle from crypto/rand.
func newHandle() (Handle, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Handle{}, fmt.Errorf("egress: entropy source failed: %w", err)
	}
	return Handle{s: "h_" + hex.EncodeToString(b[:])}, nil
}

// String returns the wire form handed to the model.
func (h Handle) String() string { return h.s }

// IsZero reports the empty handle.
func (h Handle) IsZero() bool { return h.s == "" }
