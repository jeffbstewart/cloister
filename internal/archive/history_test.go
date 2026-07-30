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

package archive

import (
	"context"
	"strings"
	"testing"
)

func TestHistoryNewestFirstAndLimited(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/log")
	r.write("a.txt", "one\n")
	r.checkpoint("first")
	r.write("a.txt", "two\n")
	r.checkpoint("second")
	r.write("a.txt", "three\n")
	r.checkpoint("third")

	changes, err := r.a.History(context.Background(), HistoryQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("len = %d, want 2", len(changes))
	}
	if changes[0].Subject != "third" || changes[1].Subject != "second" {
		t.Errorf("subjects = %q, %q; want third, second (newest first)", changes[0].Subject, changes[1].Subject)
	}
	if changes[0].ID.IsZero() {
		t.Error("checkpoint id missing")
	}
}

func TestHistoryScopedToPath(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/scoped")
	r.write("a.txt", "one\n")
	r.checkpoint("touches a")
	r.write("b.txt", "one\n")
	r.checkpoint("touches b")

	changes, err := r.a.History(context.Background(), HistoryQuery{Path: "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Subject != "touches b" {
		t.Errorf("history(b.txt) = %+v, want exactly the checkpoint touching b", changes)
	}
}

func TestHistoryFromRef(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/reffed")
	r.write("a.txt", "one\n")
	first := r.checkpoint("first")
	r.write("a.txt", "two\n")
	r.checkpoint("second")

	changes, err := r.a.History(context.Background(), HistoryQuery{Ref: first.Ref()})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Subject != "first" {
		t.Errorf("history from the first checkpoint starts at %q, want first", changes[0].Subject)
	}
}

func TestShowChange(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/shown")
	r.write("a.txt", "line one\n")
	id := r.checkpoint("add a")

	c, err := r.a.ShowChange(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "add a" || c.Author != botIdent.Name {
		t.Errorf("metadata = %q by %q, want add a by the bot", c.Subject, c.Author)
	}
	if !strings.Contains(c.Diff, "+line one") {
		t.Errorf("diff missing the added line:\n%s", c.Diff)
	}
}

func TestFileAtOldRevision(t *testing.T) {
	r := newRig(t)
	r.startWork("agent/versions")
	r.write("a.txt", "version one\n")
	first := r.checkpoint("v1")
	r.write("a.txt", "version two\n")
	r.checkpoint("v2")

	got, err := r.a.FileAt(context.Background(), first.Ref(), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version one\n" {
		t.Errorf("FileAt(v1) = %q, want the v1 content with its trailing newline", got)
	}
}

func TestFileAtRefusesTraversal(t *testing.T) {
	r := newRig(t)
	head, err := ParseRef("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.FileAt(context.Background(), head, "../outside"); err == nil {
		t.Error("FileAt with a traversal path should refuse")
	}
	if _, err := r.a.FileAt(context.Background(), Ref{}, "a.txt"); err == nil {
		t.Error("FileAt with a zero ref should refuse")
	}
}
