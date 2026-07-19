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
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ClassName is the identifier of a named engine class — the thing a consumer
// asks for in the request's model field (docs/agency.md: classes, not URLs).
// The underlying string is private: a non-zero ClassName exists only via the
// validating ParseClassName, so holding one means the name was validated.
type ClassName struct {
	s string
}

// classNameRE bounds the alphabet to what model tags themselves use (letters,
// digits, ".", "_", ":", "/", "-") with an alphanumeric first character, so a
// class name is always safe to echo into headers, log lines, and error
// bodies.  Length is capped at 128.
var classNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// ParseClassName validates an untrusted string (a request's model field, a
// config key) and returns it as a ClassName.  Anything outside the canonical
// form is rejected, never coerced.
func ParseClassName(s string) (ClassName, error) {
	if !classNameRE.MatchString(s) {
		return ClassName{}, fmt.Errorf("invalid engine class name %q", s)
	}
	return ClassName{s: s}, nil
}

// String returns the canonical form ("" for the zero ClassName).
func (c ClassName) String() string { return c.s }

// Duration is a time.Duration parsed from a YAML duration STRING (e.g. "90s",
// "5m") so a config field carries its unit explicitly — never a bare number
// whose unit survives only in the field name.
type Duration time.Duration

// UnmarshalYAML parses a duration string via time.ParseDuration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like %q: %w", "90s", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the standard-library time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalJSON emits the duration as a string ("90s"), so the status
// snapshot carries units explicitly — never a bare number.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Std().String())
}

// UnmarshalJSON parses the string form, so snapshot readers round-trip.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a JSON string like %q: %w", "90s", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Priority is the queueing class of an engine class.  When a node slot
// frees, interactive waiters are granted ahead of batch — and batch drains
// whenever no interactive request is waiting, so it never starves forever.
type Priority string

const (
	PriorityInteractive Priority = "interactive"
	PriorityBatch       Priority = "batch"
)

// routerFile is the on-disk YAML shape of the routing config.  It is decoded
// with KnownFields so a typo'd key is a startup error, then resolved into the
// validated RouterConfig — the file shape never routes a request directly.
type routerFile struct {
	// Probe governs node presence detection.
	Probe probeFile `yaml:"probe"`
	// Nodes maps a node name to its model server.
	Nodes map[string]nodeFile `yaml:"nodes"`
	// Classes maps an engine-class name to its route.
	Classes map[string]classFile `yaml:"classes"`
}

type probeFile struct {
	// Interval is how often every node is probed for presence.  Required.
	Interval Duration `yaml:"interval"`
	// Timeout bounds one node's probe; an unanswered probe marks the node
	// absent.  Required, at most Interval.
	Timeout Duration `yaml:"timeout"`
}

type nodeFile struct {
	// URL is the node's base URL, scheme://host[:port] only — no path,
	// query, or credentials.  Required.
	URL string `yaml:"url"`
	// MaxInFlight is how many requests the agency lets run on the node at
	// once; beyond it, requests wait in the door's priority queue rather
	// than piling into the model server's blind FIFO.  Required, > 0.
	MaxInFlight int `yaml:"maxInFlight"`
	// Models is the CLOSED set of model tags allowed on this node —
	// typically one on a single-GPU node, a few on a node with room for
	// them.  Every chain link naming this node must pick from the set, so
	// nothing routable can ever trigger an eviction: the never-evict rule
	// is enforced by construction at config load, not arbitrated at
	// request time.  Required, at least one tag.
	Models []string `yaml:"models"`
}

type classFile struct {
	// Priority is the class's queueing class: interactive or batch.
	// Required.
	Priority Priority `yaml:"priority"`
	// Deadline is the default total operation budget (queue + decode) when
	// the caller sends none.  Required.
	Deadline Duration `yaml:"deadline"`
	// MaxDeadline is the hard cap: a caller may tighten its budget below
	// Deadline but never stretch it past MaxDeadline.  Required.
	MaxDeadline Duration `yaml:"maxDeadline"`
	// QueueWait is the default per-link queue budget when the caller sends
	// none: waiting longer than this for a node slot advances the chain —
	// a too-busy link is an unavailable link.  Required.
	QueueWait Duration `yaml:"queueWait"`
	// MaxQueueWait is the hard cap on the caller's queue budget, like
	// MaxDeadline for Deadline.  Required.
	MaxQueueWait Duration `yaml:"maxQueueWait"`
	// Chain is the ordered fallback chain; exhausting it is a refusal,
	// never a silent substitute.  Required, at least one link.
	Chain []chainLinkFile `yaml:"chain"`
}

