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

// The archivist's audit vocabulary (docs/archivist.md).  Remote
// operations are audited, ungated: every endpoint touch leaves a
// record, none waits for approval.  Local working-tree verbs are
// deliberately unaudited.  Identifiers and counts only, never content —
// the PR body, the diff, and the reply text live at the forge, not in
// the ledger.

// The archivist's decisions.
const (
	// DecisionRemoteOK: the remote operation completed.
	DecisionRemoteOK Decision = "remote_ok"
	// DecisionRemoteError: the endpoint or the transport failed.
	DecisionRemoteError Decision = "remote_error"
	// DecisionRemoteRefused: the archivist's own client-side refusal —
	// a default-branch push, a remote outside the endpoint table.  A
	// misconfigured ruleset or an over-scoped credential must not
	// become an incident, and every refusal is loud.
	DecisionRemoteRefused Decision = "remote_refused"
)

// RemoteDetail is one remote operation: against which endpoint,
// touching which branch or PR.  The verb itself is the record's Tool
// (Header), not repeated here.
type RemoteDetail struct {
	// Endpoint is the endpoint-table name ("github.com"); "" when the
	// operation was refused before the endpoint resolved.
	Endpoint string `json:"endpoint,omitempty"`
	// Branch is the line of work involved, when one is.
	Branch string `json:"branch,omitempty"`
	// PR is the pull-request number, when one is involved.
	PR int `json:"pr,omitempty"`
	// Target names the sub-object of the operation (a review-thread
	// comment id), when one is.
	Target string `json:"target,omitempty"`
}

// Kind marks RemoteDetail as the archivist's detail body.
func (*RemoteDetail) Kind() Kind { return KindRemote }
