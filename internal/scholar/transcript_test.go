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
	"context"
	"strings"
	"testing"

	"github.com/jeffbstewart/cloister/internal/egress"
)

// TestTranscriptIsURLsOnly is the URLs-only guarantee: the transcript records the
// query, the consulted URLs, and the answer — but never a fetched page body or a
// result snippet.
func TestTranscriptIsURLsOnly(t *testing.T) {
	s := &stubSearcher{hits: []egress.Hit{{Title: "T", URL: "https://a.example/1", Snippet: "SECRET-SNIPPET"}}}
	r := &stubRetriever{md: "# Page\nSECRET-PAGE-BODY"}
	srv := New(Config{Egress: testEgress(t, s, r), Model: &flowModel{}, Audit: &recAuditor{}, Caps: DefaultCaps()})

	res, err := srv.research(context.Background(), mustRunID(t), "how do gradle toolchains work?")
	if err != nil {
		t.Fatal(err)
	}
	tr := res.transcript
	for _, want := range []string{"query: how do gradle toolchains work?", "https://a.example/1", "answer: Gradle resolves JDKs"} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcript missing %q:\n%s", want, tr)
		}
	}
	for _, banned := range []string{"SECRET-PAGE-BODY", "SECRET-SNIPPET"} {
		if strings.Contains(tr, banned) {
			t.Errorf("transcript leaked content %q:\n%s", banned, tr)
		}
	}
}

// TestTranscriptTruncates: a transcript past its byte budget is cut and flagged.
func TestTranscriptTruncates(t *testing.T) {
	caps := DefaultCaps()
	caps.MaxTranscriptBytes = 60 // smaller than the prompt+query preamble
	srv := New(Config{
		Egress: testEgress(t, &stubSearcher{hits: []egress.Hit{{URL: "https://a.example/1"}}}, &stubRetriever{md: "x"}),
		Model:  &flowModel{}, Audit: &recAuditor{}, Caps: caps,
	})
	res, err := srv.research(context.Background(), mustRunID(t), "a fairly long query that will blow the tiny transcript budget")
	if err != nil {
		t.Fatal(err)
	}
	if !res.truncated || !strings.Contains(res.transcript, "[transcript truncated]") {
		t.Errorf("tiny transcript budget should truncate + flag:\n%s", res.transcript)
	}
}
