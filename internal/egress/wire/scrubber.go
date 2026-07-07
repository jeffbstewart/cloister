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

package wire

import "strings"

// Scrubber replaces key material with "[redacted]" in any text the subsystem
// surfaces or audits.  The one real leak vector is a provider error path
// — a Kagi/Brave 4xx/5xx body echoing our auth header — so every provider-call
// error is scrubbed at the Session boundary before it leaves the package.
type Scrubber struct{ secrets []string }

// NewScrubber builds a scrubber over the given secrets; empty strings are
// ignored (a cell with no Brave key must not redact every "").
func NewScrubber(secrets ...string) *Scrubber {
	var s []string
	for _, k := range secrets {
		if k != "" {
			s = append(s, k)
		}
	}
	return &Scrubber{secrets: s}
}

// Scrub redacts every configured secret.  A nil scrubber is a no-op so callers
// need not special-case it.
func (s *Scrubber) Scrub(text string) string {
	if s == nil {
		return text
	}
	for _, k := range s.secrets {
		text = strings.ReplaceAll(text, k, "[redacted]")
	}
	return text
}
