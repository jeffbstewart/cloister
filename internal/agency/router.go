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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"
)

// DeadlineHeader is the caller's total operation budget (queue + decode) as a
// Go duration string, e.g. "90s".  Absent, the class default applies; present,
// it may tighten the budget but never stretch it past the class maxDeadline
// (fail-closed: asking for more is a refusal, not a clamp).  The header is a
// control channel to the agency and is stripped before forwarding.
const DeadlineHeader = "Agency-Deadline"

// QueueWaitHeader is the caller's max tolerated PER-LINK queue wait as a Go
// duration string, e.g. "10s".  Waiting longer than this for a node slot
// advances the fallback chain — a too-busy link is an unavailable link.
// Same semantics as DeadlineHeader: class default when absent, tighten-only
// against the class maxQueueWait, stripped before forwarding.
const QueueWaitHeader = "Agency-Queue-Wait"

// servedByHeader reports which engine actually answered, as node/model —
// a reply is never silently attributable to nothing (docs/agency.md).
const servedByHeader = "Agency-Served-By"

// maxRequestBytes caps the buffered request body.  The router must buffer to
// rewrite the model field and replay the body down the chain, so an unbounded
// request would be an unbounded allocation.  Generous against real prompts
// (a full 32k-token context is well under 1 MiB).
const maxRequestBytes = 32 << 20

// dialTimeout bounds CONNECTION establishment to one chain link.  A sleeping
// node blackholes SYNs rather than refusing them; without this bound the
// caller's whole operation budget would burn waiting on the first link
// instead of advancing down the chain.  Decode time is unaffected — this
// clock stops once the connection exists.
const dialTimeout = 5 * time.Second

// errChainExhausted marks the distinct refusal for a class whose every link
// was unavailable — fast and honest, never a mystery stall or a substitute
// outside the chain.
var errChainExhausted = errors.New("no engine in the chain is reachable")

// routeState carries one request's routing facts from the handler through the
// chain transport, via the request context.
type routeState struct {
	class classRoute
	// queueWait is the resolved per-link queue budget for this request.
	queueWait time.Duration
	// fields is the request body as raw top-level JSON fields; the model
	// field is swapped per chain link, everything else is forwarded intact.
	fields map[string]json.RawMessage
	// servedBy is set by the chain transport when a link answers.
	servedBy string
}

type routeStateKey struct{}

// stateFrom returns the request's routeState, or nil outside a routed
// request.
func stateFrom(ctx context.Context) *routeState {
	st, _ := ctx.Value(routeStateKey{}).(*routeState)
	return st
}

// router is the phase-2 /v1 handler: it maps the request's model field — an
// engine-class name, never a URL — to the class's ordered fallback chain and
// forwards to the first link that answers.  Parsing stays minimal
// (containment): top-level JSON fields only, and only the model field is
// touched.
type router struct {
	cfg      *RouterConfig
	presence *presenceTracker
	proxy    *httputil.ReverseProxy
}

// newRouter builds the router.  base is the per-link round-tripper seam; nil
// means the production transport (tests inject a recorder).
func newRouter(cfg *RouterConfig, base http.RoundTripper) *router {
	if base == nil {
		base = &http.Transport{
			// No Proxy function: the door dials its configured nodes
			// directly, never through an environment-supplied proxy.
			DialContext:     (&net.Dialer{Timeout: dialTimeout}).DialContext,
			IdleConnTimeout: 90 * time.Second,
		}
	}
	// One admission gate per node, shared by every class that chains
	// through it — the door is the one queue in front of each model server.
	gates := make(map[string]*priorityGate, len(cfg.nodes))
	for name, node := range cfg.nodes {
		gates[name] = newPriorityGate(node.maxInFlight)
	}
	rt := &router{cfg: cfg, presence: newPresenceTracker(cfg)}
	rt.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The chain transport picks the real target per attempt; point
			// the outbound request at the first link so it starts valid.
			st := stateFrom(pr.Out.Context())
			pr.SetURL(st.class.links[0].url)
			pr.Out.Header.Del(DeadlineHeader)
			pr.Out.Header.Del(QueueWaitHeader)
		},
		// Negative: flush every write through immediately.  A streaming
		// completion must reach the consumer token-by-token; buffering a
		// whole response would turn decode time into dead air.
		FlushInterval: -1,
		Transport:     &chainTransport{base: base, gates: gates, presence: rt.presence},
		ErrorHandler:  routeErrorHandler,
	}
	return rt
}