type chainLinkFile struct {
	Node  string `yaml:"node"`
	Model string `yaml:"model"`
}

// RouterConfig is the validated, resolved engine-class routing table.  It is
// constructed only by LoadRouterConfig, so holding one means every class
// names a full chain of resolvable links.
type RouterConfig struct {
	probeInterval time.Duration
	probeTimeout  time.Duration
	nodes         map[string]nodeInfo
	classes       map[ClassName]classRoute
}

// nodeInfo is one resolved node.
type nodeInfo struct {
	url         *url.URL
	maxInFlight int
	models      []string // the pinned model set, in config order
}

// classRoute is one resolved engine class.
type classRoute struct {
	name         ClassName
	priority     Priority
	deadline     time.Duration // default total operation budget
	maxDeadline  time.Duration // hard cap a caller may not exceed
	queueWait    time.Duration // default per-link queue budget
	maxQueueWait time.Duration // hard cap on the queue budget
	links        []engineLink  // ordered fallback chain
}

// engineLink is one resolved (node, model) chain link.
type engineLink struct {
	node  string   // config node name, reported in Agency-Served-By
	url   *url.URL // the node's base URL, scheme://host[:port]
	model string   // the model tag the node is asked for
}

// servedBy is the provenance string reported when this link answers.
func (l engineLink) servedBy() string { return l.node + "/" + l.model }

// classNames returns the configured class names, sorted, for the synthesized
// /v1/models listing.
func (c *RouterConfig) classNames() []ClassName {
	names := make([]ClassName, 0, len(c.classes))
	for name := range c.classes {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b ClassName) int { return strings.Compare(a.s, b.s) })
	return names
}

// classList renders the configured class names, sorted and comma-joined, for
// refusal bodies: a caller told only "unknown class" would have to probe
// /v1/models to learn what it should have asked for.
func (c *RouterConfig) classList() string {
	names := c.classNames()
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = name.String()
	}
	return strings.Join(parts, ", ")
}

// LoadRouterConfig reads and validates the routing config.  It is
// FAIL-CLOSED: a missing or unparseable file, an unknown key, or any
// unset/invalid field is an error — the caller aborts startup rather than
// route on a table it did not choose.
func LoadRouterConfig(path string) (*RouterConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agency: read config %q: %w", path, err)
	}
	cfg, err := parseRouterConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("agency: config %q: %w", path, err)
	}
	return cfg, nil
}

// parseRouterConfig decodes and resolves the raw YAML bytes.
func parseRouterConfig(raw []byte) (*RouterConfig, error) {
	var f routerFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a typo'd key is a fail-closed error, not a silent no-op
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if f.Probe.Interval <= 0 {
		return nil, fmt.Errorf("probe.interval is required and must be > 0 (e.g. \"15s\")")
	}
	if f.Probe.Timeout <= 0 {
		return nil, fmt.Errorf("probe.timeout is required and must be > 0 (e.g. \"3s\")")
	}
	if f.Probe.Timeout > f.Probe.Interval {
		return nil, fmt.Errorf("probe.timeout %s exceeds probe.interval %s", f.Probe.Timeout.Std(), f.Probe.Interval.Std())
	}
	if len(f.Nodes) == 0 {
		return nil, fmt.Errorf("nodes must list at least one node")
	}
	nodes := make(map[string]nodeInfo, len(f.Nodes))
	for name, nf := range f.Nodes {
		if name == "" {
			return nil, fmt.Errorf("nodes: a node name must not be empty")
		}
		u, err := parseNodeURL(nf.URL)
		if err != nil {
			return nil, fmt.Errorf("nodes.%s: %w", name, err)
		}
		if nf.MaxInFlight <= 0 {
			return nil, fmt.Errorf("nodes.%s: maxInFlight is required and must be > 0", name)
		}
		if len(nf.Models) == 0 {
			return nil, fmt.Errorf("nodes.%s: models must pin at least one model tag", name)
		}
		for i, model := range nf.Models {
			if model == "" {
				return nil, fmt.Errorf("nodes.%s: models[%d] is empty", name, i)
			}
			if slices.Contains(nf.Models[:i], model) {
				return nil, fmt.Errorf("nodes.%s: models lists %q twice", name, model)
			}
		}
		nodes[name] = nodeInfo{url: u, maxInFlight: nf.MaxInFlight, models: nf.Models}
	}
	if len(f.Classes) == 0 {
		return nil, fmt.Errorf("classes must list at least one engine class")
	}
	cfg := &RouterConfig{
		probeInterval: f.Probe.Interval.Std(),
		probeTimeout:  f.Probe.Timeout.Std(),
		nodes:         nodes,
		classes:       make(map[ClassName]classRoute, len(f.Classes)),
	}
	for rawName, cf := range f.Classes {
		name, err := ParseClassName(rawName)
		if err != nil {
			return nil, fmt.Errorf("classes: %w", err)
		}
		route, err := resolveClass(name, cf, nodes)
		if err != nil {
			return nil, fmt.Errorf("classes.%s: %w", name, err)
		}
		cfg.classes[name] = route
	}
	return cfg, nil
}

