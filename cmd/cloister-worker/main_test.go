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

package main

import (
	"slices"
	"testing"
)

func TestResolveRole(t *testing.T) {
	cases := []struct {
		name     string
		prog     string
		args     []string
		wantRole string
		wantArgs []string
		wantErr  bool
	}{
		{"role link", "scribe", []string{"-scribe-approvals"}, "scribe", []string{"-scribe-approvals"}, false},
		{"role link with .exe", "agency.exe", []string{"-status-dir", "/status"}, "agency", []string{"-status-dir", "/status"}, false},
		{"every role name resolves", "state-service", nil, "state-service", nil, false},
		{"generic with selector pair", "cloister-worker", []string{"-worker-mode", "builder", "-spool", "/s"}, "builder", []string{"-spool", "/s"}, false},
		{"generic with = form", "cloister-worker", []string{"-worker-mode=librarian"}, "librarian", []string{}, false},
		{"generic with double-dash = form", "cloister-worker", []string{"--worker-mode=scholar", "-answer-gate=false"}, "scholar", []string{"-answer-gate=false"}, false},
		{"compat name still selects", "agent-builder", []string{"-worker-mode", "agency"}, "agency", []string{}, false},
		{"compat healthcheck form", "agent-builder", []string{"-healthcheck", "-addr", ":9300"}, healthcheckName, []string{"-healthcheck", "-addr", ":9300"}, false},
		{"generic bare is an error", "cloister-worker", nil, "", nil, true},
		{"selector without value", "cloister-worker", []string{"-worker-mode"}, "", nil, true},
		{"unknown role", "cloister-worker", []string{"-worker-mode", "corrector"}, "", nil, true},
		{"healthcheck is not a mode", "cloister-worker", []string{"-worker-mode", "healthcheck"}, "", nil, true},
		{"selector must lead", "cloister-worker", []string{"-addr", ":9300", "-worker-mode", "scribe"}, "", nil, true},
		{"unrecognized name gets no implied role", "cloister-worker-v2", []string{"-spool", "/s"}, "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, roleArgs, err := resolveRole(tc.prog, tc.args)
			if tc.wantErr != (err != nil) {
				t.Fatalf("resolveRole err = %v, wantErr = %v", err, tc.wantErr)
			}
			if role != tc.wantRole {
				t.Errorf("role = %q, want %q", role, tc.wantRole)
			}
			if !tc.wantErr && !slices.Equal(roleArgs, tc.wantArgs) {
				t.Errorf("roleArgs = %q, want %q", roleArgs, tc.wantArgs)
			}
		})
	}
}

// TestEveryRoleHasAParser: the table drives both argv[0] dispatch and
// -worker-mode, so a role missing from it is unreachable.
func TestEveryRoleHasAParser(t *testing.T) {
	for _, name := range []string{"builder", "state-service", "scribe", "scholar", "librarian", "agency"} {
		if roles[name] == nil {
			t.Errorf("role %q has no parser", name)
		}
	}
	if roles[healthcheckName] != nil {
		t.Errorf("%q must stay a pseudo-role, not a -worker-mode value", healthcheckName)
	}
}

// TestWrongRoleFlagIsAnError is the point of the per-role flag sets: a
// flag belonging to a different role no longer parses as a silent no-op.
func TestWrongRoleFlagIsAnError(t *testing.T) {
	cases := []struct {
		role string
		args []string
	}{
		{"builder", []string{"-status-dir", "/status"}},       // agency flag
		{"scribe", []string{"-policy", "/p"}},                 // scholar flag
		{"scholar", []string{"-scribe-approvals"}},            // scribe flag
		{"librarian", []string{"-state-dir", "/state"}},       // state-service flag
		{"state-service", []string{"-rescan-interval", "1m"}}, // librarian flag
		{"agency", []string{"-workspace", "/w"}},              // the door holds no workspace
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			if _, err := roles[tc.role](tc.args); err == nil {
				t.Errorf("%s accepted %q, want a parse error", tc.role, tc.args)
			}
		})
	}
}

// TestRoleParsersAcceptTheirOwnFlags: each role's committed compose
// invocation (and the bare default form) must parse.  Parsing returns the
// deferred action without running it, so no server starts here.
func TestRoleParsersAcceptTheirOwnFlags(t *testing.T) {
	cases := []struct {
		role string
		args []string
	}{
		{"builder", nil},
		{"builder", []string{"-mark-warmed"}},
		{"state-service", nil},
		{"scribe", []string{"-scribe-approvals"}},
		{"scholar", nil},
		{"librarian", nil},
		{"agency", []string{"-status-dir", "/status"}},
		{"scribe", []string{"-healthcheck"}},
		{"agency", []string{"-healthcheck"}},
	}
	for _, tc := range cases {
		run, err := roles[tc.role](tc.args)
		if err != nil {
			t.Errorf("%s%v: %v", tc.role, tc.args, err)
			continue
		}
		if run == nil {
			t.Errorf("%s%v: nil action", tc.role, tc.args)
		}
	}
}
