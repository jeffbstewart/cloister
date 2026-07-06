// Package extract holds the egress subsystem's page-retrieval provider:
// Kagi's Extract API, which fetches AND cleans a page to markdown on Kagi's
// servers, so this cell never dials the target itself.  It builds on the wire
// leaf (capped requests, scrubber) and the policy leaf (provider name)
// without importing the core egress package.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/egress/policy"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
)

// Extracted is a page fetched and cleaned to markdown by Kagi, server-side.
type Extracted struct {
	Markdown string
	FinalURL string // best-effort: Kagi follows redirects we can't observe
}

// Retriever turns a URL into clean markdown.  Kagi is the only
// implementation: its endpoint fetches AND cleans the page on Kagi's servers,
// so we never dial the target ourselves.  There is no Brave equivalent.
type Retriever interface {
	Fetch(ctx context.Context, targetURL string) (Extracted, error)
	Name() string // "kagi" — recorded as the provider in the audit
}

// clip shortens a string for a log line (URLs can be long).
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// extractFormat is Kagi's extract output format (the request "format" field).
type extractFormat string

const (
	formatMarkdown extractFormat = "markdown"
	formatJSON     extractFormat = "json"
)

// kagiRetriever calls Kagi's Extract API:
//
//	POST https://kagi.com/api/v1/extract
//	Authorization: Bearer <token>
//	Content-Type: application/json
//	{"pages":[{"url":"..."}],"format":"markdown","timeout":<sec>}
//
// With format:markdown the response body IS the extracted markdown (NOT a JSON
// envelope) — it opens with a "## Extraction Results / ### URL: …" header, then
// the page as markdown.  A failed fetch surfaces as a non-2xx, which
// wire.PostJSON turns into an error per its documented contract.  We send
// exactly one page.
type kagiRetriever struct {
	base string // e.g. https://kagi.com
	// key is the same Kagi API token the searcher uses, but the Extract API
	// authenticates with the "Authorization: Bearer <key>" scheme (the v0 Search
	// API uses "Bot" — different products, same token).  Obtain it from the Kagi
	// API portal (kagi.com/settings → API); pay-per-use, billed to that account.
	// Injected only via KAGI_API_KEY, never shown to the model,
	// scrubber-redacted in errors.
	key      string
	hc       *http.Client
	maxBytes int64
	timeout  time.Duration // Kagi's server-side fetch budget (wire "timeout" in seconds)
	path     string        // "/api/v1/extract"
}

// NewKagiRetriever builds the Kagi extract backend over the guarded client.
func NewKagiRetriever(base, key string, hc *http.Client, maxBytes int64, timeout time.Duration) Retriever {
	return &kagiRetriever{base: base, key: key, hc: hc, maxBytes: maxBytes, timeout: timeout, path: "/api/v1/extract"}
}

func (k *kagiRetriever) Name() string { return string(policy.EngineKagi) }

type extractPage struct {
	URL string `json:"url"`
}

type extractRequest struct {
	Pages   []extractPage `json:"pages"`
	Format  extractFormat `json:"format,omitempty"`
	Timeout float64       `json:"timeout,omitempty"` // seconds
}

func (k *kagiRetriever) Fetch(ctx context.Context, targetURL string) (Extracted, error) {
	start := time.Now()
	log.Printf("egress: kagi extract %s", clip(targetURL, 200))
	reqBody, err := json.Marshal(extractRequest{
		Pages:   []extractPage{{URL: targetURL}},
		Format:  formatMarkdown,
		Timeout: k.timeout.Seconds(),
	})
	if err != nil {
		return Extracted{}, err
	}
	hdr := http.Header{"Authorization": {"Bearer " + k.key}}
	body, err := wire.PostJSON(ctx, k.hc, k.base+k.path, hdr, reqBody, k.maxBytes)
	if err != nil {
		log.Printf("egress: kagi extract %s failed after %s: %s", clip(targetURL, 200),
			time.Since(start).Round(time.Millisecond), wire.NewScrubber(k.key).Scrub(err.Error()))
		return Extracted{}, err
	}
	// format:markdown returns the markdown DIRECTLY as the body (not JSON); a
	// non-2xx already errored above.  Kagi cannot report the redirect chain in this
	// format, so FinalURL is the requested URL.
	md := string(body)
	if strings.TrimSpace(md) == "" {
		log.Printf("egress: kagi extract %s: empty response", clip(targetURL, 200))
		return Extracted{}, fmt.Errorf("kagi extract: no content returned for %s", targetURL)
	}
	log.Printf("egress: kagi extract %s -> %d bytes in %s", clip(targetURL, 200),
		len(md), time.Since(start).Round(time.Millisecond))
	return Extracted{Markdown: md, FinalURL: targetURL}, nil
}
