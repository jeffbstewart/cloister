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

package infer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeffbstewart/cloister/internal/openai"
)

// fakeCompleter is a scripted Completer: it returns a fixed reply, token
// count, and error, and records what it was called with.  It never touches
// the network.
type fakeCompleter struct {
	reply  openai.Message
	tokens int
	err    error

	// observe, if set, is called with the context so a test can assert on the
	// deadline the Client applied — without racing on wall-clock time.
	observe func(ctx context.Context)

	gotMessages []openai.Message
	gotTools    []openai.Tool
	called      bool
}

func (f *fakeCompleter) Complete(ctx context.Context, messages []openai.Message, tools []openai.Tool) (openai.Message, int, error) {
	f.called = true
	f.gotMessages = messages
	f.gotTools = tools
	if f.observe != nil {
		f.observe(ctx)
	}
	return f.reply, f.tokens, f.err
}

// stepClock is an injected clock that advances by a fixed delta on each read,
// so elapsed time is deterministic: the first read (start) and the second
// (end) differ by exactly one step.
type stepClock struct {
	now  time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	t := c.now
	c.now = c.now.Add(c.step)
	return t
}

// twoEngineConfig builds a Config wiring Quick and Thorough to the given
// completers, with a deterministic clock advancing by step per read.
func twoEngineConfig(quick, thorough Completer, step time.Duration) Config {
	clock := &stepClock{now: time.Unix(1_700_000_000, 0), step: step}
	return Config{
		Engines: map[Effort]Engine{
			Quick:    {Name: "think-fast", Completer: quick},
			Thorough: {Name: "deep-think", Completer: thorough},
		},
		Now: clock.Now,
	}
}

func TestEffortValid(t *testing.T) {
	for _, e := range []Effort{Quick, Thorough} {
		if !e.Valid() {
			t.Errorf("Effort %q should be valid", e)
		}
	}
	for _, e := range []Effort{"", "medium", "QUICK", "deep"} {
		if e.Valid() {
			t.Errorf("Effort %q should be invalid", e)
		}
	}
}

func TestAskRoutesByEffort(t *testing.T) {
	quick := &fakeCompleter{reply: openai.Message{Content: "fast answer"}}
	thorough := &fakeCompleter{reply: openai.Message{Content: "deep answer"}}
	client, err := New(twoEngineConfig(quick, thorough, 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := client.Ask(context.Background(), Quick, nil)
	if err != nil {
		t.Fatalf("Ask(Quick): %v", err)
	}
	if got.ServedBy != "think-fast" {
		t.Errorf("Quick served by %q, want think-fast", got.ServedBy)
	}
	if got.Answer != "fast answer" {
		t.Errorf("Quick answer = %q", got.Answer)
	}
	if !quick.called || thorough.called {
		t.Errorf("Quick routed wrong: quick.called=%v thorough.called=%v", quick.called, thorough.called)
	}

	quick.called, thorough.called = false, false
	got, err = client.Ask(context.Background(), Thorough, nil)
	if err != nil {
		t.Fatalf("Ask(Thorough): %v", err)
	}
	if got.ServedBy != "deep-think" {
		t.Errorf("Thorough served by %q, want deep-think", got.ServedBy)
	}
	if !thorough.called || quick.called {
		t.Errorf("Thorough routed wrong: quick.called=%v thorough.called=%v", quick.called, thorough.called)
	}
}

func TestAskStripsChainOfThought(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"single span", "<think>reasoning</think>the answer", "the answer"},
		{"multiline span", "<think>line one\nline two</think>\n\nthe answer", "the answer"},
		{"case-insensitive tag", "<THINK>noise</Think>the answer", "the answer"},
		{"multiple spans", "<think>a</think>keep<think>b</think> me", "keep me"},
		{"no think tag", "just the answer", "just the answer"},
		{"leading/trailing whitespace", "  the answer  ", "the answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeCompleter{reply: openai.Message{Content: tc.content}}
			client, err := New(twoEngineConfig(fake, &fakeCompleter{}, 0))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := client.Ask(context.Background(), Quick, nil)
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if got.Answer != tc.want {
				t.Errorf("Answer = %q, want %q", got.Answer, tc.want)
			}
		})
	}
}

func TestAskElapsedIsDeterministic(t *testing.T) {
	const step = 250 * time.Millisecond
	fake := &fakeCompleter{reply: openai.Message{Content: "ok"}}
	client, err := New(twoEngineConfig(fake, &fakeCompleter{}, step))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.Ask(context.Background(), Quick, nil)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	// The clock advances one step between the start and end reads.
	if got.Elapsed != step {
		t.Errorf("Elapsed = %v, want %v", got.Elapsed, step)
	}
}

