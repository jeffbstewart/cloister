# Cloister — Design

Why the system is shaped the way it is.  The README says what Cloister does;
this records the reasoning and the decisions that survived contact with
building it.

## The trust-boundary principle

"Contained" means enforcement at a boundary the model cannot reach, not a
request in a prompt.  Permission features that live inside an agent harness —
an LLM judging whether an action is risky, an "approve this step?" dialog —
are convenience, not security: they are an LLM gating an LLM.  Real
enforcement is deterministic and external: network topology the model cannot
route around, mounts it cannot write, validation that rejects rather than
sanitizes, and history it cannot rewrite.  Every design below is an
application of that one rule.

A corollary shows up at every trust boundary in the system: **narrow tool
contracts beat expressive protocols**.  The agent gets `research(query) →
answer`, not a conversation with the scholar; the scribe offers bounded edit
operations, not a shell; the manifest offers a fixed action menu, not command
strings.  Expressiveness across a trust boundary is attack surface.

## Lineage

Cloister grew out of an earlier, simpler containment problem: running local
inference (ollama on a workstation GPU) such that possibly-untrusted model
weights could not phone home.  The solution — an engine on an internal-only
Docker network with a tiny socat forwarder as the sole published port —
established the house pattern: *a single container cannot be both air-gapped
and reachable; split it, and give the bridging container no code that parses
anything*.  That two-container pattern recurs throughout the cell: the
status relay publishes the state service's pages the same way, and the
kagi-relay is the same idea run in reverse (sole egress instead of sole
ingress).

The step from "contained inference" to "contained agent" came when the
model gained tools.  A coding agent that can read, write, build, and browse
is not one risk but four, and Cloister's answer is to give each risk its own
container with exactly one authority.

## One authority per container

A cell is the agent plus single-purpose workers, split so that the container
running the riskiest code owns nothing it could use to cover its tracks:

- **builder** — executes manifest actions (agent-authored build code!) with
  no `/state` mount, no egress, and a workspace-plus-scratch blast radius.
  It streams logs, audit records, and status to the state service over a
  token-gated API; a hostile build can at worst inject bytes into its own,
  still-open log stream — which is its stdout anyway.
- **scribe** — the sole writer of workspace source.  The agent's mount is
  read-only, so every edit routes through the scribe's bounded, confined,
  audited operations.
- **state service** — the sole owner of `/state`.  Append-only history with
  per-run finalize, timestamps from its own clock (a stolen token cannot
  backdate the record), rate limits on every writer, and the operator's
  status pages.  This is the **one-way glass**: subsystems append, never
  read, and nothing that executes agent-influenced code can touch the
  record of what it did.
- **scholar** — the only path to the web, quarantined (below).
- **relays** — blind socat pipes that hold the boundary-crossing networks
  (host publish, internet egress) precisely because they parse nothing.

## The manifest contract

`agent-harness.yaml` at a project's root is the entire action menu.  Its
`run` arrays are exec argv — never a shell string, no interpolation; the only
agent-suppliable inputs are named params validated against server-anchored
RE2 patterns and appended as discrete argv elements.  Rejected, never
sanitized.

The manifest is repo-controlled and therefore eventually agent-writable.
That is acceptable by design: manifest commands execute inside the no-egress
builder cell, so a hostile manifest is exactly as dangerous as a hostile
`build.gradle.kts` — already contained.  The manifest is *data selecting
among jailed executions*, not a capability grant.  What the design adds is
legibility: the fully resolved argv of every run goes into the audit record,
so an action redefined to do something other than its name is visible on the
status page rather than hidden under an honest-looking tool name.

## The write path: bounded ops, not a shell

The scribe never shells out.  `sed` has `e` (execute), `w` (write any file),
and `r`; `awk` has `system()`; `patch(1)` rejects the sloppy-but-correct
diffs LLMs produce.  Handing any of them to the one component that holds the
workspace read-write would reintroduce an injection surface into the audited
write path.  Instead the scribe implements specific write operations in Go:
every path confined under the workspace root by a validating resolver
(symlinks and reparse points rejected outright, never followed), writes
atomic, sizes capped, RE2 only, and every mutation audited with its diff
stored for review.

Its `apply_diff` locates hunks by **content**, not line numbers — tolerant
of the diffs models actually produce — and refuses ambiguity rather than
guessing.

**Build-logic writes are gated.**  `agent-harness.yaml`, `*.gradle.kts`, the
wrapper, `gradle/`, `buildSrc/` — the set the build itself executes — is the
same list the dependency airlock enforces; a write there is held for human
approval (staged durably, applied only as reviewed, surviving restarts) or
refused outright when no approval channel is wired.  The gate is governance
and legibility, not a new containment layer: the network jail already bounds
the blast radius.  Binary writes and non-UTF-8 edits (not cleanly
reviewable) are always gated.

## The scholar: quarantining the web

The deepest risk of giving a coding agent internet access is **prompt
injection into the code-writing context** — attacker-influenceable web
content landing in the same model context that can call `apply_diff`.  The
scholar breaks that path structurally: the coding agent never touches the
web, and the scholar cannot touch code (no workspace, no scribe, no builder,
no shell).

