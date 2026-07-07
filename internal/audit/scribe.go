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

package audit

// MutationDetail is a scribe workspace-mutation's record body.  A
// single-target op sets Path; a move/copy sets From and To.
type MutationDetail struct {
	Path          string `json:"path,omitempty"` // workspace-relative target
	From          string `json:"from,omitempty"` // move/copy source
	To            string `json:"to,omitempty"`   // move/copy destination
	BytesBefore   int64  `json:"bytesBefore,omitempty"`
	BytesAfter    int64  `json:"bytesAfter,omitempty"`
	FilesTouched  int    `json:"filesTouched,omitempty"`
	LinesAdded    int    `json:"linesAdded,omitempty"`
	LinesRemoved  int    `json:"linesRemoved,omitempty"`
	SHA256After   string `json:"sha256After,omitempty"`
	HasDiff       bool   `json:"hasDiff,omitempty"`       // a diff payload is stored for this opId
	DiffTruncated bool   `json:"diffTruncated,omitempty"` // that payload was capped for size
}

// Kind marks MutationDetail as the scribe's detail body.
func (*MutationDetail) Kind() Kind { return KindMutation }
