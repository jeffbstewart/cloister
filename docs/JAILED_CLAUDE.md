# Jailed Claude

*Status: **M1 topology built; the image and the launcher are not.**
Decisions from the 2026-08-04 investigation.  This is a deliberate, scoped
relaxation of the cell's network containment, taken to answer a question
the current topology cannot answer.  Read
[What this costs](#what-this-costs) before deploying any of it.*

*Built: the door (`docker/abbey-claude.yaml`, `docker/cell-claude.yaml`,
the request-gate addon, the injecting hop, and compose-lint's invariant
set for both overlays); the `cloister-workbench-claude` image variant and
its launcher; and the certificate tooling
(`lifecycle/claude-door-cert.sh`).  See [Deploying M1](#deploying-m1).*

*Not yet built: the archivist's
[disclosure gate](#the-disclosure-gate).  **Nothing here has been run end
to end** — no image has been built, no session started.  The first deploy
is the test.*

## Why

Three motivations, in priority order.  The first is the prize; the other
two are things this work is well placed to answer on the way.

**1. Move daily development into the jail.**  Not overnight autonomy as
a special occasion — *ordinary work*, done in a contained workspace, in
YOLO mode, because the blast radius is bounded.  That is already a
significant win over the status quo whether or not anything else here
pans out.  Throughput should go up: the operator stops policing classes
of risk that the jail makes structurally unavailable, and the agent
stops waiting for consent it no longer needs.  The plausible failure
mode is not damage, it is *friction* — the mediated surface being worse
to work through than plain tools on the host.  Measuring that friction
honestly is what [Success criteria](#success-criteria) is for.

**2. Find out whether the system works together.**  The qwen agent
overruns its context window and degrades in ways that are hard to
attribute: Chinese glyphs, a system prompt it stops honoring, a
mishandled archivist surface.  We cannot tell whether that is the
archivist's surface, the qwen-code harness, or the model.  Driving the
*same* MCP surface with a known-good harness and a capable model
narrows it — with the caveat that a capable model routes around bad
interfaces, so a clean pass is weaker evidence than it looks.  That is
why the grading below counts *how much help the agent needed*, not just
whether it finished.

**3. Build the hedge.**  Qwen's role is not cost optimization.  The
concern is that current subscription pricing does not reflect what the
capacity costs to provide, and that the correction arrives as either
unavailability or a step change in price.  Qwen is insurance against
that day, and insurance only pays if it is kept warm.  The consequence
for this design is concrete: **the cell must not quietly become
Claude-shaped.**  The archivist surface stays harness-agnostic, the
environment prompt stays portable across both CLIs, and the qwen path
keeps getting exercised after Claude works — because that is exactly
when it would otherwise rot, and exactly when nobody would notice.

The trade this design makes to get all three: give up *network*
containment, keep *filesystem*, *credential*, and *version-control*
containment.  Today the alternative is running a frontier model on the
host, unjailed, with the operator's whole home directory in reach.

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
| All inference rides through the agency (the sole inference door) | **Broken — but only for a host that opts in.**  A second inference door exists in `docker/abbey-claude.yaml`.  Because that is an overlay rather than an edit, compose-lint's rule still holds absolutely on `docker/abbey.yaml` and `docker/cell.yaml`; "sole inference door" remains a true claim about the base topology and a false one about a merged one.  The overlays carry their own invariant set instead of weakening that rule. |
| The cell holds no route to the internet | **Broken by design.**  The agent gains a mediated, single-host, inspected route out.  It is narrower than a host-run Claude Code by a wide margin, and it is not zero. |
| Research answers grounded in retrieved results, structurally | **Broken.**  Claude Code's `WebSearch` executes server-side on Anthropic's infrastructure; results arrive inside the model response.  No relay, proxy, or capture can see the fetch, and the scholar cannot mediate it.  `permissions.deny` on `WebSearch` is **not** a usable mitigation: it is verifiably inert under `bypassPermissions` (see [Verified against the binary](#verified-against-the-binary)), which is the mode this design exists to enable.  The only real control is a structural refusal at the proxy. |
| Source confidentiality (implicit) | **Broken by design.**  The repo's contents leave the machine and go to Anthropic.  Under a Pro/Max consumer account that carries a 5-year retention window if the training toggle is on.  Acceptable for this repo, which is public; a per-cell decision for anything that is not. |
| The scholar holds no `egress` network | Intact.  A merged abbey gains a **fourth** internet holder (`claude-egress`) — but it arrives in the overlay, so `docker/abbey.yaml`'s "three internet holders" comment stays true of the file it is written in, and the overlay's own lint pins the addition at exactly one. |
| Builds run offline; no package-registry route | Intact.  Claude Code is installed at image build; `DISABLE_AUTOUPDATER=1` keeps it from reaching for one at runtime. |
| Agent bytes reach the canonical tree only via a human-reviewed PR | Intact.  The archivist, the forge ruleset, and the bot-credential split are untouched.  This is the guarantee the whole exercise is meant to *test*, not to relax. |
| The operator's host tree never enters a cell | Intact.  The grange remains the only workspace. |
| The audit trail is one-way glass | Intact, and arguably strengthened: the proxy capture is a second append-only record.  But note the Claude session's own reasoning and tool calls are not in the state service, exactly as qwen's are not. |
| No secrets or home/LAN IPs in the repo | **Intact, and closed before the first capture existed.**  Captures land on the `abbey_claude_captures` volume, never the tree, and `.gitignore` covers pcap, pcapng, har, flow, and keylog.  One thing the original wording would have missed: `tcpdump -C -W` appends a segment index (`claudeplain.pcap0`), which `*.pcap` does not match — so every file the tap actually produces would have stayed committable.  `*.pcap[0-9]*` covers it. |

### Two risks with no invariant to name

- **The proxy is trusted code in the agent's decision path.**  A proxy
  that can rewrite responses can inject arbitrary model output, which
  is arbitrary agent behavior.  The mutation addon and the CA private
  key join the archivist's bot token in the small set of things whose
  compromise compromises everything.
- ~~**Session memory outlives the grange.**~~  **Closed, structurally, and
  it turned out to be the smaller half of a bigger hole.**

  The risk as first written was that `~/.claude` on the per-project home
  volume persists across `dispose`, making transcripts and auto-memory a
  persistence channel that survives the workspace destruction the design
  turns on.  The proposed fix was the `autoMemoryDirectory` setting.
  **That fix would not have been verifiable:** Claude Code silently
  ignores settings keys it does not recognize — feed it
  `{"totallyBogusKeyXyz":1}` and it succeeds without a murmur — so a key
  that does nothing looks exactly like a key that works.  The same trap
  as `permissions.deny`, one layer down.

  What is verifiable is `CLAUDE_CONFIG_DIR`: point it somewhere new and
  the entire state tree appears there — `sessions`, `projects`,
  `backups`, and `.claude.json`.  The cell sets it to a **tmpfs**, so
  every byte the harness persists dies with the container.  compose-lint
  enforces the pairing; a settings key could not have been enforced at
  all.

  Asking *why* the home volume survived `dispose` found no design
  rationale for it.  `docs/ARCHITECTURE.md` argues only for **scoping**
  — project A's dotfiles must not reach project B — and durability came
  along because a named volume is how you get scoping.  The consequence
  was wider than session memory: `~/.bashrc` is seeded from `/etc/skel`
  into that volume, is agent-writable from then on, and is *executed* by
  every later interactive shell.  An agent that wrote a dotfile had code
  running in every subsequent task in that cell.  `workbench` now resets
  HOME to the image skeleton at `dispose`, sparing the nested mounts it
  discovers from the kernel's mount table rather than from a hard-coded
  list — because the failure mode of that list drifting is deleting the
  operator's warmed dependency caches.

  This one predated jailed-claude and applied to the qwen cell exactly
  as much.  It is fixed for both.

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

### The `web_search` refusal

`WebSearch` runs server-side on Anthropic's infrastructure, so the
scholar cannot mediate it and no capture can see the fetch.  The
config-level answer — `permissions.deny` on `WebSearch(*)` — is
**measured to be inert** in the mode this design exists to enable (see
[Verified against the binary](#verified-against-the-binary)).  There is
no in-cell control here.

So the proxy refuses it, in the same request hook as the path allowlist:
inspect the JSON body of each `/v1/messages` call and reject any that
declares a server-side `web_search` tool.  The tool declaration is in
the request body in plain sight on the plaintext hop — this is a body
inspection, not a heuristic, and it holds regardless of what the harness
or the model would prefer.

This is the difference between a rule the agent is asked to follow and
one it cannot route around.  CLAUDE.md's insistence that invariants live
in topology and tests rather than prompt text is exactly this
distinction, and the grounding invariant is the case that proves it: a
config flag looked like enforcement until it was measured.

**As built the refusal is broader than `web_search`, and deliberately a
denylist.**  It covers `web_fetch`, the code-execution tool family, and
`mcp_servers` — the server-side MCP connector, which is the same hole in
a different shape — because all of them execute on Anthropic's side of
our last hop, where no capture and no relay can follow.  An *allowlist*
would be stronger and is the M2 move; it is not the M1 move, because we
do not yet know which tool types Claude Code declares.  Some
Anthropic-defined types are server-*defined* but client-*executed* (the
text-editor and bash tool schemas) and are perfectly safe; refusing those
would break the harness for nothing.  So M1 refuses what it knows and
**logs the rest**, which is exactly the observation that lets M2 tighten
against data rather than against a guess.

### The one stand-down, and why it is not a hole

The refusal above assumes the scholar is reachable.  In a cell it always
is, which is what makes refusing `WebSearch` a *redirection* rather than
an amputation.  **On-host evaluation runs of this proxy have no
scholar** — it is an abbey service, and a host-run Claude Code cannot
dial it — so refusing there would leave the session with no research path
at all, friction bought for nothing, on a harness that was not jailed to
begin with.

So `CLOISTER_DOOR_ALLOW_SERVER_TOOLS=1` stands down the server-side-tool
refusal, and *only* that one.  The host pin and the path allowlist hold
unconditionally; they are what makes the door a door.

It is safe for the same structural reason the git-passthrough escape
hatch is: **the model cannot reach the place the switch lives.**
`claude-proxy` is an abbey container with no agent in it and no
filesystem any cell shares, so its environment is the operator's alone —
this is a topology property, not a permission one.  compose-lint refuses
an overlay whose default is anything but `0`, the addon announces the
stand-down once at startup, and every waiver is logged by name: a control
that is off should say so in a log the operator is already reading,
rather than wait to be discovered by its absence.

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

**The CA private key does not have to live anywhere in the stack, and
does not.**  The design said it lives in `claude-proxy`, which follows
from mitmproxy's usual job: minting a certificate per host it is asked to
impersonate needs the signing key on hand.  This door impersonates
exactly *one* host, forever.  So `claude-proxy` gets `--certs
api.anthropic.com=<leaf.pem>` — a single pre-minted leaf, its private
key, and the issuing chain — and there is nothing left for it to sign.

That is worth more than it sounds.  A leaf key compromise forges one
hostname that the topology already routes to us; a CA key compromise
forges *every* hostname, against a trust store that trusts only that CA,
in every cell on the machine.  Keeping the fleet-wide MITM key entirely
out of the running stack costs one offline minting step.

The CA key still exists, of course, and is still the thing to guard —
just on the operator's side of the boundary, alongside whatever mints the
leaf.  Which CA that should be is the next section, and the answer is not
the one this design originally assumed.

Other toolchains in the image keep their own stores (the JVM's `cacerts`
is separate from `/etc/ssl/certs` and needs a `keytool` import); builds
are offline so this is mostly moot, and it will still bite the first time
something in a build wants TLS.

### Use the operator's existing CA; do not stand up a new one

The operator already runs a private CA on the firewall.  **Use it.**
Standing up a second CA for this is ceremony, and the advice above to
"mint it for this purpose alone" was written on the assumption the CA
*private key* would live in `claude-proxy` — which, since the door serves
one hostname and needs only a pre-minted leaf, it does not.  Remove that
assumption and most of the argument for a dedicated CA goes with it.

The distinction that does the work here is between an **issuing CA** and
a **TLS-inspecting CA**, and it is worth stating because conflating them
gets the answer backwards.  An inspection appliance re-signs every site
it proxies, so its root vouches for the whole internet; putting *that* in
the cell's single-certificate store would gut the second guarantee, since
every reachable host would then validate cleanly.  A plain issuing CA
vouches for exactly what it has been asked to sign.  The cell trusting it
means the cell trusts that small, known set — and holds no route to any
of it regardless.

Two consequences to accept deliberately, neither of them blocking:

- **The cell trusts everything that CA has signed,** not just our leaf.
  Enumerate that set once.  If it is internal LAN services the cell has
  no route to, this is moot; if it ever includes something a cell *can*
  reach, the store has stopped being a second opinion.
- **A leak of the leaf key forges `api.anthropic.com` to anything
  trusting that CA** — including the dev host, whose own Claude Code
  talks to that name.  Bounded to one hostname, and rotation is
  reissuing one leaf from a CA that is already there, which is a better
  rotation story than the "no lifecycle" gap noted below.  It is the
  reason the *CA* key stays out of the stack while the *leaf* key goes
  in.

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

**Treat those deny rules as documentation of intent, not as a control.**
They are measured to be inert under `bypassPermissions` — the mode M3
runs in — so they express what the agent is supposed to do and enforce
nothing.  The enforcement lives at the proxy.  Keeping them costs
nothing and they do bite in attended M1 sessions that run without the
bypass flag; relying on them anywhere else is a mistake this document
made once already.

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

### The disclosure gate

Source from the workspace goes to Anthropic.  That is not a defect and
for most trees it is not even interesting — but it is a decision, and a
decision made once and then inherited silently by every later cell is
not a decision.  The gate exists to make the operator touch it **per
repository**.

**A boolean is the wrong shape.**  `DISCLOSURE_ACK=1` survives a
copy-paste of a working cell's environment into a new one, which is
precisely the case worth catching: not a careless operator, but a
careful one reusing a config that already worked.  A flag that is
already set cannot ask a question.

So both the variable's **name** and its **value** are derived from the
repository:

```
CLOISTER_DISCLOSURE_JEFFBSTEWART_EXAMPLEREPO=
    "source from jeffbstewart/examplerepo is sent to anthropic"
```

The name carries a slug of the repo (`owner/name`, uppercased,
non-alphanumerics to `_`).  The value is a sentence naming the same
repo.  Copying a working stanza to a new cell therefore fails twice
over: the required variable is absent entirely, and the inherited one
names the wrong repository.  There is no value that satisfies the gate
for two different trees, which is the property a boolean cannot have.

**Enforced by the archivist's provision gate**, which is the only place
that authoritatively knows the repository — it is the thing doing the
clone — and provision is the once-per-workspace moment.  It already
re-verifies the forge ruleset there; this is the same shape of check.
A compose `${VAR:?message}` guard cannot do it, because compose has no
nested interpolation and so cannot construct a variable name from
`${PROJECT}`.

**On what the failure message should print.**  Naming the expected
*variable* is necessary or the gate is merely obstructive.  Printing the
expected *value* verbatim is more arguable: it makes the acknowledgment
satisfiable by copying from the error, which is most of the deliberation
gone.  The recommendation is to print the variable name and describe the
required sentence — "the value must state that source from
`<owner>/<name>` is sent to Anthropic" — leaving the operator to compose
it.  Marginally more friction, meaningfully more read.

This is the same idea as the git-passthrough escape hatch: a control
whose safety comes from being deliberately awkward to satisfy, in a
tree that otherwise optimizes for the operator's convenience.  Both are
cheap because they fire once.

### What blocks the build

- ~~**compose-lint.**~~  **Settled, and the answer changed the shape of
  the build.**  The rules this design breaks are all *exact-membership*
  rules — three egress holders, three agent networks, one inference door
  — so relaxing them in place would have meant every cell on every host
  carrying the loosened rule whether or not it ran jailed-claude.
  Instead the door lands as two **overlay files**,
  `docker/abbey-claude.yaml` and `docker/cell-claude.yaml`, merged with
  `-f`.  The base files are untouched and their invariants stay absolute;
  merging an overlay is the act that trades them away, which is the
  property "variant-scoped exception" was reaching for.

  The overlays are not unlinted.  They carry their own set — stricter
  about the *delta* than the base rules are absolute about the whole:
  exactly one new internet holder, exactly one new agent edge, the
  credential in exactly one container, the request-gate addon loaded, the
  tap profile-gated, and a placeholder token in the cell.  See
  `internal/composelint/claudedoor.go`.
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
| **`--append-system-prompt <prompt>`** | the actual system prompt | No — workbench reads a read-only mount and sets the flag; the agent controls neither | **Yes** — the system prompt always does |
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
qwen and prints the prompt size, and Claude Code takes the same flag with
the same signature — `--append-system-prompt <prompt>`, a string.

**`--append-system-prompt-file <path>` exists**, despite not appearing in
`claude --help`'s flag list — see
[The flag that `--help` denies](#the-flag-that---help-denies), which
corrects an earlier finding here.  So the launcher passes the mounted
file's **path**, and the prompt never enters argv at all.

The containment property is what it always was, and does not depend on
which of the two flags is used: the file is a root-owned read-only path
the agent cannot edit, and the flag is set by a launcher the agent cannot
name.

Two consequences of it being a path rather than argv, both of them
things that stop being problems:

- **No quoting surface.**  A multi-kilobyte markdown blob full of
  quotes, backticks and `$` never crosses a shell or an argument
  boundary.  (`cmd/workbench` uses `exec.Command` regardless, which
  passes arguments directly — but the wrapper script in the image would
  otherwise have had to quote `$(cat …)` exactly right, forever.)
- **No ceiling.**  Linux caps a single argument at 128 KiB
  (`MAX_ARG_STRLEN`); a path is a few dozen bytes.  This matters more
  than it sounds, because the section below argues for pushing *more*
  content into the system prompt — that advice previously ran into a
  hard stop at roughly 30k tokens, and now does not.

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

**Bias the split toward the system prompt anyway.**  Managed policy was
measured to survive a compaction — the canary came back with no tool
call, so the content was in context rather than fetched on demand.  That
is better than the documentation implies, and it is *not* the same as a
guarantee.

Two mechanisms produce that observation and the probe cannot tell them
apart: the managed file being re-injected, or the compaction summary
carrying the content forward.  Re-injection would hold indefinitely;
summary-carryover is lossy and decays across repeated compactions.  An
overnight run compacts many times, so the difference is exactly the case
we care about and exactly the case that was not measured.

The residual ambiguity does not change the decision, which is why it is
recorded rather than chased: under either mechanism, one compaction is
proven and twenty are not.  So the window remains a **per-segment**
resource, compaction rather than window size remains the real
constraint, and anything that must hold for the whole run belongs in the
appended system prompt — even when it is long enough to feel like it
belongs in the memory file.  The environment *description* can live in
managed policy, where losing it late in a run degrades quality rather
than breaking an invariant.

The prompt is still a mounted file — workbench is just the thing that
reads it:

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

## Verified against the binary

Claims here are checked against **observed behaviour** — the binary
invoked, not the binary's help text and not the documentation.  That
wording is deliberate and was paid for twice.  The docs page describes a
`permissions.deny` control that turns out to be inert under
`bypassPermissions`; `claude --help` omits a flag that turns out to work.
Documentation overstated a control, help output understated a
capability, and only invocation caught either.  **Trust the binary, and
"the binary" means running it.**

Findings are **version-scoped** — reproduce them with
`lifecycle/probe-claude-harness.sh` after any Claude Code upgrade that
lands before the cell is built.

Tested against **Claude Code 2.1.221**, and re-checked against
**2.1.222** where noted.

| Claim | Result | Consequence |
|---|---|---|
| `--append-system-prompt-file <path>` exists | **Yes — this reverses an earlier finding.**  Present and functional in 2.1.222, and `--help` still does not list it.  See [The flag that `--help` denies](#the-flag-that---help-denies). | Workbench passes a PATH, not contents.  The 128 KiB `MAX_ARG_STRLEN` ceiling stops being a constraint, and a multi-kilobyte prompt leaves argv entirely. |
| `permissions.deny` applies under `bypassPermissions` | **No.**  Verifiably inert. | The grounding mitigation moves from config to a structural refusal at the proxy.  Deny rules stay as documentation of intent. |
| Rate-limit header names readable from `--debug` | **No.**  The debug log is produced but carries no rate-limit or retry headers. | Deferred to the Phase 1 host proxy, which sees every header in cleartext by construction. |
| Managed-policy `CLAUDE.md` survives `/compact` | **Yes, at least once.**  Post-compaction the canary was recalled directly, with no observed tool call — so the content was in context, not fetched on demand.  Whether it got there by re-injection or by the compaction summary carrying it is **unresolved**. | The split stands — but managed policy is *best-effort*, not guaranteed.  Anything that must hold for a whole overnight run still belongs in the appended system prompt. |
| `--setting-sources user` excludes project-tree rules | **Yes.**  A project rule that fires under default sources does not fire with the flag — same rule file, same prompt, one flag added. | The cell's lever against agent-authored config in the checkout is real.  Launch with it. |

### The flag that `--help` denies

`--append-system-prompt-file <path>` **exists and works.**  This document
previously recorded that it does not, and used that to justify passing a
multi-kilobyte prompt through argv.  Both the finding and the reasoning
built on it were wrong.

What makes it worth more than a correction is *how* the wrong answer was
reached, because it was reached by following this section's own rule.
The earlier check greppped `claude --help`, found no such flag, and
concluded — reasonably — that the binary does not have it.  It still does
not appear in the flag list in 2.1.222.  It is nonetheless accepted, and
honoured:

```
$ claude --append-system-prompt-file /tmp/nope.md -p x
Error: Append system prompt file not found: /tmp/nope.md
```

That is the parser reading the flag and reaching for the file.  A canary
run settles that the contents actually arrive: a phrase present only in
the file, asked for in a session that never mentions it, comes back
verbatim — and does not without the flag.

**"Trust the binary" is not the same as "trust `--help`."**  Help output
is documentation that happens to ship inside the binary, and it is
incomplete in both directions here: it omits this flag from the flag
list while *mentioning* `--append-system-prompt[-file]` in the prose of a
different flag's description.  A grep found the absence and missed the
mention.  The only test that settles a flag is invoking it, which is the
same lesson `permissions.deny` taught from the opposite direction —
there, a control that looked real was inert; here, a control that looked
absent was live.  `lifecycle/probe-claude-harness.sh` step 5 now runs the
invocation.

Consequences, all of them simplifications:

- Workbench passes a **path**, not contents.  The 128 KiB
  `MAX_ARG_STRLEN` ceiling is no longer a constraint on how much the
  appended system prompt may carry, so the argument for pushing content
  into managed policy to save argv space disappears.
- The prompt leaves argv, so it no longer appears in `ps`, and the
  shell-quoting hazard that argued for a wrapper script is moot on this
  path (the wrapper stays for other reasons — fail-closed if the prompt
  is missing).
- The containment property is unchanged either way: the file is a
  root-owned read-only path the agent cannot edit, and the flag is set by
  a launcher the agent cannot name.

### How the deny result was established

Worth recording, because the obvious experiment gives a false positive.

Denying `Bash(*)` and watching a `touch` succeed does **not** prove the
rule was ignored — it is equally consistent with `--settings` never
being applied at all.  The obvious control (the same rule *without*
`--dangerously-skip-permissions`) does not separate those either:
headless mode will not run Bash without that flag under any settings, so
the marker is absent for an unrelated reason.

The discriminator is an **observable sibling in the same settings
object** — an `env` canary next to the deny rule:

```
--settings '{"env":{"PROBE_VAR":"CANARY_9Z"},
             "permissions":{"deny":["Bash(*)"]}}'
```

One call then answers both questions at once.  The canary came back, so
the object was read and applied; the command ran anyway, so the deny
rule inside that same honoured object was not enforced.  There is no
remaining hypothesis in which the rule held.

The general lesson is worth more than the specific finding: a
config-level control that has never been *observed failing closed* should
be assumed not to be a control.  This one looked like enforcement in the
documentation, in the settings schema, and in this document — until it
was measured.

The lesson cuts both ways, which is why it is a lesson about *measuring*
rather than about distrusting configuration.  Two config-level
mechanisms this design leans on were tested the same way and came back
opposite: `permissions.deny` is inert under bypass, while
`--setting-sources user` genuinely does shut the tree-authored config
channel.  Neither result was predictable from the documentation.  Assume
nothing in either direction; the probe script is cheap and the cell is
not.

## Known gaps

From an adversarial pass over this document.  The path allowlist, the
spend cap, the nginx settings, the trust-store scope, and the
compaction/system-prompt correction are all folded in above; what
follows is what remains.

### Blocking M3

- ~~Does `permissions.deny` survive `bypassPermissions`?~~
  **Answered: no.**  It does not, and the design changed as a result —
  see [Verified against the binary](#verified-against-the-binary) and
  [The `web_search` refusal](#the-web_search-refusal).  What remains is
  the work: the refusal has to be built, and until it is, M3 stays shut.
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
not documentation*, and *acknowledge that source from this tree goes to
Anthropic*.

That last one is now **designed but unbuilt** — see
[The disclosure gate](#the-disclosure-gate), which specifies a
per-repository acknowledgment the archivist's provision gate enforces.
Two earlier shapes were considered and rejected, both worth recording
because they are the obvious ones:

- **Keyed on repository visibility** — refuse unless the target repo is
  public.  Wrong axis.  Private does not imply sensitive; a repo can be
  private to avoid *public attention* rather than to protect
  confidentiality, its contents perfectly fine to send to a vendor.  Nor
  does public imply safe.  Keying on the GitHub flag blocks legitimate
  work while catching nothing.
- **A boolean acknowledgment** — `DISCLOSURE_ACK=1`.  Survives being
  copy-pasted into the next cell, which is exactly the case that
  matters.  A flag already set cannot ask a question.

A separate, unrelated hygiene item — the consumer-plan training toggle
governs 5-year versus 30-day retention — is worth checking once and is
not what this gate is for.

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

## Success criteria

The question is not "did the agent finish."  It is **how much help did
it need, and who had to supply it.**  That framing matters because a
capable model will complete a task through a bad interface by brute
force, and an outcome-only measure records that as success.

### The grading scale

| Grade | The agent reached the right tool because… |
|---|---|
| **A** | the system prompt already told it — right tool, first try, and the first invocation did what was intended |
| **B** | a **system** nudge corrected it on the second try — the git proxy's refusal, a proxy-side rejection, a CLI wrapper's error.  Not a human |
| **C** | the **operator** had to steer it |
| **F** | the MCP tools cannot perform the task at all |

The axis is *who supplied the correction*, which is what makes the scale
actionable rather than merely descriptive.

- **F's must be ameliorated** for Claude.  A capability gap is a build
  item, not a grade.
- **C's must be minimized.**  Every C is a session that could not have
  run unattended, and unattended is the entire point.
- **B is a perfectly good resting place.**  A system that corrects
  itself deterministically is not a lesser outcome than one that never
  errs; it is usually a more durable one.

Recording is exception-only: the operator notes **C's and F's as they
occur** and nothing else.  A's and B's are the unremarkable default and
counting them would cost attention the experiment is trying to save.
Grades land in `notingithub/jail-grades.md` — untracked, because some
workspaces are private.

### Do not chase A's

The instinct on a recurring C is to strengthen the prompt: restate the
rule, add it to `CLAUDE.md`, reinforce the memory entry.  **This does not
work, and the evidence is already in hand.**

The standing preference against heredocs and page-long `sed` chains for
simple edits has a memory entry, has been reinforced repeatedly across
sessions, and still does not hold.  That is not a defect in the wording.
An LLM does not execute a rules-based checklist, and context bloat is
real — so a rule that must survive every session, at every context
depth, against every competing instruction, is a rule that will
eventually be dropped.  The same lesson arrived independently from
`permissions.deny`: a config-shaped thing that looks like a rule turned
out to enforce nothing.

**So the move on a recurring C is to convert it to a B, not to promote
it to an A.**  A deterministic gate — the git proxy's refusal is the
existing example, a `PreToolUse` hook is the available mechanism, held
in reserve until a specific C earns it — fires every time, regardless
of context budget or model attention.  It retires the gripe permanently
and for *both* models, which the hedge makes doubly valuable.

This is also where the jail changes the economics.  On the host, a gate
that refuses the operator's own tooling is an irritation.  Inside a cell
where mediation is already the deal, it is free.

### Friction is bidirectional

Host mode is **not** an all-A baseline, and the comparison is not "how
much friction does the jail add."  It is friction added versus friction
removed.

Host-mode Claude has its own recurring C's — stacking a PR onto one
already merged, needing to be told that the PR it filed is not there,
re-learning the same command.  And some host-mode friction simply
*ceases to be worth attention* in a cell: the reason to police an
over-elaborate edit on the host is fear of unexpected or unrecoverable
effects, and the grange makes those hard to produce and easy to discard.
Friction that the operator stops needing to apply is a win in the same
ledger as friction the system stops imposing.

### The bar

| | |
|---|---|
| **Claude** | B-or-better on every task.  Any F is a build item.  C's tracked and driven down. |
| **Qwen** | Graded on the same tasks and the same scale.  Not required to match — its result *is* the hedge's status report. |
| **Signal** | Claude at B-or-better while qwen still shows C's and F's is a strong result: it says the surface is sound and the gap is model capability. |
| **Scope** | Java/Kotlin, Go, and NPM workspaces.  No C++.  Ecosystems are named here; **repositories are not** — some are private, and a public design doc is not the place to enumerate them. |

That last rule is standing, not incidental: grades, and the surfaces
they indict, travel out of the untracked log into the tracked tree
freely.  Workspace names do not — including in commit messages and PR
descriptions, which is where such a thing leaks months later.

## Staging

1. **M1 — observe only.**  No mutation addon.  Attended session,
   plaintext-hop capture — **plus the path allowlist**, which is not
   deferrable: it is what makes "one pinned host" mean anything, and it
   costs a few lines.  Answers the diagnostic question and tells us what
   the harness actually reaches for.

   **Placement B was pulled forward into M1.**  The staging originally
   put the real credential in the cell here (placement A) on the grounds
   that M1 is attended and injection is M2's work.  Building the nginx
   hop at all is most of that work, and the remainder is one
   `proxy_set_header` — so the trade was five lines against every M1
   pcap being a credential disclosure if it escaped, with `.gitignore`
   as the only guard.  The design's own answer to the credential risk is
   that the capture should be credential-free *by construction, not by
   redaction discipline*; deferring the construction and relying on the
   discipline in the interim is the position that argument rejects.
   The cell now carries only `ANTHROPIC_AUTH_TOKEN=placeholder`, and
   compose-lint refuses a cell overlay whose token is anything else.
2. **M2 — mutation and the cap.**  Add the mutation addon and the spend
   tally: responses can be rewritten for fault injection, and there is a
   budget the run cannot exceed.
3. **M3 — overnight YOLO.**  `--dangerously-skip-permissions`, a
   long-running task, and the whole point of the exercise.  Gated on:
   a **working `web_search` refusal at the proxy**, since
   `permissions.deny` is measured inert in this mode;
   healthchecks, a kill switch, and a failure notification; a rehearsed
   credential rotation; a `.gitignore` covering captures; and a
   deliberate answer to the session-memory question.

## Deploying M1

Everything below is built.  Nothing below has been run end to end — the
first deploy is the test, and the observation log is the point.

**1. The certificate.**  Against the CA you already have, on the host
that will mount the result (so the `chmod 0600` takes — under MSYS it
silently does not):

```
bash lifecycle/claude-door-cert.sh assemble \
  --leaf-cert leaf.crt --leaf-key leaf.key --ca-cert ca.crt \
  --out /srv/cloister/claude-door
```

Use `mint` instead if you hold the CA key and want the script to issue
the leaf.  Either way it ends by verifying the result, and those checks
are the point: a certificate with the right CN and no `subjectAltName`
looks correct in every viewer and is refused by every client.

**2. The image.**  Set the repo variable, then let the pipeline build:

```
gh variable set CLOISTER_CA_PEM < /srv/cloister/claude-door/cloister-ca.crt
```

Without it the claude variant is **skipped** with a notice rather than
published half-configured.  The package must stay **private**
([Publishing the recipe](#publishing-the-recipe)).

**3. The abbey door.**

```
CLAUDE_DOOR_CONFIG=<repo>/docker/claude-door \
CLAUDE_LEAF_CERT=/srv/cloister/claude-door/claude-leaf.pem \
ANTHROPIC_TOKEN_FILE=/srv/cloister/anthropic-token \
docker compose -f docker/abbey.yaml -f docker/abbey-claude.yaml up -d
```

**4. The cell.**  `WORKBENCH_IMAGE` must name the **claude** variant.

```
docker compose -f docker/cell.yaml -f docker/cell-claude.yaml up -d
```

If the agent container refuses to boot, read why before changing
anything: `jail-probe` and `claude-door-probe` are fail-closed and both
say exactly what they found.  A `claude-door-probe` failure naming an
untrusted certificate is the design working — it means the name resolved
somewhere our CA did not sign.

**5. Watch the door, because that is the experiment.**

```
docker logs -f claude-proxy
```

Every refusal is logged by name.  Expect some — `/v1/messages` is the
only allowed path, and finding out what else the harness reaches for is
the diagnostic.  `/v1/messages/count_tokens` is the likely first one;
widen the allowlist against observations, not guesses.

**6. Capture only when you have a question.**

```
COMPOSE_PROFILES=capture docker compose -f docker/abbey.yaml \
  -f docker/abbey-claude.yaml up -d
docker stop claude-tap && docker rm claude-tap    # turning it off is an ACT
```

## References

- [Enterprise network configuration](https://code.claude.com/docs/en/network-config) — the allowlist table, proxy vars, `CLAUDE_CODE_CERT_STORE`, TLS-inspection support
- [Authentication](https://code.claude.com/docs/en/authentication) — credential storage, precedence, `setup-token`, `apiKeyHelper`
- [Data usage](https://code.claude.com/docs/en/data-usage) — telemetry opt-outs, WebFetch domain safety check, retention
- [Web search tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool) — server-side execution
- [Memory](https://code.claude.com/docs/en/memory) — CLAUDE.md precedence, the managed-policy path, `claudeMdExcludes`, auto memory, `/context`
- [CLI reference](https://code.claude.com/docs/en/cli-reference) — `--append-system-prompt`, `--setting-sources`, `--strict-mcp-config`, `--permission-mode`.  The page's `--append-system-prompt-file` variant is real, and `claude --help` does not list it: the page was right and the help text was wrong, which is the reverse of the usual failure.
- [claude-code LICENSE.md](https://github.com/anthropics/claude-code/blob/main/LICENSE.md) and [.devcontainer/](https://github.com/anthropics/claude-code/tree/main/.devcontainer) — all rights reserved, and Anthropic's own public containerization recipe
- [Log out of all active sessions](https://support.claude.com/en/articles/10310342-how-do-i-log-out-of-all-active-sessions) — and why it doesn't cover Claude Code tokens
- [Compromised API key](https://support.claude.com/en/articles/8384961-what-should-i-do-if-i-suspect-my-api-key-has-been-compromised)
- [grange.md](grange.md), [abbey.md](abbey.md), [git-proxy.md](git-proxy.md) — the containment this design borrows from and bends
