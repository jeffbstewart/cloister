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

// Builder-action decisions.
const (
	DecisionRun                Decision = "run"
	DecisionRejectedParam      Decision = "rejected_param"
	DecisionRejectedBusy       Decision = "rejected_busy"
	DecisionRejectedNoManifest Decision = "rejected_no_manifest"
)

// CommandDetail is a builder command's record body: a manifest action
// invocation — build, test, coverage, lint, etc.
type CommandDetail struct {
	Params   map[string]string `json:"params,omitempty"` // the agent-suppliable inputs
	Argv     []string          `json:"argv,omitempty"`   // the fully resolved command run
	ExitCode *int              `json:"exitCode,omitempty"`
	LogPath  string            `json:"logPath,omitempty"`
	LogBytes int64             `json:"logBytes,omitempty"`
}
