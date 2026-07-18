# The agency — inference gateway design

Status: **phases 1–2 implemented (the pass-through door; the router —
engine classes, fail-closed config, chains, caller deadlines, the
two-class queue); phases 3–6 are design**.  Decisions from the
2026-07-07 design review.  Lives in the
**shared inference stack** (machine-level, like `infer` itself), not in
any cell.

## Problem

Inference capacity is scarce, stateful, and about to multiply.  Today one
ollama on one GPU serves one resident model, and every consumer dials it
directly; contention between the interactive agent session and background
work is invisible and unmanaged.  The planned deep-think node adds a
second, sometimes-there ollama that can hold several models resident; the
librarian and corrector designs each sketched their own engine-routing
client; frontier (cloud) models may eventually join.  Routing, queueing,
presence, and policy need one owner, or every worker grows a worse copy.

## Ollama's concurrency model (why arbitration, not load balancing)

Three server-side knobs govern an ollama node:

- `OLLAMA_NUM_PARALLEL` — request slots per *loaded* model (continuous
  batching).  KV-cache memory costs `context × slots`, so big-context
  slots are expensive; ollama falls back toward 1 when memory is tight.
- `OLLAMA_MAX_LOADED_MODELS` — how many *different* models stay resident.
  On a 24 GB GPU with a serious coding model: effectively one.  A request
  for a non-resident model triggers evict-and-load — tens of seconds.
- `OLLAMA_MAX_QUEUE` — FIFO beyond available slots, then 503.

So: concurrent within a loaded model, serialized-with-eviction-thrash
across models on one GPU.  Models are heavyweight residents, not
stateless backends — a routing mistake costs a reload, not a packet.
The deep-think node's large unified memory genuinely holds multiple
resident models; the local GPU does not.  (Deployed defaults get verified
empirically against `/api/ps` during implementation.)

## Shape

The **agency**: the sole inference door.

- Every consumer's `OPENAI_BASE_URL` points at the agency; `infer` itself
  moves behind it onto a private net only the agency shares.  One choke
  point, one queue, one policy.  The localhost publish flips with it:
  the infra stack's `127.0.0.1:11434` relay fronts the **agency**, not
  raw ollama — direct host access to the model server disappears.
  (Model staging is unaffected: weights enter via the host-side ollama
  store, per GETTING_STARTED.md.)
- **OpenAI-compatible passthrough**, streaming included: consumers change
  a URL, not their code.
- **Named engine classes, not URLs.**  Consumers request a class
  (`interactive-code`, `deep-think`, `summarize-cheap`, …); fail-closed
  YAML maps each class to an ordered **fallback chain** of
  (node, model) targets — e.g. deep-think: macbook → local-big → refuse.
  Unavailable means the next link, never a silent substitute outside the
  chain; every response carries which engine actually served it.  The
  librarian's and corrector's engine-routing designs collapse into this
  one config.
- **Residency by construction, not arbitration** (2026-07-18 refinement
  of the original route-to-loaded/queue-over-evict design).  Each node's
  config pins the CLOSED set of models allowed there — typically one on
  a single-GPU node, a few where memory holds them — and config
  validation refuses any chain link asking a node for an unpinned model.
  Eviction is not prevented at request time; it is unrequestable: the
  door cannot be asked for anything that would displace a resident.  The
  operator sizes each node's OLLAMA_MAX_LOADED_MODELS to its pinned set.
  Probes assert the pinned set against the node's /api/ps and log
  drift — a pinned model not yet loaded (a cold start after
  reboot/deploy) or a FOREIGN resident (something other than the door
  reached the node).  Residency never steers routing.  A cold pinned
  model is PRELOADED (2026-07-18): the sweep sends ollama's load-only
  request (POST /api/generate, model named, no prompt, keep_alive -1)
  so the model is warm before anyone asks — safe unprompted precisely
  because pinning makes eviction impossible.  Nodes set
  OLLAMA_KEEP_ALIVE=-1 to match: real requests reset the idle timer
  from the server default, so any other value undoes the pin.
- **Session affinity: a non-goal** (2026-07-18).  Ollama's "prompt
  cache" is the per-slot KV cache in GPU memory: llama.cpp reuses the
  longest matching token prefix when a request lands on a slot, so
  "warm" means "this node still holds a slot whose tokens prefix the
  resent transcript" — nothing persists, nothing is shared across
  nodes.  Affinity would only pay when one class load-balances across
  nodes holding the same model; our chains are ordered fallbacks (the
  first present link always serves), and with pinned model sets a model
  rarely lives on two nodes at all.  A conversation changes nodes only
  when a link flaps or is saturated — and then availability beats
  warmth.  Revisit only if a class ever genuinely load-balances.
