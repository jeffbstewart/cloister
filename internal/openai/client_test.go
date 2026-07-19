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

package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCompleteRequestShape asserts the outgoing body carries the model,
// messages, and tools, and that a set key produces a bearer header.
func TestCompleteRequestShape(t *testing.T) {
	var gotAuth, gotCaller string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCaller = r.Header.Get(CallerHeader)
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Model: "test-model", Key: "sekret", Caller: "librarian"})
	msgs := []Message{{Role: "user", Content: "hi"}}
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "web_search"}}}
	if _, _, err := c.Complete(context.Background(), msgs, tools); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sekret")
	}
	if gotCaller != "librarian" {
		t.Errorf("%s = %q, want the self-declared caller", CallerHeader, gotCaller)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("model = %v, want test-model", gotBody["model"])
	}
	gotMsgs, ok := gotBody["messages"].([]any)
	if !ok || len(gotMsgs) != 1 {
		t.Fatalf("messages = %v, want one message", gotBody["messages"])
	}
	if m := gotMsgs[0].(map[string]any); m["role"] != "user" || m["content"] != "hi" {
		t.Errorf("message[0] = %v, want role=user content=hi", m)
	}
	gotTools, ok := gotBody["tools"].([]any)
	if !ok || len(gotTools) != 1 {
		t.Fatalf("tools = %v, want one tool", gotBody["tools"])
	}
	if fn := gotTools[0].(map[string]any)["function"].(map[string]any); fn["name"] != "web_search" {
		t.Errorf("tool[0] function name = %v, want web_search", fn["name"])
	}
}

// TestCompleteNoKeyNoBearer confirms an empty key sends no Authorization
// header (the local infer case), and an empty caller sends no attribution
// header.
func TestCompleteNoKeyNoBearer(t *testing.T) {
	var hadAuth, hadCaller bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		_, hadCaller = r.Header[CallerHeader]
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Model: "m"})
	if _, _, err := c.Complete(context.Background(), nil, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if hadAuth {
		t.Error("Authorization header sent with an empty key")
	}
	if hadCaller {
		t.Error("caller header sent with an empty caller")
	}
}

// TestCompleteParsesReply checks a normal response yields the reply message
// and the total token count from usage.
func TestCompleteParsesReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"the answer",` +
			`"tool_calls":[{"id":"c1","type":"function","function":{"name":"respond","arguments":"{}"}}]}}],` +
			`"usage":{"total_tokens":42}}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Model: "m"})
	reply, tokens, err := c.Complete(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reply.Role != "assistant" || reply.Content != "the answer" {
		t.Errorf("reply = %+v, want role=assistant content=%q", reply, "the answer")
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].Function.Name != "respond" {
		t.Errorf("reply tool calls = %+v, want one respond call", reply.ToolCalls)
	}
	if tokens != 42 {
		t.Errorf("tokens = %d, want 42", tokens)
	}
}

// TestCompleteNon2xxErrors confirms a non-2xx status becomes an error.
func TestCompleteNon2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Model: "m"})
	if _, _, err := c.Complete(context.Background(), nil, nil); err == nil {
		t.Fatal("Complete returned nil error on a 500 response")
	}
}

// TestCompleteContextCancelled confirms a cancelled context aborts the call.
func TestCompleteContextCancelled(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // hold the request open until the test releases it
		io.WriteString(w, "{}")
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	c := New(Options{BaseURL: srv.URL, Model: "m"})
	if _, _, err := c.Complete(ctx, nil, nil); err == nil {
		t.Fatal("Complete returned nil error with a cancelled context")
	}
}
