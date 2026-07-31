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

// The provision gate (docs/grange.md, "Provision-time verification").  The
// archivist runs Check over a ProvisionSnapshot — the bot-credential subset
// — and Gate decides whether the grange may be handed over.  Because the
// bot cannot read the operator lint's full picture, some requirements come
// back UNVERIFIED even for a compliant repository; the gate's job is to
// distinguish the residue that is genuinely unknowable to a plain writer
// (and safe to discount) from an unknown that must block.

// residueUnverified are the requirements whose UNVERIFIED verdict the gate
// tolerates: R1's bypass roster and R6's Actions-secrets inventory both
// concern actors OTHER than the bot and need repo admin to read, so a
// bot-credential run cannot see them and never could.  Every other
// requirement is fully bot-readable, so an UNVERIFIED there is a real
// failure to establish the fact, and blocks.
var residueUnverified = map[string]bool{"R1": true, "R6": true}

// GateResult is the provision gate's decision.  Blocking is empty iff OK;
// otherwise it holds the verdicts that forbid the grange, in requirement
// order, for the refusal message and the lifecycle audit record.
type GateResult struct {
	OK       bool
	Blocking []Verdict
}

// Gate applies the provision policy to a bot-credential Check — the last
// step of the ProvisionSnapshot -> Check -> Gate pipeline the archivist runs
// before handing over a grange.  It refuses on any VIOLATION, and tolerates
// UNVERIFIED only on the admin-only residue (R1, R6) — and R1's residue only
// when R5 proved the bot is not an admin, since the bypass roster is
// discountable precisely because admin is the sole bypass and the bot
// verifiably lacks it.
func Gate(verdicts []Verdict) GateResult {
	r5OK := false
	for _, v := range verdicts {
		if v.Req == "R5" && v.Status == OK {
			r5OK = true
		}
	}
	var blocking []Verdict
	for _, v := range verdicts {
		switch v.Status {
		case Violation:
			blocking = append(blocking, v)
		case Unverified:
			switch {
			case !residueUnverified[v.Req]:
				blocking = append(blocking, v)
			case v.Req == "R1" && !r5OK:
				// The bypass roster is unreadable AND the bot is not provably
				// a non-admin: the "bot cannot bypass" argument collapses.
				blocking = append(blocking, v)
			}
		}
	}
	return GateResult{OK: len(blocking) == 0, Blocking: blocking}
}
