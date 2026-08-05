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

package disclosure

import (
	"errors"
	"strings"
	"testing"
)

// env builds a Lookup from pairs, so each case states exactly the
// environment it describes and nothing else.
func env(kv ...string) Lookup {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestVariableIsDerivedFromTheRepository(t *testing.T) {
	for _, tc := range []struct{ repo, want string }{
		{"jeffbstewart/examplerepo", "CLOISTER_DISCLOSURE_JEFFBSTEWART_EXAMPLEREPO"},
		{"jeffbstewart/cloister", "CLOISTER_DISCLOSURE_JEFFBSTEWART_CLOISTER"},
		// Runs of non-alphanumerics collapse, so a repo with dots, dashes
		// and underscores still yields a legal shell identifier.
		{"acme/my-cool.repo_v2", "CLOISTER_DISCLOSURE_ACME_MY_COOL_REPO_V2"},
		{"Owner/Name", "CLOISTER_DISCLOSURE_OWNER_NAME"},
		// No trailing or doubled underscores, whatever the input.
		{"acme/repo--", "CLOISTER_DISCLOSURE_ACME_REPO"},
		{"acme//repo", "CLOISTER_DISCLOSURE_ACME_REPO"},
	} {
		if got := Variable(tc.repo); got != tc.want {
			t.Errorf("Variable(%q) = %q, want %q", tc.repo, got, tc.want)
		}
	}
}

// TestVariablesDifferPerRepository is the property a boolean cannot have,
// and the entire reason this gate is shaped the way it is.
func TestVariablesDifferPerRepository(t *testing.T) {
	if Variable("acme/one") == Variable("acme/two") {
		t.Fatal("two repositories share an acknowledgment variable — the gate would be satisfiable once for every tree")
	}
}

// TestUnarmedCellHasNoGate: an ordinary cell sends its source nowhere, so
// demanding an acknowledgment would be a ritual with nothing behind it.
func TestUnarmedCellHasNoGate(t *testing.T) {
	for _, look := range []Lookup{
		env(),                      // RequiredVar absent entirely
		env(RequiredVar, ""),       // ...or present and empty
		env(RequiredVar, "   "),    // ...or whitespace
		env("SOMETHING_ELSE", "x"), // ...or something unrelated set
	} {
		if err := Check("acme/repo", look); err != nil {
			t.Errorf("an unarmed cell was gated: %v", err)
		}
	}
}

func TestArmedCellRequiresTheAcknowledgment(t *testing.T) {
	const repo = "jeffbstewart/cloister"
	name := Variable(repo)

	cases := []struct {
		name    string
		look    Lookup
		wantErr bool
		wantWhy string
	}{
		{
			name:    "absent",
			look:    env(RequiredVar, "anthropic"),
			wantErr: true,
			wantWhy: "not set",
		},
		{
			name:    "empty",
			look:    env(RequiredVar, "anthropic", name, "  "),
			wantErr: true,
			wantWhy: "not set",
		},
		{
			// THE case the design exists for: a working stanza copied from
			// another cell.  The variable name would not even match, but an
			// operator who renamed it and kept the sentence must still fail.
			name:    "inherited from another repository",
			look:    env(RequiredVar, "anthropic", name, "source from someoneelse/otherrepo is sent to anthropic"),
			wantErr: true,
			wantWhy: "does not name " + repo,
		},
		{
			name:    "acknowledges the repo but not where it goes",
			look:    env(RequiredVar, "anthropic", name, "jeffbstewart/cloister is fine to share"),
			wantErr: true,
			wantWhy: "does not name anthropic",
		},
		{
			name: "a sentence of the operator's own",
			look: env(RequiredVar, "anthropic", name,
				"source from jeffbstewart/cloister is sent to anthropic"),
		},
		{
			// The wording is the operator's; only the two facts are checked.
			// Requiring exact prose would be a guessing game, because the
			// expected text is deliberately never printed.
			name: "different wording, same two facts",
			look: env(RequiredVar, "anthropic", name,
				"I accept that code in jeffbstewart/cloister leaves this machine for Anthropic's API."),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(repo, tc.look)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Check err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			var ref *RefusedError
			if !errors.As(err, &ref) {
				t.Fatalf("Check returned %T, want *RefusedError — the operator's next move is to set a variable, not to debug", err)
			}
			if !strings.Contains(ref.Why, tc.wantWhy) {
				t.Errorf("Why = %q, want it to mention %q", ref.Why, tc.wantWhy)
			}
		})
	}
}

// TestRefusalNamesTheVariableButNotAPasteableAssignment is the
// deliberation property, stated as narrowly as it is actually true.
//
// The design's own recommended wording — "the value must state that source
// from <owner>/<name> is sent to Anthropic" — necessarily contains the
// facts the value must contain, so the message can always be paraphrased
// into a working value.  Pretending otherwise would be theatre.  What must
// NOT appear is a ready-made `VAR=value` assignment: that is the difference
// between an operator who read the sentence and one who selected a line
// with the mouse.
//
// An earlier version of this test checked for the canonical phrase as one
// contiguous run, and passed only because the message happens to wrap
// across a line break.  A test that passes on whitespace is not testing
// anything, so this one normalizes first.
func TestRefusalNamesTheVariableButNotAPasteableAssignment(t *testing.T) {
	const repo = "acme/widget"
	err := Check(repo, env(RequiredVar, "anthropic"))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	flat := strings.Join(strings.Fields(strings.ToLower(msg)), " ")

	if !strings.Contains(msg, Variable(repo)) {
		t.Errorf("the refusal does not name the variable to set, which makes it merely obstructive:\n%s", msg)
	}
	// The thing that would actually collapse the deliberation: the whole
	// assignment, ready to paste.
	if strings.Contains(flat, strings.ToLower(Variable(repo))+"=") {
		t.Errorf("the refusal prints a pasteable %s=… assignment:\n%s", Variable(repo), msg)
	}
	// It must still be actionable: name both facts the value has to carry,
	// and say that composing it is the operator's job.
	for _, want := range []string{repo, "anthropic", "sentence"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the refusal never mentions %q, so the operator cannot act on it:\n%s", want, msg)
		}
	}
	// And it must say, in so many words, not to copy it — since the facts
	// are necessarily present, the instruction is what carries the intent.
	if !strings.Contains(flat, "do not paste") {
		t.Errorf("the refusal does not tell the operator to write the value themselves:\n%s", msg)
	}
}

// TestRefusalIsAuditable: the lifecycle record names what the repository
// lacked, the same way a forge-gate refusal does.
func TestRefusalIsAuditable(t *testing.T) {
	err := Check("acme/widget", env(RequiredVar, "anthropic"))
	var ref *RefusedError
	if !errors.As(err, &ref) {
		t.Fatal("expected a *RefusedError")
	}
	if ref.Requirement() == "" {
		t.Error("the refusal carries no requirement name, so the audit record cannot say why provision was refused")
	}
}
