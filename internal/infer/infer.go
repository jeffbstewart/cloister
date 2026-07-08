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

// Package infer is the engine-routed inference layer that sits on top of the
// worker-agnostic internal/openai client.  It maps a caller's INTENT — an
// Effort, never a model name — to a named, routable Engine, bounds each call
// with an effort-derived context deadline, strips the model's chain-of-thought
// so the answer costs the caller no extra context, and returns a
// provenance-carrying Result (which engine served, wall-clock elapsed, tokens
// the engine reported).  It is a library only: no MCP tools, no worker wiring.
//
// It imports only the Go standard library plus internal/openai (itself
// stdlib-only) so it can join the librarian's stdlib-only import graph.
//
// Fallback between engines is deliberately NOT here — one engine per effort.
// Presence-aware fallback chains are the agency's job (see docs/agency.md).
package infer

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/openai"
)

// Effort is the single caller knob for a comprehension op: INTENT, not a model
// name.  The agent never names a model; the mapping from effort to engine
// lives in this package's Config.
type Effort string

const (
	// Quick is a fast, shallow pass — a small model with thinking off.
	Quick Effort = "quick"
	// Thorough is an effortful, chain-of-thought pass — a reasoning model with
	// thinking on.  Its extra cost is paid engine-side; the caller pays the
	// same handful of answer tokens as Quick because the reasoning trace is
	// stripped before it returns.
	Thorough Effort = "thorough"
)

// Valid reports whether e is a known effort.  Callers fail closed on an
// unknown effort rather than guessing a default — an unrecognized intent is a
// programming error, not a thing to paper over.
func (e Effort) Valid() bool {
	switch e {
	case Quick, Thorough:
		return true
	default:
		return false
	}
}

// efforts is the closed set of efforts a Config must cover.  New validates
// against it, and it is the single place to extend the enum.
var efforts = []Effort{Quick, Thorough}

// Default effort-derived deadlines.  Quick is a small model answering from a
// few files; Thorough may run a reasoning model with a long chain-of-thought.
const (
	DefaultQuickTimeout    = 60 * time.Second
	DefaultThoroughTimeout = 5 * time.Minute
)

// Completer is the single-turn completion seam this package drives.  It is the
// subset of *openai.Client that infer needs, extracted as an interface so
// tests can supply a fake with no real HTTP.  *openai.Client satisfies it.
type Completer interface {
	Complete(ctx context.Context, messages []openai.Message, tools []openai.Tool) (openai.Message, int, error)
}

// Engine is one named, routable completion endpoint.  Name is provenance: it
// is reported verbatim in Result.ServedBy so the response always says which
// engine served (never a silent substitution).
type Engine struct {
	Name      string // e.g. "think-fast", "deep-think"
	Completer Completer
}

// Config maps each Effort to the Engine that serves it and to the deadline
// that bounds it.  Now is an injectable clock seam so elapsed time is
// deterministic under test; production leaves it nil and ApplyDefaults sets
// it to time.Now.
type Config struct {
	// Engines routes an effort to the engine that serves it.  Every effort
	// must be present; New fails closed otherwise.
	Engines map[Effort]Engine
	// Timeouts is the effort-derived deadline applied via context.WithTimeout.
	// ApplyDefaults fills any missing entry.
	Timeouts map[Effort]time.Duration
	// Now is the clock read to measure Result.Elapsed.  nil → time.Now.
	Now func() time.Time
}

// ApplyDefaults fills unset timeouts with their per-effort defaults and
// defaults the clock to time.Now.  It does not invent engines: a missing
// engine is a construction error, surfaced by New, not silently defaulted.
func (c *Config) ApplyDefaults() {
	if c.Timeouts == nil {
		c.Timeouts = make(map[Effort]time.Duration)
	}
	if _, ok := c.Timeouts[Quick]; !ok {
		c.Timeouts[Quick] = DefaultQuickTimeout
	}
	if _, ok := c.Timeouts[Thorough]; !ok {
		c.Timeouts[Thorough] = DefaultThoroughTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Client routes comprehension calls to per-effort engines.
type Client struct {
	cfg Config
}

// New validates the config and builds the client.  It fails closed: every
// effort must map to an engine that has both a Name and a Completer, else New
// returns an error — no panics, and no request ever routes to a nil engine.
func New(cfg Config) (*Client, error) {
	cfg.ApplyDefaults()
	for _, effort := range efforts {
		engine, ok := cfg.Engines[effort]
		if !ok {
			return nil, fmt.Errorf("infer: no engine configured for effort %q", effort)
		}
		if engine.Name == "" {
			return nil, fmt.Errorf("infer: engine for effort %q has no name", effort)
		}
		if engine.Completer == nil {
			return nil, fmt.Errorf("infer: engine %q for effort %q has no completer", engine.Name, effort)
		}
	}
	return &Client{cfg: cfg}, nil
}

// Result is the provenance-carrying return of Ask.  Answer is the final answer
// with the reasoning trace stripped; the rest names who served, how long it
// took, and what the engine spent.
type Result struct {
	Answer   string        // final answer, reasoning trace stripped
	ServedBy string        // the engine Name that served
	Elapsed  time.Duration // wall-clock, measured via the injected clock
	Tokens   int           // total tokens the engine reported
}

// thinkTagPattern matches an inline <think>…</think> span: case-insensitive
// ((?i)) and dot-matches-newline ((?s)) so a multi-line reasoning trace is
// caught, non-greedy (.*?) so adjacent spans are stripped independently rather
// than swallowing the text between them.
var thinkTagPattern = regexp.MustCompile(`(?is)<think>.*?</think>`)

// Ask routes one comprehension query to the engine for effort and returns its
// stripped answer with provenance.  Comprehension is single-shot, so it passes
// no tools.  It fails closed: an unknown effort or a Complete error returns an
// error rather than a partial or fabricated Result.
func (c *Client) Ask(ctx context.Context, effort Effort, messages []openai.Message) (Result, error) {
	if !effort.Valid() {
		return Result{}, fmt.Errorf("infer: unknown effort %q", effort)
	}
	// New proved every effort has an engine and a timeout, so these lookups
	// cannot miss for a valid effort.
	engine := c.cfg.Engines[effort]
	timeout := c.cfg.Timeouts[effort]

	// Jeff's rule: the deadline rides context.WithTimeout, never a parallel
	// manual clock check.  The openai client sets no timeout of its own; this
	// context is the whole bound on the turn.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Measure via the injected clock so elapsed is deterministic under test.
	start := c.cfg.Now()
	reply, tokens, err := engine.Completer.Complete(ctx, messages, nil)
	elapsed := c.cfg.Now().Sub(start)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Answer:   stripReasoning(reply.Content),
		ServedBy: engine.Name,
		Elapsed:  elapsed,
		Tokens:   tokens,
	}, nil
}

// stripReasoning removes any inline <think>…</think> chain-of-thought from the
// model's content and trims surrounding whitespace.  A thorough answer must
// cost the caller no extra context: the reasoning trace is engine-side work,
// spent to reach the answer and then discarded here so the caller pays only
// for the answer itself.  (Separate reasoning_content wire fields need no
// handling — openai.Message does not decode them, so they never arrive.)
func stripReasoning(content string) string {
	return strings.TrimSpace(thinkTagPattern.ReplaceAllString(content, ""))
}
