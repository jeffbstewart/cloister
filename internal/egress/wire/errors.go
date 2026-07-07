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

package wire

import "errors"

// ErrResponseTooBig reports an upstream body over the configured cap.  It
// lives in wire (not the core egress package) because doCapped raises it and
// the search/extract providers compare against it; callers use errors.Is.
var ErrResponseTooBig = errors.New("egress: upstream response exceeds the configured cap")
