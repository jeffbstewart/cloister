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

// Package mcpserve is the plumbing every MCP worker surface shares: the
// /mcp + /healthz handler, the schema shorthands, strict argument
// decoding, and the result constructors.  One implementation instead of
// a per-package copy, because the copies had already begun to drift on
// the property that matters most — whether an unknown argument key is
// an error or a silent fall-through.
//
// Two conventions this package hard-codes (docs/worker-roles.md):
//
//   - Tool errors are results, never Go errors: a failed operation
//     returns ErrResult(msg) with a nil error, so callers see a
//     tool-level error rather than a protocol failure.
//   - Unknown argument keys are rejected, never ignored.  On a surface
//     with destructive verbs a misspelled optional key must not
//     quietly select a different shape; on a read surface it must not
//     quietly drop a filter and answer the wrong question with
//     confidence.  Note the raw (*mcp.Server).AddTool path does NOT
//     validate arguments against the input schema — Decode is the
//     enforcement, and NoExtras on the schema is the client-side
//     documentation of the same rule.
package mcpserve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler serves MCP at /mcp and a liveness probe at /healthz.
func Handler(s *mcp.Server) http.Handler {
	return HandlerAt(map[string]*mcp.Server{"/mcp": s})
}

// HandlerAt serves one MCP server per path, plus the /healthz probe —
// how a worker offers two DIFFERENT tool sets to two different callers
// from one process.
//
// Each path is a separate mcp.Server with its own tool registry, which
// is the point: a tool registered on one surface is not merely hidden
// from the other, it is ABSENT — a call for it there answers "unknown
// tool" because the lookup misses.  (The protocol permits advertising
// and calling to disagree, and this SDK could be middleware'd into
// hiding a tool while leaving it callable; that would give obscurity
// whose secret is a guessable tool name.  Two registries give the real
// property.)  The archivist uses this to keep workspace lifecycle out
// of the agent's addressable action set (docs/archivist.md).
//
// This is NOT a security boundary: co-resident callers can reach any
// path.  It bounds what a model can NAME, not what a process can dial.
func HandlerAt(servers map[string]*mcp.Server) http.Handler {
	mux := http.NewServeMux()
	for path, srv := range servers {
		mux.Handle(path, mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return srv }, nil))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	return mux
}

// Str is the string-property schema shorthand.
func Str(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: desc}
}

// Integer is the integer-property schema shorthand.
func Integer(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: desc}
}

// Boolean is the boolean-property schema shorthand.
func Boolean(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: desc}
}

// NoExtras is JSON Schema's `additionalProperties: false`.  Set it on
// every tool's input schema; Decode enforces the same rule server-side.
func NoExtras() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

// Decode unmarshals a tool call's arguments strictly: unknown keys are
// an error.  Empty arguments decode to v's zero value.
func Decode(req *mcp.CallToolRequest, v any) error {
	if len(req.Params.Arguments) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// TextResult wraps prose in a tool result.
func TextResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// ErrResult wraps a failure message in a tool-level error result.
func ErrResult(msg string) *mcp.CallToolResult {
	r := TextResult(msg)
	r.IsError = true
	return r
}

// ProgressNotifier returns a function that sends an MCP progress
// notification to the caller mid-call, or nil if the client supplied no
// progress token (so there is nothing to key progress to).  It is how a
// verb that blocks — the scribe's approval hold, the archivist's
// await_review — tells whoever is driving the session what it is
// waiting on, without unblocking the call.
func ProgressNotifier(ctx context.Context, req *mcp.CallToolRequest) func(string) {
	token := req.Params.GetProgressToken()
	if token == nil {
		return nil
	}
	var n float64
	return func(msg string) {
		n++
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Message:       msg,
			Progress:      n,
		})
	}
}

// JSONResult renders v as indented JSON in a tool result.
func JSONResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ErrResult(fmt.Sprintf("internal: marshal result: %v", err))
	}
	return TextResult(string(b))
}