- **Presence.**  Sometimes-there nodes (a laptop) are probed; chains
  degrade when a node leaves and recover without operator action when it
  returns.
- **Caller-supplied deadlines, human-readable.**  Requests may carry two
  Go-duration-string knobs (house convention: durations are durations,
  e.g. "90s", "5m"): a **total operation deadline** covering queue +
  decode, and a **max tolerated queue wait**.  Exceeding the queue budget
  advances the fallback chain — a too-busy link is an unavailable link —
  and an exhausted chain returns a distinct refusal, fast, rather than a
  mystery stall.  Class config supplies defaults and hard maxima: a
  request may tighten its budgets, never exceed the class cap
  (fail-closed).  Interactive callers ask for short queue budgets and get
  fast honest failures; batch callers ask for long ones and wait their
  turn.
- **Two priority classes, interactive ahead of batch.**  The agent's
  qwen session and the scholar mid-query queue ahead of corrector lens
  passes and librarian summaries.  No preemption in v1: cancelling an
  in-flight decode is mechanically easy (drop the connection) but
  discards all generated tokens, repeats the long-prompt prefill on
  retry, and needs idempotent-retry bookkeeping plus anti-starvation
  guards — all to beat queue-jumping by at most one response-time.
  Deferred, with this analysis, until contention proves it worth it.
- **Frontier models: designed for, not wired.**  A frontier engine class
  slots into the same chain config behind a pinned relay (kagi-relay
  pattern), key in agency env only, per-day spend caps on the scholar's
  burn-ledger pattern — but no cloud credential or relay exists until a
  deliberate later decision.  The gate that decision must clear is
  content policy: prompt bytes leaving the house.  The config carries a
  per-consumer × per-class routing policy (the librarian's push-model
  rule generalized): which callers' content may route off-GPU, off-host,
  off-LAN.

## Visibility: the status volume

Machine-level facts need machine-level publication, but cells must not
gain network paths for it.  Two designs were weighed:

- *State services query the agency over a network* — rejected.  Networks
  are not point-to-point: any net shared between the agency's consumers
  and a cell's state service puts the **agent** one hop from state,
  breaking the "agent cannot reach state" invariant.  Avoiding that
  needs a dedicated cross-stack net, client code in state, and polling
  anyway.
- *A shared status volume* — chosen.  The agency atomically renames a
  JSON snapshot into a well-known external volume (`agency_status`);
  every cell's state service mounts it **read-only** and renders an
  Inference panel.  One-way by mount modes, zero new network edges, and
  the well-known volume name is the whole "discovery" mechanism.

Snapshot contents: configured nodes and classes, per-node reachability
and resident models, queue depths by priority class, and the last N
completed operations — caller, logical class, routed target, duration,
token counts.  Timestamps from the agency's clock.

**No status on the consumer-facing port.**  Anything reachable by
consumers is readable by agents, and machine-wide operation metadata
would leak cross-cell activity into every cell.  Status flows only
through the volume, to state services, to the operator.

## Non-functional requirements

- **Containment**: the agency sees every prompt, so it holds no
  capability beyond routing — no workspace, no state write access, no
  tools.  It parses requests minimally (enough to extract class, consumer
  identity, and streaming framing).  Stdlib-only Go; fail-closed config
  (unknown keys are startup errors; every class names its full chain).
- **Latency**: pass-through overhead must be noise against decode time;
  streaming tokens are forwarded, never buffered whole.
- **Isolation**: consumers cannot reach nodes directly once the agency
  fronts them; compose-lint asserts `infer`'s net is agency-only, the
  agency holds no `egress`/`frontend`, and the status volume is `:ro`
  everywhere but the agency.
- **Fairness/starvation**: batch never starves forever — the two-class
  queue drains batch whenever interactive is idle, and queue depths are
  visible on the status volume before anyone wonders why a review is
  slow.

## Topology

```
shared inference stack:
  agency:                       # NEW — the sole inference door
    networks: [infernet, modelnet]
    volumes:  [agency_status:/status]         # rw: the snapshot writer
  infer:
    networks: [modelnet]        # was infernet: now reachable ONLY via the agency
  agency-proxy:                 # blind socat relay; replaces infer-proxy —
    networks: [infernet, frontend]   # 127.0.0.1:11434 now reaches the AGENCY
                                     # (host smoke tests); raw ollama has no
                                     # host port at all

cells (unchanged nets):  agent/scholar/librarian/corrector reach the
  agency over infernet; state mounts agency_status:ro for the panel.
deep-think node: dialed by the agency via env-provided address
  (infernet_big posture from the librarian design; no LAN IPs in-repo).
```

