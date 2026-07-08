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
	"context"
	"encoding/json"

	"github.com/jeffbstewart/cloister/internal/openai"
)

// The OpenAI-compatible wire types and HTTP client now live in the shared,
// stdlib-only internal/openai package.  These aliases keep the scholar's
// existing call sites (loop.go, server.go, the tests) unchanged while it
// consumes the shared client.
type (
	Message      = openai.Message
	ToolCall     = openai.ToolCall
	Tool         = openai.Tool
	ToolFunction = openai.ToolFunction
)

// Completer drives one chat turn.  *openai.Client is the production impl; tests
// substitute a scripted stub, and a hosted model is a drop-in.
type Completer interface {
	Complete(ctx context.Context, messages []Message, tools []Tool) (reply Message, tokens int, err error)
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