- **Tool, not agent.**  The scholar is an agent posing as an MCP tool, and
  that narrowness is the security decision: the caller cannot open a
  dialogue with it, steer it mid-loop, or address its sub-steps.  MCP
  sampling was rejected because it routes the quarantined loop through the
  privileged caller's harness; A2A because dialogue across this boundary is
  precisely what must not exist.
- **A bespoke Go loop, not a wrapped harness.**  Wrapping a headless
  qwen-code would put an entire Node runtime, its built-in tools, and its
  settings surface inside the quarantine and then strip capability back out
  by configuration — a denylist posture in an allowlist-by-construction
  cell, one config regression away from re-arming.  The loop is instead a
  small fixed Go function offering exactly three tools (`web_search`,
  `extract_url_as_markdown`, `respond`) dispatched to in-process functions:
  "the scholar physically has no other tools" is true by construction.
- **The egress pin.**  The scholar holds no egress network.  Its only route
  out is the kagi-relay, a socat pipe hard-wired to `kagi.com:443` (TLS
  end-to-end; the relay sees ciphertext).  Searches and page fetches are
  Kagi server-side operations, so the cell never dials an arbitrary host at
  all.  Three layers keep this true over time: `compose-lint` fails CI if
  the committed topology drifts; the scholar's boot self-check probes fixed
  public addresses and refuses to start if any connects (negative-only —
  liveness is deliberately not a start gate); and a runtime probe script
  re-verifies a deployed cell.
- **Two human gates, question-sized.**  The operator approves the *query*
  (the entire exfiltration budget, read whole) before any spend, and the
  *answer* (the one artifact crossing back into the code-writing context)
  before the agent sees it.  Approving questions instead of URLs is an
  order-of-magnitude improvement in approval fatigue.  Raw model-constructed
  URLs additionally require per-retrieval approval; search-result pages are
  read via opaque session handles the model cannot mutate into an
  exfiltration channel.
- **Grounding is structural.**  An answer is accepted only after a search
  actually retrieved something; a model that answers from its weights is
  nudged, then refused.  Never prompt-discipline alone.
- **No memory, no content at rest.**  Each query runs in a fresh context
  with a fresh handle map; the audit stores URLs and counts, never page
  bodies or snippets; the transcript is URLs-only and size-capped.  Caps
  (searches, extracts, turns, tokens, wall clock, per-day budgets) are
  enforced against a restart-surviving burn ledger of bare timestamps, so a
  crash loop cannot reset the meter.  The deny list (paste sites, request
  catchers, tunnels, SERPs) is defense-in-depth and documented as such —
  Kagi follows redirects the cell cannot re-check; the real gate is the
  raw-URL approval.

## The dependency airlock

Builds run offline.  Dependency refresh is a deliberate human act that
briefly grants the builder egress — and because the build tool executes
project build logic during configuration, egress plus unreviewed build
scripts is the one dangerous combination.  The airlock therefore refuses to
open if build-logic files have uncommitted changes, runs a platform-provided
resolution script (baked into the image, not agent-writable) that downloads
every resolvable configuration without executing any test code, and always
disconnects afterward.

## Key decisions

| Decision | Choice | Why |
|---|---|---|
| Implementation language | Go, stdlib-first | Static ~10 MB binary `COPY`s into any toolchain image; stdlib covers HTTP, exec with process-group kill, RE2, JSON.  Kotlin taxed every image with a JVM; Rust needed more third-party code than Go. |
| Dependency policy | stdlib + yaml.v3 + official MCP SDK (+ its schema type) | Supply-chain surface is a security property; anything more needs written justification. |
| One binary, many workers | `-worker-mode` enum, no default | Every worker ships in one image; a cell's compose file must say what each container is, and incompatible mode flags cannot be combined. |
| Agent↔worker protocol | MCP (agents posing as tools) | The caller can't distinguish an agent-backed tool from any other — zero new capability on the privileged side. |
| Run identifiers | UUIDv7 in a struct wrapping a private string | Time-sortable, meaningless, shell/path-safe alphabet; unforgeable outside the package — a raw string can never be coerced into an ID. |
| Config posture | Fail closed, everywhere | The scholar policy requires every field (no "0 = unlimited" footgun); unknown YAML keys are startup errors; a missing approval channel means refusal, not bypass. |
| Time | UTC, writer's clock | Caps roll at UTC midnight (no DST edge cases); audit and approval timestamps are stamped by the receiving authority so callers cannot backdate. |
| On-disk ledgers | Bare epoch-second lines, sorted on load | Second resolution suffices for daily caps; no format ceremony; never trust on-disk order. |
| Higher-level concepts | Real types, never primitives | `time.Time`/`time.Duration` in memory with explicit wire codecs; named string enums (decisions, statuses, modes) over bools and bare strings. |

## Honest limits

- Prompt injection through the scholar is **attenuated, not eliminated**: a
  payload must survive the scholar's synthesis, fit the answer cap, pass the
  human answer gate, and still steer the agent.  The layering is the defense;
  none of the layers alone is.
- The SERP/paste-site deny list can be slipped by a redirect Kagi follows
  server-side; it is explicitly defense-in-depth, not the gate.
- The Brave search path has never been exercised against the real API (the
  code says so at its entry point).
- The agent image's read-only workspace mount does not enforce `.aiignore`;
  interposing a reader that respects it is future work.
