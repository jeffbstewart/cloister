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

// Completer drives one chat turn. modelClient is the production impl; tests
// substitute a scripted stub, and a hosted model is a drop-in.
type Completer interface {
	Complete(ctx context.Context, messages []Message, tools []Tool) (reply Message, tokens int, err error)
}

// modelClient talks to an OpenAI-compatible /chat/completions endpoint (the
// local `infer`, or a hosted model).  It is a plain client to the
// CONFIGURED base URL — not arbitrary egress: the model endpoint is a fixed
// internal host, and the boot self-check proves no arbitrary route
// exists.
type modelClient struct {
	base  string // e.g. http://infer:11434/v1
	model string
	key   string // optional bearer (empty for local infer)
	hc    *http.Client
}

// ModelOptions configures the model client.
type ModelOptions struct {
	BaseURL string // OpenAI-compatible base, e.g. http://infer:11434/v1
	Model   string // model tag
	Key     string // optional bearer (empty for local infer)
}

// NewModelClient builds the model client.  It sets no client-level timeout: each
// turn is bounded by the loop's context deadline (the WallClock cap), which
// cancels the request; a fixed per-request timeout would cut off a slow deep
// model mid-turn.
func NewModelClient(opts ModelOptions) *modelClient {
	return &modelClient{
		base:  strings.TrimRight(opts.BaseURL, "/"),
		model: opts.Model,
		key:   opts.Key,
		hc:    &http.Client{},
	}
}

func (m *modelClient) Complete(ctx context.Context, messages []Message, tools []Tool) (Message, int, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model":    m.model,
		"messages": messages,
		"tools":    tools,
	})
	if err != nil {
		return Message{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return Message{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.key != "" {
		req.Header.Set("Authorization", "Bearer "+m.key)
	}
	resp, err := m.hc.Do(req)
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

// toolDefs is the fixed three-tool menu offered to the model every turn.
var toolDefs = []Tool{
	{Type: "function", Function: ToolFunction{
		Name:        "web_search",
		Description: "Search the web. Returns results, each with a title, url, snippet, and an opaque handle to read it.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"the search query"},` +
			`"count":{"type":"integer","description":"how many results (1-10)"}},` +
			`"required":["query"]}`),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "extract_url_as_markdown",
		Description: "Read one page as clean markdown. Pass a search-result handle to read that result (no approval). A raw URL requires operator approval.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"target":{"type":"string","description":"a search-result handle, or a full https URL"}},` +
			`"required":["target"]}`),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "respond",
		Description: "Finish and return the answer with the source URLs you consulted.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"answer":{"type":"string","description":"the answer to the question"},` +
			`"sources":{"type":"array","items":{"type":"string"},"description":"URLs consulted"}},` +
			`"required":["answer"]}`),
	}},
}
