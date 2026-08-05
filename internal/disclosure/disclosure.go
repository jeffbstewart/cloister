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

// Package disclosure is the per-repository acknowledgment that a cell's
// source leaves the machine (docs/JAILED_CLAUDE.md, "The disclosure gate").
//
// Under jailed-claude the workspace's contents go to Anthropic.  That is
// not a defect and for most trees it is not even interesting — but it is a
// DECISION, and a decision made once and then inherited silently by every
// later cell is not a decision.  This gate exists to make the operator
// touch it once per repository.
//
// A BOOLEAN IS THE WRONG SHAPE, and understanding why is the whole design.
// `DISCLOSURE_ACK=1` survives a copy-paste of a working cell's environment
// into a new one — which is precisely the case worth catching.  Not a
// careless operator: a careful one, reusing a configuration that already
// worked.  A flag that is already set cannot ask a question.
//
// So both the variable's NAME and its VALUE derive from the repository:
//
//	CLOISTER_DISCLOSURE_JEFFBSTEWART_EXAMPLEREPO=
//	    "source from jeffbstewart/examplerepo is sent to anthropic"
//
// Copying a working stanza into a new cell therefore fails twice over: the
// required variable is absent entirely, and the inherited one names the
// wrong repository.  There is no value that satisfies the gate for two
// different trees, which is the property a boolean cannot have.
//
// This is the same idea as the git-passthrough escape hatch — a control
// whose safety comes from being deliberately awkward to satisfy, in a tree
// that otherwise optimizes for the operator's convenience.  Both are cheap
// because they fire once.
package disclosure

import (
	"fmt"
	"strings"
)

// Prefix begins every acknowledgment variable.
const Prefix = "CLOISTER_DISCLOSURE_"

// RequiredVar names the recipient a cell discloses to, and is what ARMS the
// gate.  Unset, there is no gate: an ordinary cell sends its source
// nowhere, and the acknowledgment would be a ritual with nothing behind it.
//
// It is set by docker/cell-claude.yaml — the same file that grants the
// agent its edge to the door — and compose-lint requires it there.  That
// pairing is what keeps the default from being fail-open in practice: you
// cannot merge the overlay that sends source to Anthropic without also
// merging the line that demands it be acknowledged.
const RequiredVar = "CLOISTER_DISCLOSURE_REQUIRED"

// Variable returns the acknowledgment variable's name for a repository
// given as "owner/name": the prefix, then the repository uppercased with
// every non-alphanumeric run collapsed to a single underscore.
func Variable(repo string) string {
	var b strings.Builder
	b.WriteString(Prefix)
	lastUnderscore := true // suppresses a leading underscore after the prefix
	for _, r := range strings.ToUpper(repo) {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastUnderscore = false
		case !lastUnderscore:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// Lookup reads an environment variable, reporting whether it was set —
// os.LookupEnv's shape, injected so the gate is testable without touching
// the process environment.
type Lookup func(name string) (string, bool)

// RefusedError reports a provision refused for want of an acknowledgment.
// It is a refusal, not an error: the operator's next move is to set a
// variable, not to debug anything.
type RefusedError struct {
	Repo      string // "owner/name"
	Recipient string // where the source would go, e.g. "anthropic"
	Var       string // the variable that would satisfy the gate
	Why       string // absent, or naming the wrong repository
}

func (e *RefusedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "provision refused: this cell sends workspace source to %s, and %s has not been acknowledged (%s).\n",
		e.Recipient, e.Repo, e.Why)
	fmt.Fprintf(&b, "\nSet this variable on the archivist and redeploy the cell:\n\n    %s\n", e.Var)
	// The expected VALUE is described, never printed.  Printing it would
	// make the acknowledgment satisfiable by copying it out of the error
	// message, which is most of the deliberation gone — and deliberation is
	// the only thing this gate produces.  Naming the VARIABLE is necessary
	// or the gate is merely obstructive; naming the value is not.
	fmt.Fprintf(&b, "\nIts value must be a sentence of your own stating that source from\n%s is sent to %s.  Write it out; do not paste it from here.\n",
		e.Repo, e.Recipient)
	fmt.Fprintf(&b, "\nThe variable is named after the repository on purpose: a stanza copied\nfrom a cell where this already worked will not satisfy it, because it\nnames a different tree.  See docs/JAILED_CLAUDE.md, \"The disclosure gate\".\n")
	return b.String()
}

// Requirement names this refusal for the audit record.
func (e *RefusedError) Requirement() string { return "disclosure-acknowledged" }

// Check verifies the acknowledgment for repo ("owner/name").
//
// It returns nil when the gate is not armed — RequiredVar unset — which is
// every cell that does not send its source anywhere.  Arming it is the
// deliberate act; see RequiredVar.
func Check(repo string, look Lookup) error {
	recipient, armed := look(RequiredVar)
	recipient = strings.TrimSpace(recipient)
	if !armed || recipient == "" {
		return nil
	}
	name := Variable(repo)
	value, set := look(name)
	if !set || strings.TrimSpace(value) == "" {
		return &RefusedError{Repo: repo, Recipient: recipient, Var: name, Why: "the variable is not set"}
	}
	// Two facts, checked; the wording is the operator's.  Requiring an
	// exact sentence would make the gate a guessing game, since the
	// expected text is deliberately not printed — and the point is not the
	// prose, it is that the operator had to name THIS repository and THIS
	// recipient rather than inherit someone else's answer.
	got := strings.ToLower(value)
	if !strings.Contains(got, strings.ToLower(repo)) {
		return &RefusedError{Repo: repo, Recipient: recipient, Var: name,
			Why: "its value does not name " + repo + " — most likely it was copied from another cell"}
	}
	if !strings.Contains(got, strings.ToLower(recipient)) {
		return &RefusedError{Repo: repo, Recipient: recipient, Var: name,
			Why: "its value does not name " + recipient + ", where the source would go"}
	}
	return nil
}
