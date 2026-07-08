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

package shield

// AIReadable is file content the shield has cleared for the AI to read: the
// content analog of workspace.Path.  It is minted only by Shield.Clear, never
// by a literal, so holding a non-zero AIReadable is proof the shield authorized
// this path's content.  The unexported fields make a non-zero value unforgeable
// outside this package — content cannot reach a model prompt without first
// passing the shield.
type AIReadable struct {
	path string
	data []byte
}

// Clear is the sole minter of an AIReadable.  It returns ok=false (and a zero
// value) unless the shield permits reading rel's content; the construction IS
// the check, so a non-zero result is a capability, not a claim.  Content is
// always a file, so isDir is false in the visibility test.
func (s *Shield) Clear(rel string, data []byte) (AIReadable, bool) {
	if !s.MayRead(rel, false) {
		return AIReadable{}, false
	}
	return AIReadable{path: rel, data: data}, true
}

// String returns the cleared content as a string.
func (a AIReadable) String() string { return string(a.data) }

// Bytes returns the cleared content.  The slice is shared with the minting
// source and must not be mutated — the same contract the repo's content holds.
func (a AIReadable) Bytes() []byte { return a.data }

// Path returns the root-relative path whose content this cleared.
func (a AIReadable) Path() string { return a.path }

// IsZero reports the never-cleared zero value.
func (a AIReadable) IsZero() bool { return a.path == "" && a.data == nil }
