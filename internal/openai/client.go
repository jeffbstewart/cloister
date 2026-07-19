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

// Package openai is a worker-agnostic client for an OpenAI-compatible
// chat-completions endpoint (the local `infer`/ollama service, or a hosted
// model).  It carries only the wire types and the single-endpoint HTTP
// plumbing; higher-level policy (engine routing, tool menus, loops) lives in
// the callers.  It imports only the Go standard library so it can join the
// librarian's stdlib-only import graph.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Message is one OpenAI-compatible chat message.  A tool result carries Role
// "tool", the ToolCallID it answers, and the result text in Content.
type Message struct {
	Role string `json:"role"`
	// Content is always emitted (no omitempty): an assistant message that is a
	// pure tool call has empty content, and ollama rejects a MISSING content field
	// ("invalid message content type: <nil>").  An empty string is accepted.
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one function call the model asked for.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON object, as a string
	} `json:"function"`
}

// Tool is a function definition offered to the model.
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// CallerHeader carries the worker's self-declared identity to the agency,
// which reports it in its status snapshot's op ledger.  The name must match
// internal/agency.CallerHeader (pinned equal by a test there); it stays a
// separate constant so this package keeps its zero-import posture.
const CallerHeader = "Agency-Caller"

// Client talks to an OpenAI-compatible /chat/completions endpoint (the
// local `infer`, or a hosted model).  It is a plain client to the
// CONFIGURED base URL — not arbitrary egress: the model endpoint is a fixed
// internal host, and the boot self-check proves no arbitrary route
// exists.
type Client struct {
	base   string // e.g. http://infer:11434/v1
	model  string
	key    string // optional bearer (empty for local infer)
	caller string // self-declared identity for the agency's op ledger
	hc     *http.Client
}

// Options configures the client.
type Options struct {
	BaseURL string // OpenAI-compatible base, e.g. http://infer:11434/v1
	Model   string // model tag
	Key     string // optional bearer (empty for local infer)
	// Caller is the worker's name (e.g. "librarian"), sent as CallerHeader
	// so the agency's op ledger attributes the work; empty sends nothing
	// and the agency falls back to the remote host.
	Caller string
}

// New builds the client.  It sets no client-level timeout: each turn is
// bounded by the caller's context deadline (the WallClock cap), which
// cancels the request; a fixed per-request timeout would cut off a slow deep
// model mid-turn.
func New(opts Options) *Client {
	return &Client{
		base:   strings.TrimRight(opts.BaseURL, "/"),
		model:  opts.Model,
		key:    opts.Key,
		caller: opts.Caller,
		hc:     &http.Client{},
	}
}

// Complete runs one chat turn: it posts messages and tools to the endpoint
// and returns the reply message plus the total token count reported in usage.
func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool) (Message, int, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model":    c.model,
		"messages": messages,
		"tools":    tools,
	})
	if err != nil {
		return Message{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return Message{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if c.caller != "" {
		req.Header.Set(CallerHeader, c.caller)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Message{}, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Message{}, 0, err
	}
	if resp.StatusCode/100 != 2 {
		return Message{}, 0, fmt.Errorf("model %s: %s: %s", req.URL.Host, resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Message{}, 0, fmt.Errorf("model: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Message{}, out.Usage.TotalTokens, fmt.Errorf("model: response had no choices")
	}
	return out.Choices[0].Message, out.Usage.TotalTokens, nil
}
