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

//go:build !linux

package watch

import "errors"

// ErrUnsupported: no watcher on this platform; run rescan-only.
var ErrUnsupported = errors.New("watch: not supported on this platform (rescan-only mode)")

// Watcher is unavailable on this platform.
type Watcher struct{}

// New reports ErrUnsupported.
func New(root string, shouldDescend func(rel string) bool, onChange func(rel string), onOverflow func()) (*Watcher, error) {
	return nil, ErrUnsupported
}

// Close is a no-op for the zero Watcher.
func (w *Watcher) Close() error { return nil }
