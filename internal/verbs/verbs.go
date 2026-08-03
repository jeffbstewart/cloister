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

// Package verbs is the single source of truth for the archivist's MCP
// tool NAMES.
//
// It exists because those names are spoken in three places that a
// compiler would not otherwise connect: the archivist registers them
// (internal/archivist), the git proxy names them in refusals telling an
// agent what to call instead (cmd/git-proxy), and the stock environment
// prompt tabulates them for the agent to read
// (docker/workbench/AGENTS.md).  Renaming a tool used to leave the
// other two pointing at something that no longer exists — discovered by
// a confused agent mid-task rather than by a build.
//
// Deliberately a LEAF package: constants and nothing else, no imports.
// The git proxy is a small static binary installed as /usr/bin/git, and
// importing internal/archivist to reach these names would drag the MCP
// SDK, internal/archive, and internal/forge into it.
//
// # Changing this set
//
// A rename is now a compile error at every Go call site, and a removal
// breaks the build.  What the compiler CANNOT see is the prompt file
// and the proxy's prose, so after touching anything here run:
//
//	go build ./...
//	go test ./internal/archivist/ ./cmd/git-proxy/
//
// and expect these to be the ones that speak up:
//
//   - TestEveryRegisteredToolHasAConstant (internal/archivist) —
//     the archivist registered a tool this package does not name, or
//     named one it does not register.  Fails on an addition, too:
//     a new tool belongs here before it belongs anywhere else.
//   - TestAgentsPromptNamesRealTools (internal/archivist) — the
//     stock prompt's table names a tool that does not exist.  This is
//     the one no compiler can reach, and the one the agent reads first.
//   - TestPorcelainCoverage (cmd/git-proxy) — unrelated to renames,
//     but it is how a git upgrade announces new commands, and it lives
//     next door.
//
// Adding a tool: add the constant, register it, and add a row to the
// prompt's table if an agent would otherwise reach for git instead.
// Removing one: delete the constant and let the compiler find the
// mentions.
package verbs

// The AGENT surface: the verbs a coding agent can name
// (docs/archivist.md, "Two surfaces").
const (
	// Reads.
	CurrentState   = "current_state"
	History        = "history"
	PendingChanges = "pending_changes"
	ShowChange     = "show_change"
	FileAt         = "file_at"

	// Lines of work.
	StartWork   = "start_work"
	SwitchWork  = "switch_work"
	AbandonWork = "abandon_work"

	// Recording and rolling back.
	Checkpoint = "checkpoint"
	Restore    = "restore"
	SetAside   = "set_aside"
	Resume     = "resume"

	// Integration and the remote.
	SyncFromUpstream = "sync_from_upstream"
	Publish          = "publish"
	Propose          = "propose"
	CheckProgress    = "check_progress"
	ReadReviews      = "read_reviews"
	ReplyToReview    = "reply_to_review"
	AwaitReview      = "await_review"
)

// The OPERATOR surface: the workspace's boundary events, which the
// agent is not connected to and cannot name.  Held here for the same
// registration check, not because anything agent-facing should mention
// them.
const (
	Provision      = "provision"
	Dispose        = "dispose"
	WorkspaceState = "workspace_state"
)

// Agent is every tool on the agent surface, for the tests that compare
// this package against what the archivist actually registers.
var Agent = []string{
	CurrentState, History, PendingChanges, ShowChange, FileAt,
	StartWork, SwitchWork, AbandonWork,
	Checkpoint, Restore, SetAside, Resume,
	SyncFromUpstream, Publish, Propose, CheckProgress, ReadReviews,
	ReplyToReview, AwaitReview,
}

// Operator is every tool on the operator surface.
var Operator = []string{Provision, Dispose, WorkspaceState}