## Phasing

1. Pass-through v1 — **DONE** (internal/agency): agency fronts the local
   `infer` only; consumers repoint `OPENAI_BASE_URL`; `infer` moves to
   `modelnet`; the localhost relay flips to the agency (GETTING_STARTED's
   verification step changed with it — `v1/models` instead of `api/tags`).
   Behaviorally invisible, topology proven.
2. Engine classes + fail-closed config + the two-class queue — **DONE**
   (internal/agency router: the request's model field names a class,
   chains advance past unreachable or too-busy links, caller budgets ride
   the Agency-Deadline and Agency-Queue-Wait headers, per-node
   maxInFlight admission with interactive queued ahead of batch,
   etc/agency-routes.example.yaml is the config template; the deployed
   door stays in pass-through mode until the operator mounts a config).
3. The deep-think node: presence probes — **DONE** (internal/agency
   presence: every node probed via GET /v1/models on a configured
   interval; chains skip a node marked absent without a dial and pick it
   back up at the next probe after it returns; detection only, never
   wake-on-LAN).  Residency — **DONE** as pinned per-node model sets
   (never-evict by construction; probes assert the sets against /api/ps,
   log drift, and preload cold pinned models; nodes never idle-unload —
   OLLAMA_KEEP_ALIVE=-1); session affinity recorded as a non-goal (see
   Shape).
   The deep-think node itself is wired in at turn-on via its
   env-provided address.
4. The status volume + the state services' Inference panel.
5. Librarian/corrector land their inference through classes (their docs'
   engine-routing sections become agency config).
6. Frontier, if and when decided: relay, spend ledger, content-policy
   matrix — each a deliberate, reviewable step.

## Alternatives considered: adopt instead of build (2026-07-18 survey)

Before implementation we surveyed the open-source gateway ecosystem
(~35 projects, repo states verified on GitHub as of 2026-07-18) asking
whether an existing project could serve as the agency.  Answer: no.
Nothing implements the triad this design centers on — residency-aware
arbitration (route to loaded models, queue over evict), caller deadline
budgets whose queue-wait overrun advances the fallback chain, and
two-class priority queueing.  The field splits into three shapes, none
of them ours:

- **Cloud-API aggregators** (LiteLLM, Bifrost, Portkey, the one-api
  family): ordered fallback chains and served-by attribution exist, but
  the routing assumes stateless cloud backends — no concept of a model
  as a heavyweight resident whose mis-route costs an evict-and-reload.
- **Home-lab proxies** (Olla, llama-swap, llmlb, SOLLOL): residency
  signals and presence handling, but no class chains with terminal
  refusal, no caller deadlines, no request priority.
- **Datacenter routers** (llm-d and the Kubernetes
  gateway-api-inference-extension; Envoy AI Gateway's k8s inference
  extension): these genuinely do cache-aware placement, criticality
  classes, and flow control — as Kubernetes-native vLLM infrastructure.
  Right semantics, wrong scale for a three-node fleet.

Point-in-time disqualifications worth recording (they date quickly):
TensorZero archived June 2026; Kong gates fallback, priority balancing,
and circuit breakers behind its enterprise tier; Portkey is
mid-acquisition (Palo Alto Networks) with contradictory license
signals; Arch/Plano pivoted to semantic routing where an LLM — hosted
in their cloud by default — picks the route, the opposite of "never
silently substitute," with default egress on top.

**Near miss: Olla** (thushan/olla — Go, Apache-2.0, single binary, no
external deps, no telemetry).  It already has strict no-substitute
model routing, per-node attribution headers, health checks and circuit
breakers tuned for nodes that come and go, and KV-cache sticky
sessions.  It lacks exactly the scheduler contract: engine classes
mapping to chains of different (node, model) tiers, residency
arbitration, caller budgets, priority classes — and its status
endpoints share the consumer listener, which violates this design's
no-status-on-the-consumer-port invariant.  It is pre-1.0 with
effectively one maintainer.  Forking it would mean auditing and
carrying a whole third-party proxy — in the seat that sees every
prompt — to save writing the easy passthrough parts, while still
building the hard parts inside someone else's architecture.  Its
design is worth reading before implementing the router; its code is
not worth adopting as the sole inference door.

Confirmations picked up along the way: ollama through v0.32.x remains
strictly single-host (the three knobs above unchanged; `/api/ps` is
still the residency signal to poll), and llm-d's criticality and
flow-control machinery is independent prior art for
deadline-advances-chain at datacenter scale.  Several projects handle
a sleeping node by waking it over the LAN; our rule is the opposite —
the agency never wakes a node, so presence probing stays
detection-only.