// ServeHTTP routes one /v1 request.  GET /v1/models lists the classes (they
// are the only models the door serves); everything else must name a
// configured class in its model field or is refused — fail closed, no blind
// forwarding once there is more than one place a request could go.
func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		rt.serveModels(w)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "agency: request body unreadable or over the size cap", http.StatusRequestEntityTooLarge)
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		http.Error(w, "agency: request body is not a JSON object", http.StatusBadRequest)
		return
	}
	rawModel, ok := fields["model"]
	if !ok {
		http.Error(w, fmt.Sprintf("agency: request names no model: set it to an engine class (configured: %s)", rt.cfg.classList()), http.StatusBadRequest)
		return
	}
	var modelStr string
	if err := json.Unmarshal(rawModel, &modelStr); err != nil {
		http.Error(w, "agency: model must be a JSON string naming an engine class", http.StatusBadRequest)
		return
	}
	name, err := ParseClassName(modelStr)
	if err != nil {
		http.Error(w, fmt.Sprintf("agency: %v", err), http.StatusBadRequest)
		return
	}
	route, ok := rt.cfg.classes[name]
	if !ok {
		http.Error(w, fmt.Sprintf("agency: unknown engine class %q: the door serves only configured classes, never a substitute (configured: %s)", name, rt.cfg.classList()), http.StatusNotFound)
		return
	}
	budget, err := requestDeadline(route, r.Header.Get(DeadlineHeader))
	if err != nil {
		http.Error(w, fmt.Sprintf("agency: %v", err), http.StatusBadRequest)
		return
	}
	queueWait, err := requestQueueWait(route, r.Header.Get(QueueWaitHeader))
	if err != nil {
		http.Error(w, fmt.Sprintf("agency: %v", err), http.StatusBadRequest)
		return
	}

	// The budget rides context.WithTimeout — one bound covering the whole
	// forward, never a parallel manual clock check.
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	st := &routeState{class: route, queueWait: queueWait, fields: fields}
	r = r.WithContext(context.WithValue(ctx, routeStateKey{}, st))
	// The body is consumed into fields; the chain transport builds each
	// attempt's body from there.
	r.Body = http.NoBody
	r.ContentLength = 0
	rt.proxy.ServeHTTP(w, r)
}

// requestDeadline resolves the caller's total budget against the class
// policy.
func requestDeadline(route classRoute, header string) (time.Duration, error) {
	return resolveBudget(DeadlineHeader, header, route.deadline, route.maxDeadline, route.name)
}

// requestQueueWait resolves the caller's per-link queue budget against the
// class policy.
func requestQueueWait(route classRoute, header string) (time.Duration, error) {
	return resolveBudget(QueueWaitHeader, header, route.queueWait, route.maxQueueWait, route.name)
}

