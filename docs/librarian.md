# The librarian — reader-mode design

Status: **phases 0–4 implemented** (the spike, internal/shield,
internal/repo, internal/watch, the worker + mechanical tools, and the
cell cutover — the agent holds no workspace mount).  **Phase 5 — the
comprehension ops and engine-routed inference — is planned in detail below
and now in execution** (sub-phases 5a–5d); its inference routing folds into
the agency's engine classes (docs/agency.md).  This file records the
2026-07-07 design review plus the 2026-07-08 execution decisions.

## Problem

The agent holds a read-only mount of the workspace.  That mount is the last
place where the agent touches project bytes without mediation: it does not
enforce `.aiignore` (DESIGN.md lists this under Honest limits), it produces
no record of what the agent looked at, and it drags in the entire unix
file-navigation idiom — the agent spends context and turns chaining `cat`,
`grep`, `head`, `wc`, and `find` through its built-in tools.

## Shape

A new worker mode, the **librarian**, becomes the entire read side of the
cell, the same way the scribe is the entire write side:

- **scribe** — sole writer.  Unchanged; gains no read tools.
- **librarian** — sole reader.  Serves mechanical read tools AND
  inference-backed comprehension tools from one in-memory model of the
  agent-visible workspace.
- **agent** — holds **no workspace mount at all**.  Hard cutover: the `:ro`
  mount is removed, qwen-code's built-in filesystem tools are disabled in
  the same change, and a tmpfs stub provides the working directory.

Reads and writes split cleanly because their authorities differ: the scribe
must hold the workspace read-write and must never talk to a model; the
librarian holds it read-only and must talk to the inference engine.  Neither
capability set is a superset of the other, so combining them in one
container would give a single process both the model and the pen.

The smart ops live with the mechanical reads — not in a separate worker —
because they consume the same RAM model and the same visibility filter.
Splitting them would mean a second workspace authority enforcing the same
shield, and drift between two enforcers is exactly the class of bug the
one-authority rule exists to prevent.

## The in-memory model

The librarian's working assertion: **everything the agent is allowed to
read fits in RAM with ease.**  At boot it loads the visible tree into
memory and serves every operation from there — pure Go, stdlib only, zero
subprocesses.  A CI test walks the librarian packages' import graph and
fails on `os/exec` or any non-stdlib dependency; "the librarian cannot
shell out" is true by construction, like the scholar's three-tool menu.

Coherence with the three external writers (scribe edits, builder outputs,
the operator editing on the host).  The seam to close is read-your-writes:
the agent edits file A via the scribe, then reads or greps via the
librarian, and must see its own write.

- **SPIKE RESULT (2026-07-07, Docker Desktop on Windows, bind mount,
  stdlib `syscall` inotify):** a watching container sees **other
  containers' writes with full fidelity** — creates, modifies,
  close-writes, and the scribe's atomic-rename lands as
  `MOVED_FROM`/`MOVED_TO` — but **host edits generate no events at all**
  (content and mtimes propagate; the notification does not).
- **Therefore: watcher-primary for container writers, rescan for the
  host.**  The inotify watcher (stdlib `syscall`, not the third-party
  fsnotify package) covers the read-your-writes seam completely, since
  the scribe and builder are containers.  Host edits — the one silent
  writer — are bounded by the once-a-minute background metadata rescan,
  which therefore does NOT relax to hourly (that option assumed the
  watcher covered all three writers).  Single-file ops keep a cheap
  `stat`-on-access revalidation as insurance.

**Memory cap, fail loud.**  The resident set has a configured byte budget.
Over budget at boot: refuse to start, naming the largest offenders — the
signal the operator uses to tune `.aiignore`/`.gitignore` exemptions.  Over
budget mid-session (the workspace grew): deny loading the newly seen files
with an error naming the cap and surface a status-page warning; a live
session is never killed for growth it did not cause.

## Visibility and the shield

Two content states only: **allowed** and **forbidden**.  No gated reads for
now; the write side already has approval gates, and read gating can be
revisited later.

