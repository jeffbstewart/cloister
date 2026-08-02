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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// at is the injected clock: every test time derives from it, never from
// the real one.
var at = time.Unix(1_753_000_000, 0).UTC()

func tempList(t *testing.T) *repoList {
	t.Helper()
	return &repoList{path: filepath.Join(t.TempDir(), "cloister", "repos")}
}

func TestTouchOrdersMostRecentFirst(t *testing.T) {
	l := tempList(t)
	l.touch("https://github.com/op/one", "agent/a", at)
	l.touch("https://github.com/op/two", "", at.Add(time.Minute))
	l.touch("https://github.com/op/three", "agent/c", at.Add(2*time.Minute))

	want := []string{"https://github.com/op/three", "https://github.com/op/two", "https://github.com/op/one"}
	for i, w := range want {
		if l.entries[i].Repo != w {
			t.Fatalf("entry %d = %s, want %s (order = %+v)", i, l.entries[i].Repo, w, l.entries)
		}
	}
}

// TestTouchIsOneEntryPerRepo: revisiting a repository moves it to the
// front and updates its branch rather than accumulating a second row —
// the list answers "where was I on this repo", not "every branch I ever
// had".
func TestTouchIsOneEntryPerRepo(t *testing.T) {
	l := tempList(t)
	l.touch("https://github.com/op/one", "agent/first", at)
	l.touch("https://github.com/op/two", "", at.Add(time.Minute))
	l.touch("https://github.com/op/one", "agent/second", at.Add(2*time.Minute))

	if len(l.entries) != 2 {
		t.Fatalf("entries = %+v, want two", l.entries)
	}
	if l.entries[0].Repo != "https://github.com/op/one" || l.entries[0].Branch != "agent/second" {
		t.Errorf("front entry = %+v, want op/one on agent/second", l.entries[0])
	}
}

func TestTrimCapsTheList(t *testing.T) {
	l := tempList(t)
	for i := 0; i < maxEntries+5; i++ {
		l.touch("https://github.com/op/r"+string(rune('a'+i)), "", at.Add(time.Duration(i)*time.Minute))
	}
	if len(l.entries) != maxEntries {
		t.Fatalf("entries = %d, want the cap of %d", len(l.entries), maxEntries)
	}
	// The cap drops the OLDEST, never the newest.
	if l.entries[0].Used != at.Add(time.Duration(maxEntries+4)*time.Minute).Unix() {
		t.Errorf("front entry = %+v, want the most recent", l.entries[0])
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	l := tempList(t)
	l.touch("https://github.com/op/one", "agent/keep", at)
	l.touch("https://github.com/op/two", "", at.Add(time.Minute))
	if err := l.save(); err != nil {
		t.Fatal(err)
	}

	got := loadRepos(l.path)
	if len(got.entries) != 2 {
		t.Fatalf("reloaded %+v, want two entries", got.entries)
	}
	if got.entries[0].Repo != "https://github.com/op/two" {
		t.Errorf("reloaded order = %+v, want most recent first", got.entries)
	}
	if got.entries[1].Branch != "agent/keep" {
		t.Errorf("branch lost in the round trip: %+v", got.entries[1])
	}
}

// TestLoadSortsRatherThanTrustingTheFile: the on-disk order is not
// authoritative (house rule for every epoch-second ledger).  A file
// written oldest-first must still present newest-first.
func TestLoadSortsRatherThanTrustingTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos")
	body := "1753000000\thttps://github.com/op/old\t\n" +
		"1753009999\thttps://github.com/op/new\tagent/x\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	l := loadRepos(path)
	if len(l.entries) != 2 || l.entries[0].Repo != "https://github.com/op/new" {
		t.Errorf("loaded = %+v, want the newer entry first", l.entries)
	}
}

// TestLoadSalvagesWhatItCanFromAGarbledFile: the list is a convenience
// index, so a damaged line costs that line and nothing more.  A
// workbench that refused to start over a corrupt cache file would cost
// the whole session to save one typed URL.
func TestLoadSalvagesWhatItCanFromAGarbledFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos")
	body := "not-a-number\thttps://github.com/op/bad\n" +
		"\n" +
		"missing-fields\n" +
		"1753000000\thttps://github.com/op/good\tagent/x\n" +
		"1753000001\t\n" // an empty repo is not an entry
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	l := loadRepos(path)
	if len(l.entries) != 1 || l.entries[0].Repo != "https://github.com/op/good" {
		t.Errorf("salvaged = %+v, want just the readable entry", l.entries)
	}
}

func TestLoadMissingFileIsEmptyNotAnError(t *testing.T) {
	l := loadRepos(filepath.Join(t.TempDir(), "never", "written"))
	if len(l.entries) != 0 {
		t.Errorf("missing file = %+v, want an empty list", l.entries)
	}
}

func TestSaveCreatesTheDirectory(t *testing.T) {
	l := tempList(t) // path is <tmp>/cloister/repos; cloister/ does not exist
	l.touch("https://github.com/op/one", "", at)
	if err := l.save(); err != nil {
		t.Fatalf("save into a missing directory: %v", err)
	}
	if _, err := os.Stat(l.path); err != nil {
		t.Errorf("nothing written: %v", err)
	}
}

func TestForget(t *testing.T) {
	l := tempList(t)
	l.touch("https://github.com/op/one", "", at)
	l.touch("https://github.com/op/two", "", at.Add(time.Minute))

	if _, ok := l.forget(0); ok {
		t.Error("forget(0) accepted; positions are 1-based")
	}
	if _, ok := l.forget(3); ok {
		t.Error("forget past the end accepted")
	}
	e, ok := l.forget(1)
	if !ok || e.Repo != "https://github.com/op/two" {
		t.Fatalf("forget(1) = %+v, %v; want the front entry", e, ok)
	}
	if len(l.entries) != 1 || l.entries[0].Repo != "https://github.com/op/one" {
		t.Errorf("after forget = %+v", l.entries)
	}
}

func TestShortRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/op/repo.git": "github.com/op/repo",
		"https://github.com/op/repo":     "github.com/op/repo",
		"git@github.com:op/repo.git":     "github.com:op/repo",
	} {
		if got := shortRepo(in); got != want {
			t.Errorf("shortRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgo(t *testing.T) {
	now := at.Add(72 * time.Hour)
	for _, tc := range []struct {
		used int64
		want string
	}{
		{now.Unix(), "just now"},
		{now.Add(-5 * time.Minute).Unix(), "5m ago"},
		{now.Add(-3 * time.Hour).Unix(), "3h ago"},
		{now.Add(-50 * time.Hour).Unix(), "2d ago"},
	} {
		if got := ago(tc.used, now); got != tc.want {
			t.Errorf("ago(%d) = %q, want %q", tc.used, got, tc.want)
		}
	}
}
