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

package egress

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/egress/policy"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
)

// --- shared test scaffolding ------------------------------------------------

var fixedNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

func testPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	on := true
	p := &policy.Policy{}
	p.Search.Engine = policy.EngineKagi
	p.Search.DailyCap = 5
	p.Search.DenySearchEnginePages = &on
	p.Extract.DailyCap = 3
	p.Extract.Deny = []policy.DenyEntry{{Host: "pastebin.com"}, {Host: "*.ngrok.io"}}
	p.Limits.MaxResponseBytes = 1 << 20
	p.Limits.Timeout = policy.Duration(10 * time.Second)
	return p
}

type stubSearcher struct {
	name  string
	hits  []Hit
	err   error
	calls int
}

func (s *stubSearcher) Name() string { return s.name }
func (s *stubSearcher) Search(_ context.Context, _ string, _ int) ([]Hit, error) {
	s.calls++
	return s.hits, s.err
}

type stubRetriever struct {
	name    string
	md      string
	err     error
	calls   int
	lastURL string
}

func (r *stubRetriever) Name() string { return r.name }
func (r *stubRetriever) Fetch(_ context.Context, u string) (Extracted, error) {
	r.calls++
	r.lastURL = u
	if r.err != nil {
		return Extracted{}, r.err
	}
	return Extracted{Markdown: r.md, FinalURL: u}, nil
}

