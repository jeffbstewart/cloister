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

// Package search holds the egress subsystem's web-search providers: Kagi
// (default) and Brave (alternate), both trimmed to the same Hit shape behind
// the Searcher seam.  It builds on the wire leaf (guarded client, capped
// requests, scrubber) and the policy leaf (engine names, result ceiling)
// without importing the core egress package.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/jeffbstewart/cloister/internal/egress/policy"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
)

// htmlTagRE matches an HTML tag; Kagi snippets/titles arrive with <strong>
// highlight markup and HTML entities.
var htmlTagRE = regexp.MustCompile(`<[^>]*>`)

// cleanText strips highlight tags and decodes HTML entities so the model sees
// plain text (strip first, then unescape).
func cleanText(s string) string {
	return html.UnescapeString(htmlTagRE.ReplaceAllString(s, ""))
}

// Hit is one trimmed search result, before the Session mints its handle.
type Hit struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher is a web-search backend: Kagi (default) or Brave (alternate),
// both trimmed to the same Hit shape so the swap is invisible above the seam.
type Searcher interface {
	Search(ctx context.Context, query string, count int) ([]Hit, error)
	Name() string // "kagi" | "brave" — recorded in the audit
}

// clip shortens a string for a log line (queries/URLs can be long).
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// clampCount holds a requested result count within [1, policy.MaxResultCount].
func clampCount(count int) int {
	if count < 1 {
		return 1
	}
	if count > policy.MaxResultCount {
		return policy.MaxResultCount
	}
	return count
}

// --- Kagi -------------------------------------------------------------------

// kagiSearcher calls Kagi's Search API v1:
//
//	POST https://kagi.com/api/v1/search
//	Authorization: Bearer <token>
//	Content-Type: application/json
//	{"query":"…","extract":{"count":N},"filters":{"safe_search":false}}
//
// Response envelope: {"meta":{…},"data":{"search":[…],"video":[…]},"error":[…]}
// — data is an OBJECT keyed by result kind; web results are data.search.  On error
// data is null and error[] carries {code,msg}. Same Bearer token as the Extract
// API (v0/"Bot" was the wrong scheme).
type kagiSearcher struct {
	base string // e.g. https://kagi.com
	// key is the Kagi API token, sent as "Authorization: Bearer <key>" (the same
	// token the Extract API uses).  Obtain it from the Kagi API portal
	// (kagi.com/settings → API); pay-per-use, billed to that account.  The scholar
	// receives it ONLY via KAGI_API_KEY, never exposes it to the model, and
	// the scrubber redacts it from any surfaced provider error.
	key      string
	hc       *http.Client
	maxBytes int64
}

// NewKagiSearcher builds the Kagi search backend over the guarded client.
func NewKagiSearcher(base, key string, hc *http.Client, maxBytes int64) Searcher {
	return &kagiSearcher{base: base, key: key, hc: hc, maxBytes: maxBytes}
}

func (k *kagiSearcher) Name() string { return string(policy.EngineKagi) }

// kagiSearchReq is the v1 search body.  The result count rides under "extract"
// per Kagi's documented shape; safe_search is off for research completeness.
type kagiSearchReq struct {
	Query   string          `json:"query"`
	Extract kagiSearchCount `json:"extract"`
	Filters kagiSearchFilt  `json:"filters"`
}
type kagiSearchCount struct {
	Count int `json:"count"`
}
type kagiSearchFilt struct {
	SafeSearch bool `json:"safe_search"`
}

