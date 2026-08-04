# Jailed Claude

*Status: **design only — nothing built.**  Decisions from the 2026-08-04
investigation.  This is a deliberate, scoped relaxation of the cell's
network containment, taken to answer a question the current topology
cannot answer.  Read [What this costs](#what-this-costs) before building
any of it.*

## Why

Two motivations, one diagnostic and one operational.

**Diagnostic.**  The qwen agent overruns its context window and degrades
in ways that are hard to attribute: it emits Chinese glyphs, stops
honoring the system prompt, and mishandles the archivist's MCP surface.
We cannot currently tell whether that is a bug in the archivist's
surface, a limitation of the qwen-code harness, or a limitation of the
model.  Every fix we attempt is a guess.  Putting a known-good harness
and a known-capable model behind the *same* MCP surface collapses the
ambiguity: if Claude Code drives the archivist cleanly, the surface is
fine and the problem is upstream of it.  This changes both harness and
model at once, which is accepted — the follow-up (qwen-code harness
against a Claude model, if Anthropic's terms permit it) separates them.
Claude harness plus Claude model is the *blessed* configuration, and it
is the one worth testing the tooling against first.

**Operational.**  The real prize is overnight autonomy.  The cell's
supervision model — a jail that is a containment boundary rather than a
consent dialog — only pays off with an agent capable enough to be worth
turning loose on a complex, long-running task without a human in the
loop.  With that agent, `--dangerously-skip-permissions` inside the jail
stops being reckless and starts being the point: the harness's own
consent gate is redundant when the jail is the boundary, and the PR gate
is still the only path to `main`.  Today that posture is not available
on the dev server at any price, because the only way to run a frontier
model over this tree is to run it on the host, unjailed, with the
operator's whole home directory in reach.  The trade this design makes
is: give up *network* containment, keep *filesystem*, *credential*, and
*version-control* containment, and gain the ability to leave a capable
agent running unattended.

## What this costs

### The credential risk, first

This setup puts a bearer credential for the operator's entire Anthropic
subscription within reach of a jailed, model-driven process, and then
decrypts the traffic that carries it.  Two distinct exposures:

1. **A stolen token.**  A one-year `setup-token` OAuth token, or a
   Console API key, is a full-value credential.  If it sits in the cell
   as an environment variable, the model can read it, and any capture
   of the decrypted stream contains it in the clear.
2. **A leaked capture.**  A pcap of decrypted traffic is, by
   construction, a plaintext dump of everything the harness sends —
   including the `Authorization` header on every request.  A capture
   file that lands in the repo tree, or in a bug report, or in a
   pasted-in trace, is a credential disclosure.  The presubmit secret
   scanner is the last line of defense here and should never have to
   be.

**The design's answer is to make the capture credential-free by
construction, not by redaction discipline.**  The cell holds only a
placeholder token; the real credential is injected at the last hop, in
the abbey, downstream of every capture point.  See
[Credential handling](#credential-handling).

This does not make the credential unusable by the agent — anything the
agent sends through the door comes back authenticated, so the model can
still burn quota and issue arbitrary Anthropic API calls.  Injection
converts a *stealable secret* into an *ambient capability bounded by the
proxy*.  That is a real improvement, and it is not the same as safety.
Assume the credential will need rotating; see
[Rotating the credential](#rotating-the-credential) and rehearse it
before the first overnight run.

### The invariants this breaks

Measured against the security invariants in [CLAUDE.md](../CLAUDE.md):

| Invariant | Under jailed-claude |
|---|---|
| All inference rides through the agency (the sole inference door) | **Broken.**  A second inference door exists.  compose-lint's rule that every consumer dials `agency:11434` needs an explicit, variant-scoped exception — and once there are two doors, "sole" is a claim the topology no longer makes. |
| The cell holds no route to the internet | **Broken by design.**  The agent gains a mediated, single-host, inspected route out.  It is narrower than a host-run Claude Code by a wide margin, and it is not zero. |
| Research answers grounded in retrieved results, structurally | **Broken.**  Claude Code's `WebSearch` executes server-side on Anthropic's infrastructure; results arrive inside the model response.  No relay, proxy, or capture can see the fetch, and the scholar cannot mediate it.  `permissions.deny` on `WebSearch` is the mitigation, and it is a *prompt/config-level* rule — precisely the kind of control CLAUDE.md says an invariant must not rest on. |
| Source confidentiality (implicit) | **Broken by design.**  The repo's contents leave the machine and go to Anthropic.  Under a Pro/Max consumer account that carries a 5-year retention window if the training toggle is on.  Acceptable for this repo, which is public; a per-cell decision for anything that is not. |
| The scholar holds no `egress` network | Intact — but the abbey gains a **fourth** internet holder (`claude-egress`), and the "three internet holders" comment in `docker/abbey.yaml` becomes wrong. |
| Builds run offline; no package-registry route | Intact.  Claude Code is installed at image build; `DISABLE_AUTOUPDATER=1` keeps it from reaching for one at runtime. |
| Agent bytes reach the canonical tree only via a human-reviewed PR | Intact.  The archivist, the forge ruleset, and the bot-credential split are untouched.  This is the guarantee the whole exercise is meant to *test*, not to relax. |
| The operator's host tree never enters a cell | Intact.  The grange remains the only workspace. |
| The audit trail is one-way glass | Intact, and arguably strengthened: the proxy capture is a second append-only record.  But note the Claude session's own reasoning and tool calls are not in the state service, exactly as qwen's are not. |
| No secrets or home/LAN IPs in the repo | **At risk.**  Captures must live on a volume that is not the tree, and `**/*.pcap*`, `**/*.keylog`, and the capture volume path belong in `.gitignore` before the first capture is taken. |

### Two risks with no invariant to name

- **The proxy is trusted code in the agent's decision path.**  A proxy
  that can rewrite responses can inject arbitrary model output, which
  is arbitrary agent behavior.  The mutation addon and the CA private
  key join the archivist's bot token in the small set of things whose
  compromise compromises everything.
- **Session memory outlives the grange.**  `~/.claude` on the per-project
  home volume persists across `dispose`, so transcripts and auto-memory
  become a persistence channel that survives the workspace destruction
  the design turns on.  Decide deliberately: point `autoMemoryDirectory`
  *into the grange* so memory dies with the task, or at home so it
  doesn't.  Do not let the default decide.

## Topology

The door lives in the **abbey**, not the cell — same reasoning as the
agency and the git relays.  One per machine, shared by every cell, and
the credential stays on the abbey side of the boundary.

```
CELL                                   ABBEY
                                                        ┌── claude-tap
  agent ──claudenet──▶ claude-proxy ──claudeplain──▶ claude-egress ──egress──▶ api.anthropic.com
    │  TLS, our CA        │  terminate,      plain HTTP  │  inject the real
    │  SNI/alias:         │  inspect,        (placeholder │  credential, originate
    │  api.anthropic.com  │  mutate           token only) │  TLS, verify the real cert
    │                     │                               │
    └ dns: 127.0.0.1      └ mitmproxy addons              └ nginx
```

Four properties fall out of this shape, and each one is why a hop exists:

1. **The agent believes it is talking to `api.anthropic.com`.**
   `claude-proxy` carries that name as a docker network alias on
   `claudenet`, so the agent's dead-DNS pin (`dns: 127.0.0.1`) still
   resolves it authoritatively through the embedded resolver — the same
   trick `github-relay` uses.  No `ANTHROPIC_BASE_URL` redirection.
   This matters for fidelity: we are testing the blessed configuration,
   and it also means the calls that *ignore* the base URL — the
   fast-mode availability check, the WebFetch domain-safety preflight —
   are captured too, instead of silently failing.  Every other Anthropic
   hostname simply fails to resolve, which is the observation we want:
   the proxy log tells us what the harness tried to reach.
2. **The middle hop is plaintext, and that is the capture point.**
   This is the one structural concession the packet-trace requirement
   forces.  A single TLS-terminating proxy that re-originates TLS
   directly to the internet gives you flow logs, not packets;
   interposing a cleartext segment makes `tcpdump -w` produce a pcapng
   that Wireshark dissects as ordinary HTTP with no keys, no
   configuration, and no decryption step.
3. **The credential is injected downstream of the capture.**  Because
   injection happens in `claude-egress`, the plaintext hop — the thing
   we record — carries only the placeholder.  The capture is
   credential-free because of where the hops sit, not because someone
   remembered to redact.
4. **Real TLS validation still happens, at the last hop.**
   `claude-egress` verifies `api.anthropic.com`'s real certificate chain
   against the public roots.  We MITM our own agent; we do not MITM
   Anthropic.

`claude-tap` is a capture sidecar sharing `claude-proxy`'s network
namespace (`network_mode: "service:claude-proxy"`, `cap_add: [NET_RAW]`).
The namespace-sharing is load-bearing: docker networks are switched
bridges, so a sniffer container merely *attached* to `claudeplain` sees
none of the unicast traffic between the two hops.  It has to be inside
one of the endpoints' namespaces.  (Operator quick path, outside the
compose model: `tcpdump -i br-<id>` on the host against the bridge.)

## What the proxy enforces

The proxy is not just an observation point.  Three controls live there,
and the first two are load-bearing — without them the door is wider than
the topology diagram suggests.

### The path allowlist — required, not optional

**`api.anthropic.com` is not only an inference endpoint.  It is also a
storage service.**  The Files API (`/v1/files`) and the Batches API live
on that host.  An agent that can send authenticated requests through the
door can upload the entire grange and retrieve it later, from outside,
with the same account — while every other control in this design remains
intact.  A host pin is not containment when the host will hold your
bytes for you.

So the proxy allowlists **paths**, not just the host: `/v1/messages` and
nothing else.  Everything else gets a local 403 and a log line naming
what was attempted.  This is a few lines of mitmproxy addon and it is
the difference between "one pinned host" meaning something and meaning
very little.

The credential is the second layer, and the two interact.  A
`setup-token` OAuth credential "can only make model requests" and so
cannot reach those endpoints even if the allowlist fails; a Console API
key has the **full** API surface and depends entirely on the allowlist
holding.  Neither substitutes for the other — the allowlist is
mandatory regardless, and the credential choice decides whether there is
anything behind it.  See [Which credential](#which-credential).

### The spend cap

Every response carries exact token accounting, so the tally needs no
estimation — parse the SSE frames already being captured and accumulate
`usage`:

| Field | Note |
|---|---|
| `input_tokens` | **The uncached remainder only** |
| `cache_creation_input_tokens` | Bills at 1.25× (5-min TTL) or 2× (1-hour) |
| `cache_read_input_tokens` | Bills at ~0.1× |
| `output_tokens` | Cumulative on `message_delta` in a stream |

Total prompt size is the sum of the three input buckets.  Count them
separately: a tally on `input_tokens` alone badly *under*counts a
cache-heavy agentic loop, and summing all four at one rate badly
*over*counts cost.  Input counts arrive on `message_start`, cumulative
output on `message_delta`.

The API returns tokens, not dollars — the price table is ours to carry
locally.  On a subscription credential there is no dollar meter at all
and the binding constraint is Anthropic's own rate limits, so tokens is
the right unit either way.  Responses also carry rate-limit headers
(limit/remaining pairs, plus `retry-after` on a 429), which are
authoritative from Anthropic's side rather than reconstructed from our
arithmetic — cheaper than tallying, and worth reading first.  Confirm
the exact header names against a live response rather than from
documentation; the capture shows them in cleartext on the first request.

Enforcement: once the tally crosses the budget, the addon stops
forwarding and synthesizes a 429 locally.  Claude Code already backs off
on 429, so a jailed agent degrades into waiting rather than crashing.
Granularity is per-request — output tokens are only known when a
response completes, so the cap refuses request N+1 rather than cutting
off mid-generation.  That is the right shape for an overnight budget.

**Why the proxy and not the cell.**  Claude Code exports its own usage
metrics over OpenTelemetry, which is easier and requires none of this.
But that telemetry runs inside a container the model controls, so it is
*advisory*.  Proxy-side counting is the trustworthy vantage point — the
same reasoning that put the credential at the door.

### Mutation

The `response` hook rewrites responses for fault injection: truncated
bodies, malformed tool calls, injected 429s and 529s, delayed frames.
The point is to exercise the archivist surface's error paths, which is
otherwise hard to do on demand.  Note that mutating a *streamed* SSE
response requires handling it at the `responseheaders` hook and
disabling streaming for that flow, or the body is gone by the time
`response` fires.

The request hook is worth a look too: `output_config.task_budget`
injects a token countdown the model can *see* while generating, so it
paces itself and finishes gracefully instead of being guillotined by the
spend cap.  A softer control than a hard cutoff, and a good second
experiment once observe-only works.

## Package nominations

Nothing here should be written from scratch.

### The inspecting/mutating proxy

| Package | License | Verdict |
|---|---|---|
| **mitmproxy** | MIT | **Recommended.**  Reverse mode terminates TLS for a named upstream; Python addons give `request`/`response` hooks for mutation in a few lines; `mitmdump` is headless, `mitmweb` is a live UI for interactive work; `--certs` takes our own CA; flow files and HAR export complement the pcap.  Official image, HTTP/2 support, and the only candidate whose *primary purpose* is exactly this job. |
| Envoy | Apache-2.0 | Strong alternate if this ever becomes a permanent fixture.  The HTTP tap filter dumps full request/response bodies, `ext_proc`/Lua handle mutation, and it is production-grade.  Much heavier configuration for a diagnostic. |
| OWASP ZAP | Apache-2.0 | Best-in-class *interactive* request/response tampering with a REST API and headless mode.  JVM footprint, and awkward as a long-lived compose service. |
| Caddy | Apache-2.0 | Its built-in internal CA is genuinely attractive (it would mint and rotate our CA for us), but `reverse_proxy` has no real response-mutation story.  Consider it for the CA alone. |
| Squid + `ssl_bump` | GPL-2.0 | Not recommended.  The classic answer, painful to configure, weak at mutation. |

### The egress leg

| Package | License | Verdict |
|---|---|---|
| **nginx** | BSD-2 | **Recommended.**  Collapses both jobs into one well-understood component: `proxy_pass https://api.anthropic.com` with `proxy_ssl_server_name on`, `proxy_ssl_verify on`, `proxy_ssl_name api.anthropic.com`, plus `proxy_set_header Authorization "Bearer ..."` for injection.  The official image's `/etc/nginx/templates/*.template` + `envsubst` mechanism reads the token from a mounted file at start, so the secret never needs to be baked in.  **Three settings are not optional here:** `proxy_buffering off` (or SSE streaming breaks and the agent appears to hang until each full response lands), a `proxy_read_timeout` far above the 60s default (a long generation takes minutes — the archivist already needed a 90-minute MCP timeout for the same class of reason), and stripping `Accept-Encoding` so the plaintext hop carries readable bodies rather than compressed ones. |
| ghostunnel | Apache-2.0 | Clean Go TLS client wrapper, static binary, house language — but no header injection, so it needs a separate injector hop.  Prefer it only if we decide injection belongs elsewhere. |
| stunnel | GPL-2.0 + OpenSSL exception | Functionally equivalent to ghostunnel here.  Copyleft, though `socat` (GPL-2.0) is already house precedent in the existing relays. |

### Capture and CA

| Package | License | Role |
|---|---|---|
| **tcpdump / libpcap** | BSD | The capture sidecar.  `-w` writes pcapng straight to the captures volume. |
| tshark | GPL-2.0 | Operator-side dissection and filtering.  Not in the stack. |
| **smallstep `step` / `step-ca`** | Apache-2.0 | Recommended for minting the cloister CA — short-lived certs, scriptable, no openssl.cnf archaeology. |
| openssl / cfssl | Apache-2.0 / BSD | Fine.  Use whatever the operator already trusts. |

## The CA

Claude Code does no certificate pinning, and Anthropic documents
supporting TLS-inspection appliances outright: enterprise proxies "work
without additional configuration when their root certificate is
installed in the OS trust store and the runtime can read it."  We are
not fighting the client.

Three trust layers exist, and we control all three:

| Layer | Lever |
|---|---|
| Mozilla CA set bundled with Claude Code | `CLAUDE_CODE_CERT_STORE` (values: `bundled`, `system`; default `bundled,system`) |
| OS trust store — we build the image | Dockerfile |
| Additional roots | `NODE_EXTRA_CA_CERTS=/path/ca.pem` |

**The design of record is `CLAUDE_CODE_CERT_STORE=system` with an image
trust store containing exactly one certificate: the cloister CA.**  Not
merely *adding* our root — *removing* every other one.  The result is
belt and suspenders: the topology means no route exists to anything but
the proxy, and the trust store means no issuer is trusted but ours.  An
agent that somehow found a path to a real internet host would fail
validation against a perfectly legitimate public certificate, loudly, in
a log we read.

Three caveats:

- Reading the OS store needs a runtime with `tls.getCACertificates` —
  Node 22.15 or later for npm installs.  `docker/workbench/Dockerfile:34`
  pins `node:22-bookworm-slim`, which is past that, but the pin should
  become explicit: on an older runtime `CLAUDE_CODE_CERT_STORE=system`
  degrades silently to bundled-only, and the suspenders quietly vanish.
- `CLAUDE_CODE_CERT_STORE` has no `settings.json` schema key.  Set it in
  the process environment or a settings `env` block.
- **Never** `NODE_TLS_REJECT_UNAUTHORIZED=0`.  It disables validation
  globally and throws away the second guarantee to save a config line.
- **The single-cert store is image-wide, not Claude-wide.**
  `/etc/ssl/certs` is the system trust store for Go, git, curl, npm, and
  python too.  Builds are offline so this is probably survivable, but
  "probably" is not the standard: any TLS a toolchain attempts will fail
  in a confusing way, and any image-build layer *after* the swap that
  needs TLS breaks outright.  Swap it last, and expect to revisit this
  the first time a build wants a network.

The CA private key is a fleet-wide MITM key.  It lives in the abbey, in
`claude-proxy`, and only the public certificate is mounted into the
cell.  Mint it for this purpose alone — a CA that is also installed in
the operator's workstation trust store turns an abbey compromise into a
browsing compromise.  Other toolchains in the image keep their own
stores (the JVM's `cacerts` is separate from `/etc/ssl/certs` and needs
a `keytool` import); builds are offline so this is mostly moot, and it
will still bite the first time something in a build wants TLS.

## Packet capture

**Scope: a short-lived diagnostic, not an audit log.**  The tap is
something the operator switches on to answer a specific question — what
is the harness sending, how large is the system prompt, did the
archivist call actually go out, what does the error path look like — and
switches off when the question is answered.  It is emphatically **not**
a durable record of what the agent did.

That distinction matters because a decrypted capture is the most
sensitive artifact this design produces, and its risk grows with its
lifetime.  The one-way-glass audit trail already exists and lives in the
state service; the archivist's records are the durable account of agent
actions, and they are the thing to extend if longer-horizon logging is
wanted.  Do not let the pcap drift into that role.  In practice: capture
per session, not continuously; treat each file as scratch with an
explicit expiry; and delete rather than archive.  Ring-buffer the writes
(`tcpdump -C 100 -W 20`) — not for disk, which is not a real constraint
on this host, but because an unsegmented overnight capture is a single
multi-gigabyte pcap that Wireshark will choke on.

Two paths.  Build the first; keep the second in reserve.

**Primary — the plaintext hop.**  `claude-tap` runs
`tcpdump -i any -w /captures/<session>.pcapng` inside `claude-proxy`'s
network namespace.  Wireshark opens the result and dissects it as HTTP
with zero configuration: request lines, headers, JSON bodies, and the
SSE token stream.  No keys to manage, no decryption step, and — because
of hop ordering — no credential in the file.

Fidelity caveat: the plaintext hop normalizes HTTP/2 down to HTTP/1.1,
so this is not the true wire framing.  For "what is the harness sending
and how big is it," that is a feature.  For anything about h2 framing,
flow control, or the TLS handshake itself, it is a lie.

**Secondary — the TLS keylog.**  Node accepts `--tls-keylog=<file>`, and
that option *is* permitted inside `NODE_OPTIONS`, so
`NODE_OPTIONS=--tls-keylog=/captures/tls-keys.log` on the agent plus a
`tcpdump` in the agent's namespace yields a ciphertext pcap that
Wireshark decrypts via *(Pre)-Master-Secret log filename*.  That gives
true HTTP/2 frames and the real handshake.  Two caveats: `NODE_OPTIONS`
applies to every Node process in the image, qwen included; and a keylog
plus its pcap decrypt everything captured, so the pair inherits whatever
the stream carried.  Under this design the in-cell stream carries only
the placeholder token, which keeps the keylog path credential-free too.

Response mutation is a mitmproxy addon (`def response(flow)`).  Note
that mutating a *streamed* SSE response requires handling it at the
`responseheaders` hook and disabling streaming for that flow, or the
body is gone by the time `response` fires.

## Credential handling

| | Placement | Assessment |
|---|---|---|
| A | `CLAUDE_CODE_OAUTH_TOKEN` in the cell | Simple, and the model can read it.  Acceptable for a first attended bring-up; not for overnight. |
| **B** | **Injected by `claude-egress` from a mounted file** | **Design of record.**  The cell gets `ANTHROPIC_AUTH_TOKEN=placeholder`; the door strips it and sets the real header.  Restores the archivist's `GITHUB_TOKEN_FILE` property — the secret never enters a container the model controls. |

A placeholder works because Claude Code never validates the credential
locally: `ANTHROPIC_AUTH_TOKEN` is precedence rank 2 and is sent verbatim
as `Authorization: Bearer`.  Setting it is what stops the harness from
trying to open a browser login flow that cannot possibly complete
inside a jail.

### Which credential

The two options pull in opposite directions, and neither dominates:

| | Subscription token (`claude setup-token`) | Console API key |
|---|---|---|
| **API surface** | **Model requests only** — structurally cannot reach the Files or Batches APIs | **Full** — including the upload endpoints that make an exfiltration path |
| **Terms** | Consumer (Pro/Max): 5-year retention with the training toggle on, 30-day off | Commercial: no training on content, 30-day retention |
| **Cost** | Bills against the plan | Metered |
| **Delivery** | Env var only (`CLAUDE_CODE_OAUTH_TOKEN`) | `apiKeyHelper` reads a mounted file — never in the environment, re-invoked on 401 and every five minutes (`CLAUDE_CODE_API_KEY_HELPER_TTL_MS`) |
| **Lifetime** | One year, revoked at claude.ai → Settings → Claude Code | Expiration settable at creation; revoked in the Console |

The **Delivery** row applies to placement A only — under placement B no
credential enters the cell at all, so `apiKeyHelper` becomes moot and
both options arrive the same way: a file mounted into `claude-egress`.

The credential with the better data posture has the worse capability
posture.  That is the whole tension, and the
[path allowlist](#the-path-allowlist--required-not-optional) is what
resolves it: with `/v1/messages` as the only reachable path, both
credentials are reduced to the same surface, and the choice becomes a
data-policy question again rather than a security one.

**Recommendation, staged:**

- **M1/M2, on this repo: the subscription token.**  It is narrower *by
  construction*, which is real defense in depth — if the allowlist
  addon is buggy or misconfigured, a model-requests-only credential
  still cannot reach the Files API, while an API key can.  The consumer
  terms cost nothing here because the repo is public.  Prefer it while
  the allowlist is new and unproven.
- **Any non-public tree: the Console API key,** for the commercial
  terms, plus `apiKeyHelper` so the key never enters the environment.
  At that point the allowlist is doing load-bearing security work rather
  than acting as a second layer, which raises the bar on verifying it —
  see [Known gaps](#known-gaps).

Do not read the first bullet as "consumer terms are fine."  It is
specific to a public repository, and the [gate](#doctrine--prose-where-the-repo-demands-mechanism)
that keeps this stack off private trees is the thing that makes it safe
to say.

### Rotating the credential

Rehearse these before the first unattended run, not after an incident.
The three flows are genuinely separate, and the most surprising fact is
that **the account-wide logout does not revoke Claude Code tokens.**

| Credential | Revocation flow |
|---|---|
| Claude Code OAuth token (`claude setup-token`) | claude.ai → **Settings → Claude Code** → trash-can icon on the token.  Managed independently of the account session. |
| Console API key | [platform.claude.com](https://platform.claude.com) → Settings → API keys → meatball menu → **Delete API Key**.  Keys also accept an expiration at creation — set one. |
| Everything session-based | claude.ai → initials (lower left) → Settings → **Account → Log Out** → confirm.  Signs out every device immediately.  **Does not** revoke Claude Code OAuth tokens — those need the row above. |

Anthropic's [compromised-key
guidance](https://support.claude.com/en/articles/8384961-what-should-i-do-if-i-suspect-my-api-key-has-been-compromised)
is the playbook if a capture escapes.

## Cell configuration

Environment:

```
CLAUDE_CODE_CERT_STORE=system
ANTHROPIC_AUTH_TOKEN=placeholder-the-door-replaces-this
DISABLE_AUTOUPDATER=1
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
DISABLE_FEEDBACK_COMMAND=1
CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1
ENABLE_CLAUDEAI_MCP_SERVERS=false
CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1
```

`settings.json`: `permissions.deny` on `WebFetch(*)` and `WebSearch(*)`
(the scholar's MCP surface is the only sanctioned research path), no
plugins, `autoMemoryDirectory` pointed per the decision above, and
`cleanupPeriodDays` set deliberately rather than left at 30.

**Launch with `--setting-sources user`.**  This is the load-bearing
line, and it is easy to miss.  Without it, settings and hooks in the
*checkout* — `.claude/settings.json`, `.claude/settings.local.json`,
`.claude/rules/` — are live configuration authored by whoever wrote the
branch, which in this design is the agent itself.  Excluding `project`
and `local` shuts that channel.  It is the better answer than
`disableAllHooks: true`, which would also disable the operator's own
`InstructionsLoaded` hook — a containment *instrument*, not a risk.  The
distinction to hold: operator-authored hooks delivered through the image
are wanted; tree-authored hooks are the thing to refuse.

MCP registration mirrors the qwen setup exactly — `archivist` at
`http://archivist:9600/mcp` and `scholar` at `http://scholar:9500/mcp`,
both plain HTTP on internal networks and therefore untouched by any of
the TLS work above.  Holding that surface constant is the entire point
of the diagnostic.  Pass them with `--mcp-config` and add
`--strict-mcp-config`, which loads *only* the servers named on the flag
and ignores every other source — so the agent cannot register a server
of its own.

The image entrypoint's stock-prompt materialization needs a Claude arm
(`~/.claude/CLAUDE.md` alongside `~/.qwen/QWEN.md`); the Dockerfile
already anticipates this ("future agent variants add theirs").

### What blocks the build

- **compose-lint.**  It pins every internal network's membership and
  enforces the single-inference-door rule.  A new `claudenet`, a new
  internet holder, and a second inference door all have to be taught to
  it explicitly — the point of that lint is that this design cannot land
  by accident.
- **Licensing.**  See [Publishing the recipe](#publishing-the-recipe).

## Teaching Claude its environment

The agent has to be told what qwen is told: the archivist owns git, the
grange is the workspace, there is no network, nothing about the cell
goes into a recorded file.  Claude's larger context window makes it
affordable to say more — but *more* is not the interesting axis.  Where
the instructions land, and whether the agent can edit them, is.

### Three mechanisms, and they are not equivalent

| Mechanism | Lands in | Agent can alter it? | Survives `/compact`? |
|---|---|---|---|
| **`--append-system-prompt-file <path>`** | the actual system prompt | No — the file is a read-only mount, the flag is workbench's | **Yes** — the system prompt always does |
| **Managed policy `/etc/claude-code/CLAUDE.md`** | a user message *after* the system prompt | No — root-owned read-only mount, and `claudeMdExcludes` explicitly cannot exclude managed policy | Undocumented; assume no |
| `~/.claude/CLAUDE.md` (user scope) | same | **Yes** — the home volume is uid-1000-owned | Undocumented |

The distinction matters more than the naming suggests.  Anthropic is
explicit that "CLAUDE.md content is delivered as a user message after
the system prompt, not as part of the system prompt itself," and that
for instructions you want at the system-prompt level you use
`--append-system-prompt`.  The documented objection to that flag — "this
must be passed every invocation, so it's better suited to scripts and
automation than interactive use" — does not apply here.  **Cloister has
a launcher.**  `workbench` already passes `--append-system-prompt` to
qwen and prints the prompt size; Claude Code takes the same flag plus a
`--append-system-prompt-file` variant, which is a straight improvement:
the prompt becomes a mounted file instead of an argv payload.

**The third row is disqualified for anything load-bearing.**
`~/.claude/CLAUDE.md` sits on a volume the agent owns, so an agent that
decides its instructions are inconvenient can rewrite them — and that
edit survives `dispose`, because the home volume does.  A
self-modifying, workspace-outliving instruction channel is the exact
shape of the persistence risk this design is already worried about.
Materialize nothing there.

**Design of record: use the first two together.**  The short,
load-bearing invariants — git reads and the archivist writes, the
workspace is fixed, there is no network — go in the appended system
prompt, where they survive compaction.  That property is not a detail
here: the motivating use case is an overnight run that will compact many
times, and rules that evaporate at the first compaction are worse than
no rules, because the early transcript looks fine.  The longer
environment description — the grange lifecycle, the MCP surface, the PR
gate — goes in the managed-policy CLAUDE.md, which is root-owned,
unexcludable, and cheap in a large window.

**Bias the split toward the system prompt, though.**  Only *project-root*
CLAUDE.md is documented to be re-read and re-injected after `/compact`;
managed-policy content is not, and should be assumed not to survive.
Over a run that compacts twenty times, that means the environment
description is present for the first segment and absent for the rest.
This partly undercuts the "large window, so say more" premise: the
window is a **per-segment** resource, and compaction — not window size —
is the real constraint.  Anything that must hold for the whole run
belongs in the appended system prompt, even when it is long enough to
feel like it belongs in the memory file.

*(Verify `--append-system-prompt-file` exists before building the mount
around it — the documentation reference is secondhand.  The fallback is
`--append-system-prompt` with the file read in the launcher, which is
what workbench already does for qwen.)*

```yaml
volumes:
  - ${CLAUDE_PROMPT_DIR}/system.md:/etc/cloister/claude-system.md:ro
  - ${CLAUDE_PROMPT_DIR}/CLAUDE.md:/etc/claude-code/CLAUDE.md:ro
```

Two notes on the mounts.  The rootfs is `read_only: true`, so
`/etc/claude-code` cannot be created at runtime — the bind has to supply
the path (docker creates mount points before remounting the rootfs
read-only, but verify it on the first build rather than assuming).  And
`managed-settings.json` has a `claudeMd` key that inlines the same
content, honored only in managed/policy scope; prefer the file, since a
mounted markdown file is reviewable in the repo and a JSON string
embedding markdown is not.

### Making sure it took

The doctrine is already settled and this design does not change it:
**delivery is proved mechanically, adherence is tested by a question**
(commit `de9dd49`, "workbench: drop the canary, spend the prompt tail on
rules instead").  A recited phrase measures neither.  What Claude
adds is better instruments for the first half:

- **`/context`** lists what actually loaded under **Memory files**.  This
  is the authoritative answer to "did the managed CLAUDE.md arrive" — if
  it is not in that list, Claude cannot see it.  `/memory` lists the
  locations it *looks* at, including ones that do not exist, which is
  how you catch a mount landing at the wrong path.
- **The `InstructionsLoaded` hook** logs exactly which instruction files
  loaded, when, and why.  This is the one that works unattended: have it
  append a line to the audit volume at session start, and an overnight
  run leaves behind proof of its own configuration.  It is also the
  reason not to set `disableAllHooks`.
- **The wire.**  This design already puts a packet capture on the
  plaintext hop, and the system prompt is in the body of the first
  request.  That is ground truth — not a UI assertion about what loaded,
  but the bytes that went out.  The MITM built for observing the harness
  turns out to be the strongest possible verification of the prompt, and
  it costs nothing extra.
- **`--debug api`** and workbench's existing prompt-size print cover the
  rest.

For adherence, keep the established check: ask what `git commit -m "x"`
would do.  A good answer names the refusal and the archivist tool; a bad
one says the commit succeeds.

## Publishing the recipe

**The recipe can be public; the image cannot.**  These are different
artifacts and the distinction is clean.

`LICENSE.md` in `anthropics/claude-code` reads, in full: "© Anthropic
PBC. All rights reserved. Use is subject to Anthropic's Commercial Terms
of Service."  All rights reserved means no redistribution right is
granted — which is exactly what `docker/workbench/Dockerfile:139`
already concluded.  Pushing a built image containing the CLI to a public
GHCR package *is* redistribution.  That stays off the table.

A Dockerfile is not a copy.  A layer that runs
`npm install -g @anthropic-ai/claude-code@<version>` contains no
Anthropic code; it names a publicly installable package that the person
running the build fetches themselves under their own license.  Telling
someone how to install software they are licensed to use has never been
distribution, and the precedent here is about as strong as precedent
gets: **Anthropic publishes its own containerization recipe**, including
`.devcontainer/init-firewall.sh` with a default-deny iptables policy, in
that same public repository.  A public cloister Dockerfile is the same
category of artifact.

So: **the Dockerfile, the compose stanzas, this document, the proxy
configuration, and the CA setup all live in the public tree.  Only the
built image goes to a private registry.**  Concretely, `images.yml`
grows a `cloister-workbench-claude` variant whose build definition is
public and whose push target is a private GHCR package; operators who
want it build or pull it themselves.  Nothing about the design has to be
held back, which matters — a containment design that cannot be reviewed
is not much of a containment design.

Three things to keep an eye on, none of them blocking:

- **Read the Commercial Terms before the first unattended run**, not
  for the redistribution clause — that one is settled — but for anything
  about automated or agentic use and account sharing.  A single operator
  running one jailed agent against their own subscription is the
  ordinary case; it is still worth ten minutes with the actual document,
  and Anthropic support can confirm in writing if anything reads
  ambiguously.  Nothing here is legal advice.
- **Frame the mutation capability carefully in public.**  Response
  rewriting exists in this design to fault-inject *our own* harness and
  test the archivist surface's error paths.  Published addon examples
  should demonstrate that — truncated responses, malformed tool calls,
  injected 429s — and not model-safety circumvention.  This matters more
  than it would in another repo, because
  [heretic.md](heretic.md) sits two files away and a careless reader
  will connect them.  The two are unrelated: heretic is about weights we
  build ourselves, this is about a proxy in front of a vendor API.
- **Captures are not documentation.**  No pcap, keylog, HAR, or flow
  dump belongs in the tree, publicly or privately.  `.gitignore` entries
  for `**/*.pcap*`, `**/*.keylog`, and the captures volume path go in
  before the first capture is taken, not after.

## Known gaps

From an adversarial pass over this document.  The path allowlist, the
spend cap, the nginx settings, the trust-store scope, and the
compaction/system-prompt correction are all folded in above; what
follows is what remains.

### Blocking M3

- **Does `permissions.deny` survive `bypassPermissions`?**  Unverified,
  and the design leans on `deny` for `WebSearch(*)` as the only
  mitigation for the broken grounding invariant — in exactly the mode
  M3 is built around.  If deny rules do not apply under
  `--dangerously-skip-permissions`, that mitigation is fiction.  The
  structural fallback is to refuse at the proxy: reject requests whose
  body declares the server-side `web_search` tool.  **Verify before the
  first unattended run, not after.**
- **A kill switch, a healthcheck, and a notification.**  The archivist
  has a healthcheck; `claude-proxy` and `claude-egress` have none.
  Nothing stops the run at 3am, and nothing says the door died at 2am —
  the run just stops making progress silently.  All three are cheap and
  all three are missing.

### Methodological — settle before building

**The experiment is asymmetric, and the motivation section oversells
it.**  "If Claude Code drives the archivist cleanly, the surface is
fine" does not follow: a capable model *routes around* bad interfaces —
much of what capability is.  A pass tells us a frontier model copes; it
says little about whether the surface is good.  Only a failure is
strongly informative.

Concretely: `publish-rename-gap` is a known archivist bug where a rename
was seen by in-container git and never reached the PR.  Would this
experiment have caught it?  Probably not — a capable agent would likely
have worked around it silently, which is the masking problem exactly.

**Before building, write down what specific failures would count as
evidence.**  A diagnostic whose success criterion is "it went fine" is
not a diagnostic.

### Doctrine — prose where the repo demands mechanism

CLAUDE.md says invariants are "topology + tests, NOT prompt text."
Several mitigations here are prose: *frame the mutation capability
carefully*, *decide `autoMemoryDirectory` deliberately*, *captures are
not documentation*, and — the load-bearing one — *a per-cell decision
for anything not public*.

Nothing currently prevents this stack being pointed at a private repo
under consumer retention terms.  That one deserves a real gate in the
house style: a `${VAR:?message}` guard, or a provision-time refusal
unless the target repo is public or an explicit acknowledgment variable
is set.  Cheap, and it converts a sentence into a control.

### Operational

- **A shared door has no per-cell attribution.**  One proxy, one
  credential, every cell: interleaved captures, no per-project spend
  accounting, and any one cell can exhaust the shared budget.  Cells
  carry `AUDIT_ORIGIN`; the door has no equivalent.  The spend cap above
  is fleet-wide until this is fixed.
- **The CA has no lifecycle.**  No expiry, no rotation, and cell-side
  trust is permanent.  For a fleet-wide MITM key that is not good
  enough for long.
- **Watchtower.**  `cell.yaml` is deliberate about which services carry
  the update label and which do not.  The new abbey services are silent
  on it; make the call explicitly.
- **The grange volume is unbounded.**  Disk is not a real constraint on
  this host, so this is a low-severity note rather than a risk: a
  runaway loop in one cell writes into shared storage with no quota, and
  the failure mode if it ever mattered would be fleet-wide rather than
  cell-local.

## Staging

1. **M1 — observe only.**  No mutation addon.  Attended session, cell
   credential (placement A), plaintext-hop capture — **plus the path
   allowlist**, which is not deferrable: it is what makes "one pinned
   host" mean anything, and it costs a few lines.  Answers the
   diagnostic question and tells us what the harness actually reaches
   for.
2. **M2 — injection, mutation, and the cap.**  Move to placement B; add
   the mutation addon and the spend tally.  Now the pcaps are
   credential-free, responses can be rewritten for fault injection, and
   there is a budget the run cannot exceed.
3. **M3 — overnight YOLO.**  `--dangerously-skip-permissions`, a
   long-running task, and the whole point of the exercise.  Gated on:
   the `permissions.deny`-under-`bypassPermissions` verification;
   healthchecks, a kill switch, and a failure notification; a rehearsed
   credential rotation; a `.gitignore` covering captures; and a
   deliberate answer to the session-memory question.

## References

- [Enterprise network configuration](https://code.claude.com/docs/en/network-config) — the allowlist table, proxy vars, `CLAUDE_CODE_CERT_STORE`, TLS-inspection support
- [Authentication](https://code.claude.com/docs/en/authentication) — credential storage, precedence, `setup-token`, `apiKeyHelper`
- [Data usage](https://code.claude.com/docs/en/data-usage) — telemetry opt-outs, WebFetch domain safety check, retention
- [Web search tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool) — server-side execution
- [Memory](https://code.claude.com/docs/en/memory) — CLAUDE.md precedence, the managed-policy path, `claudeMdExcludes`, auto memory, `/context`
- [CLI reference](https://code.claude.com/docs/en/cli-reference) — `--append-system-prompt-file`, `--setting-sources`, `--strict-mcp-config`, `--permission-mode`
- [claude-code LICENSE.md](https://github.com/anthropics/claude-code/blob/main/LICENSE.md) and [.devcontainer/](https://github.com/anthropics/claude-code/tree/main/.devcontainer) — all rights reserved, and Anthropic's own public containerization recipe
- [Log out of all active sessions](https://support.claude.com/en/articles/10310342-how-do-i-log-out-of-all-active-sessions) — and why it doesn't cover Claude Code tokens
- [Compromised API key](https://support.claude.com/en/articles/8384961-what-should-i-do-if-i-suspect-my-api-key-has-been-compromised)
- [grange.md](grange.md), [abbey.md](abbey.md), [git-proxy.md](git-proxy.md) — the containment this design borrows from and bends
