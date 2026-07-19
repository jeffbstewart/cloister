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
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseRouterConfig(t *testing.T) {
	cfg, err := parseRouterConfig([]byte(`
probe:
  interval: 15s
  timeout: 3s
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
    models:
      - coder-model:30b
  macbook:
    url: http://deep-think-node:11434
    maxInFlight: 2
    models:
      - big-moe:latest
      - big-moe:small
classes:
  interactive-code:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m
    chain:
      - node: infer
        model: coder-model:30b
  deep-think:
    priority: batch
    deadline: 2m
    maxDeadline: 10m
    queueWait: 30s
    maxQueueWait: 5m
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
	if cfg.probeInterval != 15*time.Second || cfg.probeTimeout != 3*time.Second {
		t.Errorf("probe = %s/%s, want 15s/3s", cfg.probeInterval, cfg.probeTimeout)
	}
	if node := cfg.nodes["macbook"]; node.maxInFlight != 2 || node.url.Host != "deep-think-node:11434" ||
		!slices.Equal(node.models, []string{"big-moe:latest", "big-moe:small"}) {
		t.Errorf("macbook node = %+v, want maxInFlight 2 at deep-think-node:11434 pinning both big-moe tags", node)
	}

	name, err := ParseClassName("deep-think")
	if err != nil {
		t.Fatal(err)
	}
	route := cfg.classes[name]
	if route.priority != PriorityBatch {
		t.Errorf("deep-think priority = %q, want batch", route.priority)
	}
	if route.deadline != 2*time.Minute || route.maxDeadline != 10*time.Minute {
		t.Errorf("deep-think deadlines = %s/%s, want 2m/10m", route.deadline, route.maxDeadline)
	}
	if route.queueWait != 30*time.Second || route.maxQueueWait != 5*time.Minute {
		t.Errorf("deep-think queue budgets = %s/%s, want 30s/5m", route.queueWait, route.maxQueueWait)
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
	// validProbe/validNodes/validClassFields/validChain cover every required
	// field; each case below breaks exactly one thing.
	const validProbe = `
probe:
  interval: 15s
  timeout: 3s`
	const validNodes = validProbe + `
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
    models:
      - coder-model:30b`
	const validClassFields = `
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m`
	const validChain = `
    chain:
      - node: infer
        model: coder-model:30b`
	cases := []struct {
		name string
		yaml string
	}{
		{"unknown key", validNodes + `
surprise: true
classes:
  chat:` + validClassFields + validChain},
		{"no nodes", validProbe + `
classes:
  chat:` + validClassFields + validChain},
		{"no classes", validNodes},
		{"missing probe", `
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
classes:
  chat:` + validClassFields + validChain},
		{"probe timeout exceeds interval", `
probe:
  interval: 3s
  timeout: 15s
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
classes:
  chat:` + validClassFields + validChain},
		{"node missing maxInFlight", validProbe + `
nodes:
  infer:
    url: http://infer:11434
classes:
  chat:` + validClassFields + validChain},
		{"node URL unparseable", validProbe + `
nodes:
  infer:
    url: "http://bad url"
    maxInFlight: 4
classes:
  chat:` + validClassFields + validChain},
		{"node URL bad scheme", validProbe + `
nodes:
  infer:
    url: ftp://infer:11434
    maxInFlight: 4
classes:
  chat:` + validClassFields + validChain},
		{"node URL with path", validProbe + `
nodes:
  infer:
    url: http://infer:11434/v1
    maxInFlight: 4
classes:
  chat:` + validClassFields + validChain},
		{"node missing models", validProbe + `
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
classes:
  chat:` + validClassFields + validChain},
		{"node model empty", validProbe + `
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
    models:
      - ""
classes:
  chat:` + validClassFields + validChain},
		{"node model duplicated", validProbe + `
nodes:
  infer:
    url: http://infer:11434
    maxInFlight: 4
    models:
      - coder-model:30b
      - coder-model:30b
classes:
  chat:` + validClassFields + validChain},
		{"chain model not pinned on node", validNodes + `
classes:
  chat:` + validClassFields + `
    chain:
      - node: infer
        model: unpinned-model:7b`},
		{"missing priority", validNodes + `
classes:
  chat:
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m` + validChain},
		{"unknown priority", validNodes + `
classes:
  chat:
    priority: urgent
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m` + validChain},
		{"missing deadline", validNodes + `
classes:
  chat:
    priority: interactive
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m` + validChain},
		{"missing maxDeadline", validNodes + `
classes:
  chat:
    priority: interactive
    deadline: 90s
    queueWait: 10s
    maxQueueWait: 1m` + validChain},
		{"deadline exceeds maxDeadline", validNodes + `
classes:
  chat:
    priority: interactive
    deadline: 10m
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m` + validChain},
		{"deadline as bare number", validNodes + `
classes:
  chat:
    priority: interactive
    deadline: 90
    maxDeadline: 5m
    queueWait: 10s
    maxQueueWait: 1m` + validChain},
		{"missing queueWait", validNodes + `
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    maxQueueWait: 1m` + validChain},
		{"missing maxQueueWait", validNodes + `
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 10s` + validChain},
		{"queueWait exceeds maxQueueWait", validNodes + `
classes:
  chat:
    priority: interactive
    deadline: 90s
    maxDeadline: 5m
    queueWait: 2m
    maxQueueWait: 1m` + validChain},
		{"empty chain", validNodes + `
classes:
  chat:` + validClassFields + `
    chain: []`},
		{"chain names unknown node", validNodes + `
classes:
  chat:` + validClassFields + `
    chain:
      - node: ghost
        model: coder-model:30b`},
		{"chain link missing model", validNodes + `
classes:
  chat:` + validClassFields + `
    chain:
      - node: infer`},
		{"invalid class name", validNodes + `
classes:
  "bad name":` + validClassFields + validChain},
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

// TestDurationJSON: the snapshot wire form is a duration STRING, and it
// round-trips.
func TestDurationJSON(t *testing.T) {
	b, err := json.Marshal(Duration(90 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"1m30s"` {
		t.Errorf("marshal = %s, want a duration string", b)
	}
	var d Duration
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Std() != 90*time.Second {
		t.Errorf("round-trip = %s, want 90s", d.Std())
	}
	if err := json.Unmarshal([]byte(`90`), &d); err == nil {
		t.Error("bare-number duration unmarshaled, want refusal")
	}
}

// TestCommittedReferenceRoutesAreValid loads the repo's reference routing
// config, so schema drift — a renamed field, a new required one — fails the
// suite instead of the deployed door's startup.
func TestCommittedReferenceRoutesAreValid(t *testing.T) {
	cfg, err := LoadRouterConfig(filepath.Join("..", "..", "etc", "agency-routes.yaml"))
	if err != nil {
		t.Fatalf("reference config invalid: %v", err)
	}
	for _, class := range []string{"interactive-code", "think-fast", "deep-think", "research", "summarize-cheap", "review"} {
		name, err := ParseClassName(class)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cfg.classes[name]; !ok {
			t.Errorf("reference config missing the %q class", class)
		}
	}
}

func TestLoadRouterConfigMissingFile(t *testing.T) {
	if _, err := LoadRouterConfig("does-not-exist.yaml"); err == nil {
		t.Error("LoadRouterConfig on a missing file succeeded, want error")
	}
}
