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

package policy

import "testing"

func TestValidateDenyHost(t *testing.T) {
	ok := []string{"pastebin.com", "*.ngrok.io", "www.google.co.uk", "a.b.c.example.com"}
	for _, h := range ok {
		if err := validateDenyHost(h); err != nil {
			t.Errorf("validateDenyHost(%q) rejected a valid host: %v", h, err)
		}
	}
	bad := []string{"", "ev*l.com", "*.*.com", "*", "foo.*", "http://x.com", "x.com/path", "has space.com"}
	for _, h := range bad {
		if err := validateDenyHost(h); err == nil {
			t.Errorf("validateDenyHost(%q) accepted an invalid host", h)
		}
	}
}

func TestDeniesOperatorList(t *testing.T) {
	p := &Policy{}
	off := false
	p.Search.DenySearchEnginePages = &off
	p.Extract.Deny = []DenyEntry{
		{Host: "pastebin.com"},
		{Host: "*.ngrok.io"},
		{Host: "example.com", PathPrefix: "/upload"},
	}
	cases := []struct {
		host, path string
		want       bool
	}{
		{"pastebin.com", "/raw", true},
		{"PASTEBIN.COM", "/raw", true},         // host case-insensitive
		{"x.ngrok.io", "/y", true},             // wildcard subdomain
		{"ngrok.io", "/y", false},              // wildcard does NOT match apex
		{"example.com", "/upload/x", true},     // path prefix hit
		{"example.com", "/Upload/x", false},    // path is case-SENSITIVE
		{"example.com", "/read", false},        // path prefix miss
		{"docs.gradle.org", "/current", false}, // not listed
	}
	for _, c := range cases {
		if got := p.Denies(c.host, c.path); got != c.want {
			t.Errorf("Denies(%q,%q) = %v, want %v", c.host, c.path, got, c.want)
		}
	}
}

func TestDeniesBuiltinToggle(t *testing.T) {
	p := &Policy{}
	on := true
	p.Search.DenySearchEnginePages = &on
	if !p.Denies("www.google.com", "/search") {
		t.Error("google /search should be denied with the toggle on")
	}
	if p.Denies("www.google.com", "/maps") {
		t.Error("google /maps should be allowed (only /search is a SERP)")
	}
	if !p.Denies("duckduckgo.com", "/") {
		t.Error("duckduckgo whole-host should be denied")
	}
	// Toggle off → built-ins inactive.
	off := false
	p.Search.DenySearchEnginePages = &off
	if p.Denies("www.google.com", "/search") {
		t.Error("toggle off: google /search must not be denied by built-ins")
	}
}

func TestIsInternalHost(t *testing.T) {
	internal := []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "169.254.1.1", "::1", "localhost", "foo.local", "wat.localhost"}
	for _, h := range internal {
		if !IsInternalHost(h) {
			t.Errorf("IsInternalHost(%q) = false, want true", h)
		}
	}
	public := []string{"kagi.com", "docs.gradle.org", "8.8.8.8", "1.1.1.1"}
	for _, h := range public {
		if IsInternalHost(h) {
			t.Errorf("IsInternalHost(%q) = true, want false", h)
		}
	}
}