- **`.aiignore` — "part of the project, but shielded."**  Matched files
  appear in listings by name with their read and execute permission bits
  stripped (the listing API surfaces POSIX `rwx` bits, so the agent can see
  that a file exists and that it may not read it).  Any operation that
  explicitly names a jailed path is rejected immediately and audited.
  Tree-wide operations skip jailed files silently — a grep simply does not
  search them, which is not a denial.
- **`.gitignore` — "not part of the project."**  Build outputs, caches,
  generated trees.  These are invisible: absent from listings, trees,
  globs, and searches.  An explicit read still denies.  The working theory
  is that "don't version it" implies "the agent needn't read it"; if a
  workflow proves to need build-output inspection, that becomes a
  tightly-scoped additional tool, not a loosened shield.
- Build outputs must be covered by the union of the two files, and the RAM
  cap enforces honesty: an uncovered output tree blows the budget at boot.
- **`.aiignore` itself is always readable** — the agent may know its own
  constraints — and its edits join the gated-write sensitive set (held for
  human approval, like build logic).  Without the gate the agent could
  unshield itself.
- Shield changes apply **immediately and silently** at rescan.  Content
  already in the agent's context is water under the bridge.
- The shield is one shared package (`internal/shield`: parse both ignore
  files, answer "may read / may list / must gate").  The librarian applies
  it when serving; the **scribe applies the same package** on the write
  path, because an edit is a covert read — `apply_diff` and `dryRun`
  results echo file content, so mutating a jailed file is refused.  Any
  future multi-file write op skips sensitive files by default and gates
  for approval to include them.

## Audit: denials only

Successful reads are not audited — the volume would change the ledger's
character, and the interesting event is refusal.  A new typed audit detail
records each denial: the denied path or paths (batch ops may deny several)
and the tool that was invoked.  Tool shape only; arguments are not
recorded.

## Tool surface

Bounded ops, not a shell — the read-side mirror of the scribe's write
principle.  Every result is size-capped and paginated; every path is
confined by the same validating resolver the scribe uses.

Mechanical:

| Tool | Replaces | Notes |
|---|---|---|
| `read_file` | `cat` | whole file, caps enforced |
| `read_lines` | `head` / `tail` / `sed -n 'A,Bp'` | ranged reads |
| `stat_file` | `ls -l`, `wc -l`, `sha256sum`, `file` | size, mtime, line count, hash, permission bits |
| `list_dir`, `tree` | `ls`, `find -maxdepth` | permission bits surfaced; depth-limited |
| `glob` | `find -name` | name patterns across the tree |
| `search` | `grep` family | RE2; four modes: matches-with-context, files-with-matches, count-per-file, total count; glob/path filters |
| `batch_read` | `cat a b c` | several files, one call |
| `extract_section` | `sed -n '/a/,/b/p'` | pattern-to-pattern — "read this function" without line numbers |
| `recently_modified` | `ls -t`, `find -newer` | tree by mtime |
| `diff_files` | `diff a b` | two workspace files |
| `count_grouped` | `grep -o \| sort \| uniq -c` | match-frequency table |
| `validate_text` | — | encoding/UTF-8 report, pairing with the scribe's UTF-8 gates |

Inference-backed (same worker, same model, same shield; each takes an
optional `effort` — see "Effort, cost, and the comprehension ops" below):

- `summarize_file(path, effort?)`, `summarize_directory(path, effort?)` —
  map-reduce over resident content; context-saving alternatives to reading
  whole trees.  Default `quick`.
- `ask_about_file(path, question, effort?)` — one-shot Q&A grounded in the
  file's bytes.  Default `quick`.
