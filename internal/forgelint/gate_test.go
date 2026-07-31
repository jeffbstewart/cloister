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

package forgelint

import (
	"slices"
	"testing"
)

// botCleanVerdicts is what Check returns for a compliant repository read
// with the bot's own token: R1 and R6 come back UNVERIFIED (the bypass
// roster and secrets inventory are admin-only), everything else OK.
func botCleanVerdicts() []Verdict {
	return []Verdict{
		{"R1", Unverified, "bypass roster unreadable"},
		{"R2", OK, ""},
		{"R3", OK, ""},
		{"R4", OK, ""},
		{"R5", OK, "bot is a write collaborator"},
		{"R6", Unverified, "secrets inventory unreadable"},
		{"R7", OK, ""},
		{"R8", OK, ""},
	}
}

func blockingReqs(r GateResult) []string {
	reqs := make([]string, 0, len(r.Blocking))
	for _, v := range r.Blocking {
		reqs = append(reqs, v.Req)
	}
	return reqs
}

func TestGatePassesCleanBotSnapshot(t *testing.T) {
	r := Gate(botCleanVerdicts())
	if !r.OK {
		t.Fatalf("clean bot snapshot blocked on %v", blockingReqs(r))
	}
}

func TestGateBlocks(t *testing.T) {
	tests := map[string]struct {
		mutate    func([]Verdict) []Verdict
		wantBlock []string
	}{
		"a violation anywhere blocks": {
			func(v []Verdict) []Verdict { v[1] = Verdict{"R2", Violation, "stale approvals survive"}; return v },
			[]string{"R2"},
		},
		"unverified outside the residue blocks": {
			func(v []Verdict) []Verdict { v[7] = Verdict{"R8", Unverified, "probe unreadable"}; return v },
			[]string{"R8"},
		},
		"unreadable bot role blocks (R5 not tolerable)": {
			func(v []Verdict) []Verdict { v[4] = Verdict{"R5", Unverified, "role unreadable"}; return v },
			// R5 blocks on its own; R1's residue also blocks because the
			// non-admin premise is no longer established.
			[]string{"R1", "R5"},
		},
		"admin bot blocks, and R1 residue loses its footing": {
			func(v []Verdict) []Verdict { v[4] = Verdict{"R5", Violation, "bot is admin"}; return v },
			[]string{"R1", "R5"},
		},
		"R6 violation (unguarded workflows) blocks": {
			func(v []Verdict) []Verdict { v[5] = Verdict{"R6", Violation, "workflows unguarded"}; return v },
			[]string{"R6"},
		},
	}
	for name, tc := range tests {
		r := Gate(tc.mutate(botCleanVerdicts()))
		if r.OK {
			t.Errorf("%s: gate passed, want refusal", name)
			continue
		}
		if got := blockingReqs(r); !slices.Equal(got, tc.wantBlock) {
			t.Errorf("%s: blocking = %v, want %v", name, got, tc.wantBlock)
		}
	}
}
