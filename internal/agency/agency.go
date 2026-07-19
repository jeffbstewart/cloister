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
// (docs/agency.md).  Every consumer's OPENAI_BASE_URL dials the agency; the
// door serves the OpenAI-compatible /v1 API in one of two modes:
//
//   - Pass-through (phase 1): forward everything to the one model server
//     behind the door, which no longer shares a network with any consumer.
//     Behaviorally invisible, topology proven.
//   - Class routing (phase 2, router.go): the request's model field names an
//     engine CLASS, and fail-closed config maps each class to an ordered
//     fallback chain of (node, model) links.  Unavailable means the next
//     link, never a silent substitute; an exhausted chain is a distinct
//     refusal; every response says which engine served.
//
// Containment: the agency sees every prompt, so it holds no capability
// beyond forwarding — no workspace, no state access, no tools, and it
// parses requests minimally (top-level JSON fields, only to route).  Only
// /v1/ paths pass: the raw model-server API (model pulls, deletes, ollama's
// native endpoints) is not reachable through the door.  Streaming responses
// are flushed token-by-token, never buffered whole.
package agency

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Config carries the agency's bootstrap inputs.  Exactly one of UpstreamURL
// and Routes must be set — the door is either a pass-through or a router,
// chosen deliberately at startup.
type Config struct {
	// UpstreamURL is the base URL of the model server a PASS-THROUGH door
	// fronts, e.g. http://infer:11434.
	UpstreamURL string
	// Routes is the engine-class routing table of a ROUTING door, loaded
	// via LoadRouterConfig.
	Routes *RouterConfig
}

// Server serves the OpenAI-compatible API in the configured mode.
type Server struct {
	// v1 handles everything under /v1/ — the pass-through proxy or the
	// class router, fixed at construction.
	v1 http.Handler
	// router is set in routing mode only; it backs ProbePresence and
	// WriteStatusSnapshots, both no-ops for a pass-through door.
	router        *router
	presence      *presenceTracker
	probeInterval time.Duration
}

// New validates the config and builds the server.  It fails closed: no mode,
// both modes, or a malformed upstream URL is a startup error, never a lazily
// discovered one.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.UpstreamURL != "" && cfg.Routes != nil:
		return nil, fmt.Errorf("agency: config sets both UpstreamURL and Routes: choose pass-through or class routing")
	case cfg.Routes != nil:
		rt := newRouter(cfg.Routes, nil)
		return &Server{v1: rt, router: rt, presence: rt.presence, probeInterval: cfg.Routes.probeInterval}, nil
	case cfg.UpstreamURL == "":
		return nil, fmt.Errorf("agency: either UpstreamURL (pass-through) or Routes (class routing) is required")
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

	// servedBy names the engine that answers, reported on every response so
	// a reply is never silently attributable to nothing.  In pass-through
	// mode there is exactly one engine: the upstream's hostname.
	servedBy := upstream.Hostname()
	return &Server{v1: &httputil.ReverseProxy{
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
			resp.Header.Set(servedByHeader, servedBy)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("agency: forward %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "agency: model server unreachable", http.StatusBadGateway)
		},
	}}, nil
}

// ProbePresence sweeps node presence on the configured interval until ctx
// ends, starting with an immediate sweep.  It is a no-op for a pass-through
// door.  Callers run it on its own goroutine beside the HTTP server.
func (s *Server) ProbePresence(ctx context.Context) {
	if s.presence == nil {
		return
	}
	s.presence.run(ctx, s.probeInterval)
}

// Handler serves the forwarded /v1 API and a liveness probe at /healthz.
// Every other path is refused: the agency is a door, not a bypass.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/", s.v1)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "agency: only the OpenAI-compatible /v1 API passes the inference door", http.StatusNotFound)
	})
	return mux
}
