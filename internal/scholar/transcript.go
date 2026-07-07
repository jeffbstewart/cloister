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

package scholar

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// promptVersion identifies the baked system prompt a transcript ran under.
// Bump it when prompt.go changes materially.  The series restarted at 1 with
// the source-control migration; SVN-era transcripts count from the old one.
const promptVersion = "scholar-prompt-1"

// transcript accumulates a URLs-ONLY record of one research call:
// the prompt version, the query, each tool CALL (its query/URL), result METADATA
// (which URLs, byte counts), the model's own messages, and the answer.  It never
// stores verbatim page bodies or result lists — those are the tool RESULT
// contents, which are deliberately left out.  It is size-capped; on overflow it
// sets truncated and drops further lines.
type transcript struct {
	b         strings.Builder
	max       int
	truncated bool
}

func newTranscript(max int, query string) *transcript {
	t := &transcript{max: max}
	t.line("prompt: %s", promptVersion)
	t.line("query: %s", oneLine(query))
	return t
}

// line appends a formatted line unless the cap is reached (then it flags
// truncated and stops).
func (t *transcript) line(format string, args ...any) {
	if t.truncated {
		return
	}
	s := fmt.Sprintf(format, args...)
	if t.max > 0 && t.b.Len()+len(s)+1 > t.max {
		t.truncated = true
		t.b.WriteString("…[transcript truncated]\n")
		return
	}
	t.b.WriteString(s)
	t.b.WriteByte('\n')
}

func (t *transcript) String() string { return t.b.String() }

// oneLine collapses whitespace so a multi-line query/answer/message stays one
// transcript line (also strips any stray newlines an attacker page injected into
// the model's echoed text).
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