func TestAskPropagatesTokens(t *testing.T) {
	fake := &fakeCompleter{reply: openai.Message{Content: "ok"}, tokens: 5912}
	client, err := New(twoEngineConfig(fake, &fakeCompleter{}, 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.Ask(context.Background(), Quick, nil)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Tokens != 5912 {
		t.Errorf("Tokens = %d, want 5912", got.Tokens)
	}
}

func TestAskPassesNoTools(t *testing.T) {
	fake := &fakeCompleter{reply: openai.Message{Content: "ok"}}
	client, err := New(twoEngineConfig(fake, &fakeCompleter{}, 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Ask(context.Background(), Quick, nil); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if fake.gotTools != nil {
		t.Errorf("Complete got tools %v, want nil (comprehension is single-shot)", fake.gotTools)
	}
}

func TestAskUnknownEffort(t *testing.T) {
	client, err := New(twoEngineConfig(&fakeCompleter{}, &fakeCompleter{}, 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Ask(context.Background(), Effort("medium"), nil); err == nil {
		t.Fatal("Ask with unknown effort should error")
	}
}

func TestNewRejectsMissingEngine(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			name: "missing thorough",
			cfg: Config{Engines: map[Effort]Engine{
				Quick: {Name: "think-fast", Completer: &fakeCompleter{}},
			}},
		},
		{
			name: "missing quick",
			cfg: Config{Engines: map[Effort]Engine{
				Thorough: {Name: "deep-think", Completer: &fakeCompleter{}},
			}},
		},
		{
			name: "engine with no name",
			cfg: Config{Engines: map[Effort]Engine{
				Quick:    {Completer: &fakeCompleter{}},
				Thorough: {Name: "deep-think", Completer: &fakeCompleter{}},
			}},
		},
		{
			name: "engine with no completer",
			cfg: Config{Engines: map[Effort]Engine{
				Quick:    {Name: "think-fast"},
				Thorough: {Name: "deep-think", Completer: &fakeCompleter{}},
			}},
		},
		{
			name: "nil engines",
			cfg:  Config{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Error("New should reject config, got nil error")
			}
		})
	}
}

func TestAskPropagatesCompleteError(t *testing.T) {
	wantErr := errors.New("engine down")
	fake := &fakeCompleter{err: wantErr}
	client, err := New(twoEngineConfig(fake, &fakeCompleter{}, 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Ask(context.Background(), Quick, nil); !errors.Is(err, wantErr) {
		t.Errorf("Ask error = %v, want %v (fail closed)", err, wantErr)
	}
}

func TestAskAppliesDeadline(t *testing.T) {
	// Assert the Client set a context deadline inside the call, rather than
	// racing on real time.  The default Quick timeout is 60s, so the deadline
	// must land in the future and within that bound.
	var (
		hadDeadline bool
		deadline    time.Time
	)
	fake := &fakeCompleter{
		reply: openai.Message{Content: "ok"},
		observe: func(ctx context.Context) {
			deadline, hadDeadline = ctx.Deadline()
		},
	}
	client, err := New(twoEngineConfig(fake, &fakeCompleter{}, 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Ask(context.Background(), Quick, nil); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !hadDeadline {
		t.Fatal("Complete's context had no deadline; effort timeout was not applied")
	}
	if until := time.Until(deadline); until <= 0 || until > DefaultQuickTimeout {
		t.Errorf("deadline %v is out of the (0, %v] window from now", until, DefaultQuickTimeout)
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	if cfg.Timeouts[Quick] != DefaultQuickTimeout {
		t.Errorf("Quick timeout = %v, want %v", cfg.Timeouts[Quick], DefaultQuickTimeout)
	}
	if cfg.Timeouts[Thorough] != DefaultThoroughTimeout {
		t.Errorf("Thorough timeout = %v, want %v", cfg.Timeouts[Thorough], DefaultThoroughTimeout)
	}
	if cfg.Now == nil {
		t.Error("Now should default to a non-nil clock")
	}

	// ApplyDefaults must not clobber a caller-set timeout.
	custom := Config{Timeouts: map[Effort]time.Duration{Quick: time.Second}}
	custom.ApplyDefaults()
	if custom.Timeouts[Quick] != time.Second {
		t.Errorf("ApplyDefaults clobbered a set timeout: got %v", custom.Timeouts[Quick])
	}
}
