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

// Package mcpclient is the shared client core for cloister's own MCP
// consumers: dial a surface, call a tool, decode its JSON answer, and —
// the part worth having in one place — keep a tool-level REFUSAL
// distinct from a transport failure.
//
// That distinction is the whole reason this is a package rather than a
// few lines in each caller.  "The archivist said no, and here is why"
// and "the archivist is unreachable" call for opposite responses: the
// first is a message to show the human and a decision to make, the
// second is a broken cell.  A client that flattened both into error
// would push that judgement onto every call site, and they would not
// all make it the same way.
//
// Callers wrap this with typed verbs — internal/operator for the
// archivist's operator surface, cmd/git-proxy for its agent surface.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RefusedError is a tool-level refusal: the server answered, and the
// answer was no.  The message is the server's own — it names the failing
// requirement, the unpublished work, the violated rule — and is meant to
// be shown verbatim rather than paraphrased, because the reason is the
// part the reader needs.
type RefusedError struct {
	Verb    string
	Message string
}

func (e *RefusedError) Error() string { return e.Verb + ": " + e.Message }

// Config wires a Client.
type Config struct {
	// URL is the MCP surface, e.g. http://archivist:9600/mcp.
	URL string
	// Name identifies this client in the handshake; it reaches the
	// server's logs, so make it say who is calling.
	Name string
	// Version is this client's version, for the same handshake.
	Version string
}

// Client is one connected MCP session.
type Client struct {
	sess *mcp.ClientSession
}

// Dial connects.  The caller closes the Client.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcpclient: no URL")
	}
	if cfg.Name == "" {
		cfg.Name = "cloister"
	}
	c := mcp.NewClient(&mcp.Implementation{Name: cfg.Name, Version: cfg.Version}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: cfg.URL}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: dialing %s: %w", cfg.URL, err)
	}
	return &Client{sess: sess}, nil
}

// Close ends the session.
func (c *Client) Close() error { return c.sess.Close() }

// Call invokes one tool and unmarshals its JSON answer into out (nil to
// discard).  A tool-level error becomes a *RefusedError; anything else
// is a transport or decode failure.
func (c *Client) Call(ctx context.Context, name string, args map[string]any, out any) error {
	res, err := c.sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return fmt.Errorf("mcpclient: calling %s: %w", name, err)
	}
	text := firstText(res)
	if res.IsError {
		return &RefusedError{Verb: name, Message: text}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("mcpclient: unparseable %s answer %q: %w", name, text, err)
	}
	return nil
}

func firstText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
