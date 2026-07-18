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

// Package agency is the sole inference door of the shared infra stack
// (docs/agency.md).  This is phase 1 — a pass-through: every consumer's
// OPENAI_BASE_URL dials the agency, and the agency forwards the
// OpenAI-compatible /v1 API to the one model server behind it, which no
// longer shares a network with any consumer.  Behaviorally invisible,
// topology proven; engine classes, queueing, and fallback chains arrive in
// later phases.
//
// Containment: the agency sees every prompt, so it holds no capability
// beyond forwarding — no workspace, no state access, no tools, and it
// parses nothing beyond the request line it routes on.  Only /v1/ paths
// pass: the raw model-server API (model pulls, deletes, ollama's native
// endpoints) is not reachable through the door.  Streaming responses are
// flushed token-by-token, never buffered whole.  Stdlib only.
package agency

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Config carries the agency's bootstrap inputs.
type Config struct {
	// UpstreamURL is the base URL of the model server the door fronts,
	// e.g. http://infer:11434.  Required.
	UpstreamURL string
}

// Server forwards the OpenAI-compatible API to the configured upstream.
type Server struct {
	// servedBy names the engine that answers, reported on every response
	// so a reply is never silently attributable to nothing.  In phase 1
	// there is exactly one engine: the upstream's hostname.
	servedBy string
	proxy    *httputil.ReverseProxy
}

// New validates the config and builds the server.  It fails closed: a
// missing or malformed upstream URL is a startup error, never a lazily
// discovered one.
func New(cfg Config) (*Server, error) {
	if cfg.UpstreamURL == "" {
		return nil, fmt.Errorf("agency: UpstreamURL is required")
	}
	upstream, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("agency: parse upstream URL: %w", err)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("agency: upstream URL %q: scheme must be http or https", cfg.UpstreamURL)
	}
	if upstream.Host == "" {
		return nil, fmt.Errorf("agency: upstream URL %q has no host", cfg.UpstreamURL)
	}

	s := &Server{servedBy: upstream.Hostname()}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			// Present the upstream's own name as the Host header — exactly
			// what consumers sent when they dialed the model server
			// directly, so the flip stays behaviorally invisible.
			r.Out.Host = upstream.Host
		},
		// Negative: flush every write through immediately.  A streaming
		// completion must reach the consumer token-by-token; buffering a
		// whole response would turn decode time into dead air.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("Agency-Served-By", s.servedBy)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("agency: forward %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "agency: model server unreachable", http.StatusBadGateway)
		},
	}
	return s, nil
}

// Handler serves the forwarded /v1 API and a liveness probe at /healthz.
// Every other path is refused: the agency is a door, not a bypass.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/", s.proxy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "agency: only the OpenAI-compatible /v1 API passes the inference door", http.StatusNotFound)
	})
	return mux
}