// parseNodeURL validates a node's base URL: http(s), a host, and nothing
// else — the request's own path is appended at forward time, so a path,
// query, or credentials in the base would silently rewrite every request.
func parseNodeURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL %q: scheme must be http or https", rawURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL %q has no host", rawURL)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("URL %q: must be scheme://host[:port] only", rawURL)
	}
	return u, nil
}

// resolveClass validates one class and resolves its chain against the node
// table.
func resolveClass(name ClassName, cf classFile, nodes map[string]nodeInfo) (classRoute, error) {
	switch cf.Priority {
	case PriorityInteractive, PriorityBatch:
	case "":
		return classRoute{}, fmt.Errorf("priority is required (interactive or batch)")
	default:
		return classRoute{}, fmt.Errorf("priority %q: want interactive or batch", cf.Priority)
	}
	if cf.Deadline <= 0 {
		return classRoute{}, fmt.Errorf("deadline is required and must be > 0 (e.g. \"90s\")")
	}
	if cf.MaxDeadline <= 0 {
		return classRoute{}, fmt.Errorf("maxDeadline is required and must be > 0 (e.g. \"5m\")")
	}
	if cf.Deadline > cf.MaxDeadline {
		return classRoute{}, fmt.Errorf("deadline %s exceeds maxDeadline %s", cf.Deadline.Std(), cf.MaxDeadline.Std())
	}
	if cf.QueueWait <= 0 {
		return classRoute{}, fmt.Errorf("queueWait is required and must be > 0 (e.g. \"10s\")")
	}
	if cf.MaxQueueWait <= 0 {
		return classRoute{}, fmt.Errorf("maxQueueWait is required and must be > 0 (e.g. \"1m\")")
	}
	if cf.QueueWait > cf.MaxQueueWait {
		return classRoute{}, fmt.Errorf("queueWait %s exceeds maxQueueWait %s", cf.QueueWait.Std(), cf.MaxQueueWait.Std())
	}
	if len(cf.Chain) == 0 {
		return classRoute{}, fmt.Errorf("chain must list at least one link")
	}
	route := classRoute{
		name:         name,
		priority:     cf.Priority,
		deadline:     cf.Deadline.Std(),
		maxDeadline:  cf.MaxDeadline.Std(),
		queueWait:    cf.QueueWait.Std(),
		maxQueueWait: cf.MaxQueueWait.Std(),
		links:        make([]engineLink, 0, len(cf.Chain)),
	}
	for i, link := range cf.Chain {
		node, ok := nodes[link.Node]
		if !ok {
			return classRoute{}, fmt.Errorf("chain[%d]: unknown node %q", i, link.Node)
		}
		if link.Model == "" {
			return classRoute{}, fmt.Errorf("chain[%d]: model is required", i)
		}
		// The never-evict invariant: a chain may only ask a node for a
		// model pinned there, so no routable request can force a load
		// that displaces another resident.
		if !slices.Contains(node.models, link.Model) {
			return classRoute{}, fmt.Errorf("chain[%d]: model %q is not pinned on node %q (pinned: %s)",
				i, link.Model, link.Node, strings.Join(node.models, ", "))
		}
		route.links = append(route.links, engineLink{node: link.Node, url: node.url, model: link.Model})
	}
	return route, nil
}
