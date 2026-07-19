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
// (docs/agency.md).  Every consumer's OPENAI_BASE_URL dials the agency,
// which serves the OpenAI-compatible /v1 API by routing ENGINE CLASSES:
// the request's model field names a class, and fail-closed config maps
// each class to an ordered fallback chain of (node, model) links.
// Unavailable means the next link, never a silent substitute; an exhausted
// chain is a distinct refusal; every response says which engine served.
// (The phase-1 pass-through mode was removed once routing proved out —
// routing is the only mode.)
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
	"net/http"
	"time"
)

// Config carries the agency's bootstrap inputs.
type Config struct {
	// Routes is the engine-class routing table, loaded via
	// LoadRouterConfig or DefaultRouterConfig.  Required.
	Routes *RouterConfig
}

// Server serves the OpenAI-compatible API.
type Server struct {
	router        *router
	presence      *presenceTracker
	probeInterval time.Duration
}

// New validates the config and builds the server.  It fails closed: a
// missing routing table is a startup error, never a lazily discovered one.
func New(cfg Config) (*Server, error) {
	if cfg.Routes == nil {
		return nil, fmt.Errorf("agency: Routes is required — the door routes engine classes")
	}
	rt := newRouter(cfg.Routes, nil)
	return &Server{router: rt, presence: rt.presence, probeInterval: cfg.Routes.probeInterval}, nil
}

// ProbePresence sweeps node presence on the configured interval until ctx
// ends, starting with an immediate sweep.  Callers run it on its own
// goroutine beside the HTTP server.
func (s *Server) ProbePresence(ctx context.Context) {
	s.presence.run(ctx, s.probeInterval)
}

// Handler serves the routed /v1 API and a liveness probe at /healthz.
// Every other path is refused: the agency is a door, not a bypass.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/", s.router)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "agency: only the OpenAI-compatible /v1 API passes the inference door", http.StatusNotFound)
	})
	return mux
}