- `find_relevant_files(question, scope?, effort?)` — semantic locate ("where
  is retry handled?"); internal retrieve-then-rank loop, see below.
- `explain_change(opId)` — possible later: narrate a stored scribe diff.

The list is a starting catalog, not a contract; the review gate for adding
a tool is that it replaces a unix chain the agent demonstrably wants, at a
higher level of abstraction.

## Inference routing

**Content is pushed, never pulled.**  The inference engines are stateless
completion endpoints: the librarian assembles each query, embedding the
shield-filtered file content it chooses to include, and the model answers
from exactly that.  No engine — local or remote — holds a workspace mount,
a credential, or any way to fetch bytes itself, so the shield is enforced
once, in the librarian, before content leaves the process.

The librarian drives the **existing** infer engine and model — no model
swap.  The client is engine-routed from day one: a named-engine
configuration maps op classes to endpoints, rather than a single hardwired
URL.  Planned second engine: `infernet_big`, an external network to a
LAN "deep think" node (a MacBook), for the heavier comprehension ops.  Its
address arrives via stack env only (the no-LAN-IPs presubmit invariant),
compose-lint must bless exactly which workers may hold that network, and
ops routed to it degrade gracefully when the node is absent — a laptop is
a sometimes-there machine.  Because the push model means allowed workspace
content transits the LAN to that node, which op classes may be routed
off-host is part of the engine configuration, not an implementation
detail.

## Effort, cost, and the comprehension ops (Phase 5, decided 2026-07-08)

**One caller knob: `effort`.**  The comprehension ops take a single named
enum, `effort: quick | thorough` (extensible; two values to start).  It is
INTENT, not a model name — the agent never names a model.  The librarian
maps `quick → think-fast` and `thorough → deep-think` (the agency's engine
classes); the agency resolves the class to a concrete model via its
presence-aware fallback chain.  So when a node sleeps, a model is retired, or
the roster changes, the agent's tool calls do not change — the mapping lives
in librarian config, the roster in the agency.

**Why one knob and not two.**  "Fast, little context" and "effortful,
chain-of-thought" feel like one axis but decompose into two independent
costs:

- *Time / compute* — which model, CoT on or off.  Paid by the ENGINE.
  Chosen by `effort`.
- *Context* — tokens returned into the agent.  Paid by the CALLER.  Kept
  small by the firewall for BOTH efforts.

The librarian **returns the final answer only and strips the reasoning
trace**, so a `thorough` answer costs the agent the same handful of tokens as
a `quick` one — the CoT is spent engine-side and discarded.  "Little context"
is therefore the default for both; the only thing `effort` buys is
engine-side depth.  Reasoning/CoT configuration belongs in the engine-class
definition, not the agent's knob: `deep-think` = a reasoning model with
thinking on, `think-fast` = a small model with it off.  Even before the
agency and the deep-think node exist, `effort` is meaningful — the client
points both classes at the existing engine and toggles the model's thinking
mode.

**No deadline knob yet.**  Deadlines are effort-derived (config profiles),
enforced with `context.WithTimeout` — not an agent-facing parameter (the
agent should not pick milliseconds).  Trivially added later if a caller needs
to override.

**Cost + provenance come back in the response.**  Every comprehension result
carries a compact trailer separating the answer (context the agent pays for)
from the provenance (cheap, ~15 tokens):

    {the distilled answer}

    — librarian · thorough → deep-think(model@node) · 11.2s · 5,912 tok

- The footer is model-visible on purpose: the tokens are ENGINE-side work, so
  the quick-vs-thorough delta is visible and the agent can decide when to
  escalate effort.
- Source of truth is the text footer; also emit it as MCP structuredContent
  for programmatic use, but do not depend on the client surfacing that.
- Map-reduce ops aggregate tokens + wall-clock across sub-calls; if fallback
  made it mixed-engine, name both.  Never a silent substitution — the
  response always says which engine served (agency invariant).
- The operator gets this independently from the agency's status volume
  (last-N ops); the footer is the agent's copy.

**`find_relevant_files` — the retrieve-then-rank loop lives INSIDE the
librarian.**  A metadata-first, selectively-investigate design is right for a
big tree (MediaManager-scale), but it does NOT need an agent↔librarian
protocol — exposing the stages would burn the agent's context on candidate
lists.  The librarian holds the RAM tree, the shield, and the model, so it
runs the loop internally and returns only a ranked result:

1. Cheap candidate generation, NO heavy model: one tiny `quick` call expands
   the question into keywords/synonyms; the librarian greps the resident tree
   for them (grep over RAM is fast even on a large tree) and filters by
   metadata (path, size, mtime).
2. Bounded rerank: feed only the top-N candidates' snippets to the model to
   rank and give a one-line "why."  Cap N; report if truncated.
3. Return ranked paths + reasons.  Optional `scope` (path prefix / glob) lets
   the agent steer by calling again narrower — no stateful protocol.

The genuinely hard part is *true* semantic recall (a file that says
"backoff/attempt" when the question said "retry").  Closing that needs an
embedding index over the tree (resident vectors, cosine top-k, rebuilt on the
rescan cycle) and an embeddings engine lane — **deferred to Phase 6**.  v1
ships keyword-expansion + grep + rerank, which handles a big tree without it.

**Big-tree guard on `summarize_directory`.**  A map-reduce over thousands of
files is thousands of engine calls.  Over a file-count / total-size threshold
the op refuses and asks for a narrower scope, rather than silently launching
(or silently truncating) the fan-out — the same "no silent caps, say what you
dropped" rule as the mechanical tools.

**No off-host content audit.**  When `thorough` pushes allowed file content
over the LAN to the deep-think node, that transit is NOT audited: it is the
operator's own workspace content going to the operator's own machine on the
house network — far lower risk than the scholar, which quarantines untrusted
WEB bytes.  Read denials remain the only audited read event.

## Topology

```
librarian:
  volumes:  ${WORKSPACE}:/workspace:ro     # sole reader; the agent has NO mount
  networks:
    - buildnet    # reachable by the agent (inbound MCP, :9400)
    - infernet    # reach infer for the comprehension ops
    - statenet    # denial audits to state; internal -> no egress
```

- The agent's service loses its workspace volume; `working_dir` points at a
  tmpfs stub.  `qwen-mcp-init.mjs` registers `librarian` at
  `http://librarian:9400/mcp` and the coreTools allowlist drops the
  built-in read tools alongside the already-excluded mutators and web
  tools.
- compose-lint grows invariants: the agent mounts no workspace; the
  librarian's mount is `:ro`; the librarian holds no `egress`, `frontend`,
  or `kagiegress` network.
- The scholar remains workspace-free; the librarian remains web-free.  The
  prompt-injection quarantine is untouched: web content and workspace
  content still never share a mediator.

## Phasing

0. Spike: the inotify propagation experiment on the real mount type —
   scribe write, builder write, host edit, each observed (or not) from a
   watching container.  Decides watcher-primary vs revalidation-primary.
1. `internal/shield` — ignore-file parsing and the may-read/may-list/must-
   gate decisions, with the CI import-graph assertion.
2. `internal/repo` (name TBD) — the RAM model: load, revalidate, rescan,
   cap enforcement.
3. Librarian worker mode + mechanical tools; scribe adopts `internal/shield`
   on the write path; denial audit detail.
4. Compose + compose-lint + docs; the agent cutover (mount out, qwen config).
5. Comprehension ops + engine-routed inference client.  Sub-phased:
   - **5a(i)** — extract the reusable OpenAI-compatible chat-completions
     client (the wire types + the single-endpoint `Complete` HTTP plumbing,
     today in internal/scholar/model.go) into a shared, **stdlib-only**,
     worker-agnostic package; refactor the scholar onto it with no behavior
     change.  Stdlib-only is load-bearing: the librarian graph imports it, so
     it must stay inside the internal/shield deps assertion.  Its own PR.
   - **5a(ii)** — `internal/infer` (name TBD): the engine-routed layer ON TOP
     of the shared client — `effort → class → endpoint`, CoT-strip, an
     `{answer, servedBy, elapsed, tokens}` return, context-deadline bound (no
     client timeout, mirroring the scholar).  Config + fakes + tests,
     library-first; no ops, no librarian wiring yet.
   - **5b** — `ask_about_file` + `summarize_file` (single-file) wired into the
     librarian, the `effort` schema, and the provenance footer; add the new
     packages to the internal/shield deps_test stdlib-only roots.
   - **5c** — `summarize_directory` (map-reduce + big-tree budget guard) and
     `find_relevant_files` (internal keyword-expand → grep → bounded rerank,
     optional `scope`).
   - **5d** — compose + compose-lint (confirm `infernet` reaches an
     OpenAI-compatible endpoint — the agency when it exists, else the current
     engine), docs flipped design→shipped, effort-default tuning.
6. Later: `infernet_big` + the deep-think node behind the agency;
   **embedding-based semantic recall for `find_relevant_files`** (its own
   engine lane + index lifecycle); gated reads if ever needed; build-output
   inspection tools if a workflow demands them.
