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

package agency

import (
	"strings"
	"testing"
	"time"
)

func TestParseRouterConfig(t *testing.T) {
	cfg, err := parseRouterConfig([]byte(`
nodes:
  infer: http://infer:11434
  macbook: http://deep-think-node:11434
classes:
  interactive-code:
    deadline: 90s
    maxDeadline: 5m
    chain:
      - node: infer
        model: coder-model:30b
  deep-think:
    deadline: 2m
    maxDeadline: 10m
    chain:
      - node: macbook
        model: big-moe:latest
      - node: infer
        model: coder-model:30b
`))
	if err != nil {
		t.Fatalf("parseRouterConfig: %v", err)
	}
	if got := len(cfg.classes); got != 2 {
		t.Fatalf("classes = %d, want 2", got)
	}

	name, err := ParseClassName("deep-think")
	if err != nil {
		t.Fatal(err)
	}
	route := cfg.classes[name]
	if route.deadline != 2*time.Minute || route.maxDeadline != 10*time.Minute {
		t.Errorf("deep-think deadlines = %s/%s, want 2m/10m", route.deadline, route.maxDeadline)
	}
	if len(route.links) != 2 {
		t.Fatalf("deep-think chain = %d links, want 2", len(route.links))
	}
	first := route.links[0]
	if first.node != "macbook" || first.model != "big-moe:latest" || first.url.Host != "deep-think-node:11434" {
		t.Errorf("deep-think link[0] = %s -> %s, want macbook (deep-think-node:11434) -> big-moe:latest", first.servedBy(), first.url)
	}

	if got := cfg.classList(); got != "deep-think, interactive-code" {
		t.Errorf("classList = %q, want sorted comma-joined names", got)
	}
}

func TestParseRouterConfigFailsClosed(t *testing.T) {
	const validChain = `
    chain:
      - node: infer
        model: coder-model:30b`
	cases := []struct {
		name string
		yaml string
	}{
		{"unknown key", `
nodes:
  infer: http://infer:11434
surprise: true
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m` + validChain},
		{"no nodes", `
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m` + validChain},
		{"no classes", `
nodes:
  infer: http://infer:11434`},
		{"node URL unparseable", `
nodes:
  infer: "http://bad url"
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m` + validChain},
		{"node URL bad scheme", `
nodes:
  infer: ftp://infer:11434
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m` + validChain},
		{"node URL with path", `
nodes:
  infer: http://infer:11434/v1
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m` + validChain},
		{"missing deadline", `
nodes:
  infer: http://infer:11434
classes:
  chat:
    maxDeadline: 5m` + validChain},
		{"missing maxDeadline", `
nodes:
  infer: http://infer:11434
classes:
  chat:
    deadline: 90s` + validChain},
		{"deadline exceeds maxDeadline", `
nodes:
  infer: http://infer:11434
classes:
  chat:
    deadline: 10m
    maxDeadline: 5m` + validChain},
		{"deadline as bare number", `
nodes:
  infer: http://infer:11434
classes:
  chat:
    deadline: 90
    maxDeadline: 5m` + validChain},
		{"empty chain", `
nodes:
  infer: http://infer:11434
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m
    chain: []`},
		{"chain names unknown node", `
nodes:
  infer: http://infer:11434
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m
    chain:
      - node: ghost
        model: coder-model:30b`},
		{"chain link missing model", `
nodes:
  infer: http://infer:11434
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m
    chain:
      - node: infer`},
		{"invalid class name", `
nodes:
  infer: http://infer:11434
classes:
  "bad name":
    deadline: 90s
    maxDeadline: 5m` + validChain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRouterConfig([]byte(tc.yaml)); err == nil {
				t.Errorf("parseRouterConfig succeeded, want error")
			}
		})
	}
}

func TestParseClassName(t *testing.T) {
	for _, valid := range []string{"chat", "deep-think", "coder-model:30b", "org/model.v2_x"} {
		if _, err := ParseClassName(valid); err != nil {
			t.Errorf("ParseClassName(%q) = %v, want ok", valid, err)
		}
	}
	invalids := []string{"", "bad name", "-leading-dash", ".hidden", "tab\tname", strings.Repeat("x", 129)}
	for _, invalid := range invalids {
		if _, err := ParseClassName(invalid); err == nil {
			t.Errorf("ParseClassName(%q) succeeded, want error", invalid)
		}
	}
}

func TestLoadRouterConfigMissingFile(t *testing.T) {
	if _, err := LoadRouterConfig("does-not-exist.yaml"); err == nil {
		t.Error("LoadRouterConfig on a missing file succeeded, want error")
	}
}
