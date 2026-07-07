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

// Package watch is the librarian's recursive filesystem watcher
// (docs/librarian.md): raw stdlib inotify on Linux — where the cell
// containers run, and where the 2026-07-07 spike proved container
// writers' events arrive with full fidelity — and ErrUnsupported
// everywhere else, degrading the owner to rescan-only freshness.
// Correctness never depends on the watcher: the minute rescan and
// stat-on-access revalidation carry it; events only make the model
// fresher.
package watch

import "errors"

// ErrUnsupported: no watcher on this platform; the owner runs
// rescan-only.  Declared here, platform-agnostic, because callers
// errors.Is against it on every platform — including linux, where New
// never returns it.
var ErrUnsupported = errors.New("watch: not supported on this platform (rescan-only mode)")
