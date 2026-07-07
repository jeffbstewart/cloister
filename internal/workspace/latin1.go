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

package workspace

import "unicode/utf8"

// Latin-1 view for the permit_non_utf8 edit path.  Why this exists: ordinary
// (unapproved) edit operations are restricted to valid UTF-8, so a file that
// already contains invalid bytes could never be repaired through that door —
// the engine could not even represent its content.  This view is the repair
// mechanism, and it is only reachable with operator approval (the
// permit_non_utf8 flag is approval-gated).  Decoding maps each byte to the
// code point of the same value — a lossless byte↔code-point bijection — so an
// already-malformed file (the em-dash bug: a lone Windows-1252 0x97) becomes
// an addressable string the diff/replace engine can match and edit.

// Latin1Decode views raw bytes as Latin-1: byte b → rune(b).  Any file, valid
// UTF-8 or not, round-trips losslessly through Decode→BytesFromView untouched.
func Latin1Decode(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// BytesFromView re-encodes an edited view.  A code point ≤ 0xFF becomes its single
// byte, so existing content (and any byte the edit left alone) is preserved
// exactly; a higher code point — a real character the edit introduced, e.g. a
// proper em-dash U+2014 replacing the bad byte — is UTF-8 encoded.  That is what
// repairs a malformed file into correct UTF-8.
func BytesFromView(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r <= 0xFF {
			out = append(out, byte(r))
		} else {
			out = utf8.AppendRune(out, r)
		}
	}
	return out
}