func (k *kagiSearcher) Search(ctx context.Context, query string, count int) ([]Hit, error) {
	start := time.Now()
	log.Printf("egress: kagi search %q (count %d)", clip(query, 150), clampCount(count))
	reqBody, err := json.Marshal(kagiSearchReq{
		Query:   query,
		Extract: kagiSearchCount{Count: clampCount(count)},
		Filters: kagiSearchFilt{SafeSearch: false},
	})
	if err != nil {
		return nil, err
	}
	hdr := http.Header{"Authorization": {"Bearer " + k.key}}
	body, err := wire.PostJSON(ctx, k.hc, k.base+"/api/v1/search", hdr, reqBody, k.maxBytes)
	if err != nil {
		log.Printf("egress: kagi search %q failed after %s: %s", clip(query, 150),
			time.Since(start).Round(time.Millisecond), wire.NewScrubber(k.key).Scrub(err.Error()))
		return nil, err
	}
	var out struct {
		Data struct {
			Search []struct {
				URL     string `json:"url"`
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"data"`
		Error []struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		log.Printf("egress: kagi search %q: decode failed: %v", clip(query, 150), err)
		return nil, fmt.Errorf("kagi: decode search response: %w", err)
	}
	if len(out.Error) > 0 {
		return nil, fmt.Errorf("kagi search: %s (code %d)", out.Error[0].Msg, out.Error[0].Code)
	}
	hits := make([]Hit, 0, len(out.Data.Search))
	for _, d := range out.Data.Search {
		if d.URL == "" {
			continue
		}
		hits = append(hits, Hit{Title: cleanText(d.Title), URL: d.URL, Snippet: cleanText(d.Snippet)})
		if len(hits) >= clampCount(count) {
			break
		}
	}
	log.Printf("egress: kagi search %q -> %d results in %s", clip(query, 150), len(hits),
		time.Since(start).Round(time.Millisecond))
	return hits, nil
}

// --- Brave ------------------------------------------------------------------

type braveSearcher struct {
	base string // e.g. https://api.search.brave.com
	// key is a Brave Search API subscription token.  Obtain one from the Brave
	// Search API dashboard — api-dashboard.search.brave.com (docs:
	// brave.com/search/api) — which needs a Brave account and a subscription
	// (the free tier ended Feb 2026: a small monthly credit, then metered).  It
	// is sent as the "X-Subscription-Token" header.  The scholar receives it
	// ONLY via the BRAVE_API_KEY env var, never exposes it to the model,
	// and the scrubber redacts it from any surfaced provider error.
	key      string
	hc       *http.Client
	maxBytes int64
}

// NewBraveSearcher builds the Brave search backend over the guarded client.
func NewBraveSearcher(base, key string, hc *http.Client, maxBytes int64) Searcher {
	return &braveSearcher{base: base, key: key, hc: hc, maxBytes: maxBytes}
}

func (b *braveSearcher) Name() string { return string(policy.EngineBrave) }

// Search implements Searcher via Brave's web-search API.
//
// CAUTION: the Brave path has never been exercised against the real API —
// only against stubs of its documented response shape — and likely contains
// errors.  Validate it end-to-end before relying on engine: brave.
func (b *braveSearcher) Search(ctx context.Context, query string, count int) ([]Hit, error) {
	start := time.Now()
	log.Printf("egress: brave search %q (count %d)", clip(query, 150), clampCount(count))
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(clampCount(count)))
	hdr := http.Header{
		"X-Subscription-Token": {b.key},
		"Accept":               {"application/json"},
	}
	body, err := wire.GetBytes(ctx, b.hc, b.base+"/res/v1/web/search?"+q.Encode(), hdr, b.maxBytes)
	if err != nil {
		log.Printf("egress: brave search %q failed after %s: %s", clip(query, 150),
			time.Since(start).Round(time.Millisecond), wire.NewScrubber(b.key).Scrub(err.Error()))
		return nil, err
	}
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		log.Printf("egress: brave search %q: decode failed: %v", clip(query, 150), err)
		return nil, fmt.Errorf("brave: decode search response: %w", err)
	}
	hits := make([]Hit, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		if r.URL == "" {
			continue
		}
		hits = append(hits, Hit{Title: r.Title, URL: r.URL, Snippet: r.Description})
		if len(hits) >= clampCount(count) {
			break
		}
	}
	log.Printf("egress: brave search %q -> %d results in %s", clip(query, 150), len(hits),
		time.Since(start).Round(time.Millisecond))
	return hits, nil
}