// resolveBudget resolves one caller-supplied budget header: absent means the
// class default, present may tighten but never exceed the class cap
// (fail-closed — over-asking is refused, not clamped, so a caller finds out
// its assumption is wrong instead of silently running on less).
func resolveBudget(headerName, header string, def, max time.Duration, class ClassName) (time.Duration, error) {
	if header == "" {
		return def, nil
	}
	d, err := time.ParseDuration(header)
	if err != nil {
		return 0, fmt.Errorf("%s %q: want a Go duration like \"90s\": %w", headerName, header, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s %q must be > 0", headerName, header)
	}
	if d > max {
		return 0, fmt.Errorf("%s %s exceeds class %q cap %s", headerName, d, class, max)
	}
	return d, nil
}

// serveModels synthesizes the OpenAI /v1/models listing from the configured
// classes.  Consumers discover classes, not nodes: what lies behind the door
// stays behind it.
func (rt *router) serveModels(w http.ResponseWriter) {
	type model struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	for _, name := range rt.cfg.classNames() {
		list.Data = append(list.Data, model{ID: name.String(), Object: "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(list); err != nil {
		log.Printf("agency: write /v1/models: %v", err)
	}
}

// chainTransport walks the class's fallback chain: each link gets the request
// with the model field rewritten to that link's model; a transport error
// (unreachable, refused, dial timeout) or an exhausted queue wait advances to
// the next link — an HTTP response of any status is an answer from that
// engine and is returned as-is, because masking a served error behind a
// fallback would hide misconfig.
type chainTransport struct {
	base http.RoundTripper
	// gates admit requests to each node up to its maxInFlight, interactive
	// ahead of batch.  A slot is held from admission until the response
	// body is fully consumed — decode time included.
	gates map[string]*priorityGate
	// presence marks which nodes answered the last probe; an absent node's
	// links are skipped without a dial.
	presence *presenceTracker
}

func (t *chainTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	st := stateFrom(req.Context())
	var linkErrs []error
	for _, link := range st.class.links {
		// A burned budget stops the walk: the caller's answer is "deadline
		// exhausted", not "chain exhausted".
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		// An absent node is an unavailable link, skipped without a dial —
		// neither a queue slot nor a dial timeout is spent on it.
		if !t.presence.present(link.node) {
			linkErrs = append(linkErrs, fmt.Errorf("%s: node absent at last probe", link.servedBy()))
			continue
		}
		gate := t.gates[link.node]
		if err := t.admit(req.Context(), gate, st); err != nil {
			if ctxErr := req.Context().Err(); ctxErr != nil {
				return nil, ctxErr
			}
			linkErrs = append(linkErrs, fmt.Errorf("%s: queue wait %s exhausted", link.servedBy(), st.queueWait))
			continue // a too-busy link is an unavailable link
		}
		body, err := bodyForLink(st.fields, link.model)
		if err != nil {
			gate.release()
			return nil, err
		}
		out := req.Clone(req.Context())
		u := *req.URL
		u.Scheme = link.url.Scheme
		u.Host = link.url.Host
		out.URL = &u
		// Present the node's own name as the Host header, as if the
		// consumer had dialed the model server directly.
		out.Host = link.url.Host
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.TransferEncoding = nil
		resp, err := t.base.RoundTrip(out)
		if err != nil {
			gate.release()
			if ctxErr := req.Context().Err(); ctxErr != nil {
				return nil, ctxErr
			}
			linkErrs = append(linkErrs, fmt.Errorf("%s: %w", link.servedBy(), err))
			continue // unavailable means the next link
		}
		st.servedBy = link.servedBy()
		resp.Header.Set(servedByHeader, st.servedBy)
		// The slot stays held while the answer streams; the proxy closes
		// the body after the copy, releasing it.
		resp.Body = &releasingBody{ReadCloser: resp.Body, gate: gate}
		return resp, nil
	}
	return nil, fmt.Errorf("%w: %v", errChainExhausted, errors.Join(linkErrs...))
}

// admit waits for a slot on the link's gate, bounded by the request's queue
// budget and by whatever remains of its total budget.
func (t *chainTransport) admit(ctx context.Context, gate *priorityGate, st *routeState) error {
	waitCtx, cancel := context.WithTimeout(ctx, st.queueWait)
	defer cancel()
	return gate.acquire(waitCtx, st.class.priority)
}

// releasingBody hands the node slot back when the response body is closed —
// the end of the stream, not the start, is when the node's capacity frees.
type releasingBody struct {
	io.ReadCloser
	gate *priorityGate
	once sync.Once
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.gate.release)
	return err
}

// bodyForLink re-serializes the request fields with the model field swapped
// to the link's model tag.  Only the model field changes; every other field's
// bytes pass through verbatim.
func bodyForLink(fields map[string]json.RawMessage, model string) ([]byte, error) {
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("agency: encode model %q: %w", model, err)
	}
	fields["model"] = raw
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("agency: rebuild request body: %w", err)
	}
	return body, nil
}

// routeErrorHandler turns the chain transport's terminal errors into the
// distinct refusals the design promises: a burned budget and an exhausted
// chain each get their own fast, honest answer.
func routeErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	class := "?"
	if st := stateFrom(r.Context()); st != nil {
		class = st.class.name.String()
	}
	log.Printf("agency: class %q: forward %s %s: %v", class, r.Method, r.URL.Path, err)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, fmt.Sprintf("agency: class %q: operation deadline exhausted", class), http.StatusGatewayTimeout)
	case errors.Is(err, errChainExhausted):
		http.Error(w, fmt.Sprintf("agency: class %q: %v", class, errChainExhausted), http.StatusServiceUnavailable)
	default:
		http.Error(w, "agency: forward failed", http.StatusBadGateway)
	}
}
