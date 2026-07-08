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

package shield

import "testing"

func TestClearReadablePathMints(t *testing.T) {
	s := load(t, map[string]string{
		".aiignore": "secrets/\n",
		"src/a.go":  "code",
	})
	ar, ok := s.Clear("src/a.go", []byte("package main\n"))
	if !ok {
		t.Fatal("Clear on a readable path returned ok=false")
	}
	if ar.IsZero() {
		t.Error("cleared value reports IsZero")
	}
	if ar.Path() != "src/a.go" {
		t.Errorf("Path() = %q, want src/a.go", ar.Path())
	}
	if ar.String() != "package main\n" {
		t.Errorf("String() = %q, want the cleared content", ar.String())
	}
	if string(ar.Bytes()) != "package main\n" {
		t.Errorf("Bytes() = %q, want the cleared content", ar.Bytes())
	}
}

func TestClearStrippedPathDenies(t *testing.T) {
	s := load(t, map[string]string{
		".aiignore": "secrets/\n",
	})
	ar, ok := s.Clear("secrets/prod.env", []byte("hunter2"))
	if ok {
		t.Fatal("Clear on a stripped path returned ok=true")
	}
	if !ar.IsZero() {
		t.Errorf("denied Clear returned a non-zero value: %+v", ar)
	}
}

func TestZeroAIReadableIsZero(t *testing.T) {
	var ar AIReadable
	if !ar.IsZero() {
		t.Error("zero AIReadable does not report IsZero")
	}
}