func newSub(t *testing.T, p *policy.Policy, s Searcher, r Retriever, scrub *wire.Scrubber) *Subsystem {
	t.Helper()
	dir := t.TempDir()
	sl, err := OpenLedger(filepath.Join(dir, "search"), 48*time.Hour, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	el, err := OpenLedger(filepath.Join(dir, "extract"), 48*time.Hour, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := NewSubsystem(Config{
		Policy: p, Searcher: s, Retriever: r, SearchLedger: sl, ExtractLedger: el,
		Scrubber: scrub, Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// --- Session behavior -------------------------------------------------------

func TestSearchMintsHandlesAndExtractResolvesExactURL(t *testing.T) {
	const want = "https://docs.gradle.org/Current/PathCase?Q=AbC" // mixed-case path/query
	s := &stubSearcher{name: "kagi", hits: []Hit{{Title: "T", URL: want, Snippet: "S"}}}
	r := &stubRetriever{name: "kagi", md: "# hi"}
	se := newSub(t, testPolicy(t), s, r, nil).NewSession()

	res, err := se.Search(context.Background(), "gradle", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Handle.IsZero() {
		t.Fatalf("want 1 result with a handle, got %+v", res)
	}
	ext, err := se.Extract(context.Background(), res[0].Handle.String())
	if err != nil {
		t.Fatalf("extract handle: %v", err)
	}
	if ext.Markdown != "# hi" {
		t.Errorf("markdown = %q", ext.Markdown)
	}
	if r.lastURL != want {
		t.Errorf("retriever got %q, want exact minted URL %q (path/query case must survive)", r.lastURL, want)
	}
}

func TestExtractRawURLNeedsApprovalAndMakesNoUpstreamCall(t *testing.T) {
	r := &stubRetriever{name: "kagi"}
	se := newSub(t, testPolicy(t), &stubSearcher{name: "kagi"}, r, nil).NewSession()

	_, err := se.Extract(context.Background(), "https://example.com/page?q=x")
	if !errors.Is(err, ErrNeedsApproval) {
		t.Fatalf("raw URL err = %v, want ErrNeedsApproval", err)
	}
	if r.calls != 0 {
		t.Errorf("raw URL made %d upstream calls, want 0 (approval gates it)", r.calls)
	}
}

func TestExtractApprovedURLEnforcesGates(t *testing.T) {
	r := &stubRetriever{name: "kagi", md: "content"}
	se := newSub(t, testPolicy(t), &stubSearcher{name: "kagi"}, r, nil).NewSession()

	// Approval bypasses nothing: a denied host is still refused, no upstream call.
	if _, err := se.ExtractApprovedURL(context.Background(), "https://pastebin.com/raw/x"); !errors.Is(err, ErrDenied) {
		t.Errorf("approved denied host err = %v, want ErrDenied", err)
	}
	if r.calls != 0 {
		t.Errorf("approved denied host made %d upstream calls, want 0", r.calls)
	}
	// An allowed host extracts.
	ext, err := se.ExtractApprovedURL(context.Background(), "https://ok.example/page")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Markdown != "content" || r.calls != 1 {
		t.Errorf("approved extract: markdown=%q calls=%d, want content/1", ext.Markdown, r.calls)
	}
}

func TestExtractRejectsMutatedOrForeignHandle(t *testing.T) {
	r := &stubRetriever{name: "kagi"}
	se := newSub(t, testPolicy(t), &stubSearcher{name: "kagi", hits: []Hit{{URL: "https://ok.example/x"}}}, r, nil).NewSession()
	res, _ := se.Search(context.Background(), "q", 1)

	mutated := res[0].Handle.String() + "0" // a token we never minted
	if _, err := se.Extract(context.Background(), mutated); !errors.Is(err, ErrUnknownHandle) {
		t.Errorf("mutated handle err = %v, want ErrUnknownHandle", err)
	}
	if _, err := se.Extract(context.Background(), "h_00000000000000000000000000000000"); !errors.Is(err, ErrUnknownHandle) {
		t.Errorf("foreign handle err = %v, want ErrUnknownHandle", err)
	}
	if r.calls != 0 {
		t.Errorf("unknown handles made %d upstream calls, want 0", r.calls)
	}
}

func TestExtractDeniedAndInternalHostsRefusedNoUpstream(t *testing.T) {
	r := &stubRetriever{name: "kagi"}
	hits := []Hit{
		{URL: "https://pastebin.com/raw/abc"},        // deny-listed
		{URL: "https://x.ngrok.io/leak"},             // wildcard deny
		{URL: "https://10.0.0.5/internal"},           // RFC-1918 hygiene
		{URL: "https://search.brave.com/search?q=z"}, // built-in SERP deny (toggle on)
	}
	se := newSub(t, testPolicy(t), &stubSearcher{name: "kagi", hits: hits}, r, nil).NewSession()
	res, _ := se.Search(context.Background(), "q", 4)

	wants := []error{ErrDenied, ErrDenied, ErrInternalHost, ErrDenied}
	for i, res := range res {
		_, err := se.Extract(context.Background(), res.Handle.String())
		if !errors.Is(err, wants[i]) {
			t.Errorf("hit %d (%s) err = %v, want %v", i, hits[i].URL, err, wants[i])
		}
	}
	if r.calls != 0 {
		t.Errorf("refused extracts made %d upstream calls, want 0", r.calls)
	}
}

func TestDailyCapsTrip(t *testing.T) {
	p := testPolicy(t) // search cap 5, extract cap 3
	s := &stubSearcher{name: "kagi", hits: []Hit{{URL: "https://ok.example/x"}}}
	r := &stubRetriever{name: "kagi", md: "x"}
	se := newSub(t, p, s, r, nil).NewSession()
	ctx := context.Background()

	var handle string
	for i := 0; i < p.Search.DailyCap; i++ {
		res, err := se.Search(ctx, "q", 1)
		if err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
		handle = res[0].Handle.String()
	}
	if _, err := se.Search(ctx, "q", 1); !errors.Is(err, ErrSearchCap) {
		t.Errorf("search past cap err = %v, want ErrSearchCap", err)
	}
	for i := 0; i < p.Extract.DailyCap; i++ {
		if _, err := se.Extract(ctx, handle); err != nil {
			t.Fatalf("extract %d: %v", i, err)
		}
	}
	if _, err := se.Extract(ctx, handle); !errors.Is(err, ErrExtractCap) {
		t.Errorf("extract past cap err = %v, want ErrExtractCap", err)
	}
}

func TestScrubberRedactsProviderError(t *testing.T) {
	const key = "SECRET-kagi-token-123"
	s := &stubSearcher{name: "kagi", err: errors.New(`upstream kagi.com: 401: {"auth":"Bot ` + key + `"}`)}
	se := newSub(t, testPolicy(t), s, &stubRetriever{name: "kagi"}, wire.NewScrubber(key)).NewSession()

	_, err := se.Search(context.Background(), "q", 1)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error leaked the key: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("error not scrubbed: %q", err.Error())
	}
}

// --- structural: no bare HTTP outside the one guarded constructor -----------

// TestNoBareHTTPOutsideTransport walks the whole egress tree — the core and
// every leaf (wire, policy, search, extract) — and refuses bare HTTP anywhere
// except wire/transport.go, the ONE sanctioned home for the guarded client.
func TestNoBareHTTPOutsideTransport(t *testing.T) {
	banned := []string{"http.DefaultClient", "http.Get(", "http.Post(", "http.Transport{", "http.Client{"}
	sanctioned := filepath.Join("wire", "transport.go")
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") || path == sanctioned {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, b := range banned {
			if strings.Contains(string(src), b) {
				t.Errorf("%s contains %q — all outbound HTTP must go through wire.NewGuardedClient (wire/transport.go)", path, b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
