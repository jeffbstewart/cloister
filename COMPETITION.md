# Cloister and the field — competitive comparison

A survey of how cloister's containment story compares to Niels Provos'
**IronCurtain** (cited as `npx @provos/ironcurtain`, v0.13.0, published
2026-07-11, Apache-2.0), plus the wider 2026 agent-sandboxing landscape.
Written 2026-07-16; version-pinned claims rot, the structural comparison
should not.

## TL;DR

IronCurtain and cloister are the two projects in this survey that start
from the same premise — **the LLM is assumed compromised; security must
not depend on model compliance** — and they arrive at oppositely-shaped
systems:

- **IronCurtain** funnels everything through one *trusted process*: a
  semantic MCP proxy holding a policy engine whose rules are compiled by
  an LLM from a plain-English "constitution."  Rich, general (email,
  calendar, GitHub, web), cloud-model-first, escalates to a human when
  rules say so.
- **Cloister** has no semantic chokepoint at all: policy *is* the
  topology.  Capabilities are network edges and mounts that either exist
  or don't; the workers are single-purpose; enforcement is the kernel
  and the compose file, checked by `compose-lint` and fail-closed boot
  probes.  Local-model-only, coding-only, no cloud credential anywhere.

Roughly: IronCurtain is a *policy interpreter* with sandboxes around it;
cloister is a *sandbox lattice* with no interpreter to subvert.  Each
covers the other's blind spot, and there are two or three ideas of his
genuinely worth stealing (fail-closed dual probes, the package
quarantine age gate, and — for the macOS question — validation that
seatbelt-based jails are a living, shippable technique).

## What IronCurtain is

A TypeScript "secure agent runtime with trusted process mediation"
(Node 22+), research-prototype-grade by its own description, with four
independent defense layers:

1. **Process/network isolation.**  Two agent modes.  *Code mode*: the
   built-in agent writes TypeScript executed in a V8 isolate with zero
   host access — the only affordances are typed stubs that emit MCP tool
   calls.  *Docker mode*: an external agent (Claude Code, Goose) runs in
   a container with `--network=none`, `--cap-drop=ALL`, non-root; its
   only channels are two bind-mounted Unix sockets (MCP proxy, MITM
   proxy).
2. **TLS-terminating MITM proxy.**  The agent's LLM API calls exit
   through a host-side proxy that terminates TLS with a private CA,
   enforces hostname *and* endpoint allowlists (`POST /v1/messages`
   only), and swaps a fake sentinel API key for the real one — the real
   credential never enters the container.  The same proxy fronts npm /
   PyPI / APT with allow/deny lists and a **2-day quarantine age gate**
   on newly published versions.  Agents can request new domains at
   runtime; adding always escalates to the human, removing is free.
3. **MCP policy engine.**  Every tool call is decided allow / deny /
   escalate by deterministic JSON rules compiled — by an LLM pipeline
   (annotate → compile → resolve lists → generate scenarios → verify &
   repair) — from a human-readable constitution.  Hardcoded structural
   invariants sit under the compiled rules: default-deny, protected
   paths, symlink-resolving path checks, unknown-argument rejection.
4. **OS-level sandboxing of the MCP servers themselves** via
   `@anthropic-ai/sandbox-runtime` (srt): bubblewrap namespaces on
   Linux, **Seatbelt profiles on macOS**, per-server network proxying.
   Credentials live only in the servers/trusted process, never the
   agent.

Around that core: a terminal multiplexer, daemon + web UI, multi-agent
workflows with state machines and human gates, a semantic memory MCP
server, and append-only JSONL audit trails with token/step/time/cost
budgets.

Its own `docs/SECURITY_CONCERNS.md` is admirably honest: the LLM policy
compiler can misinterpret a constitution; V8 isolates are not an OS
boundary; approved passthrough domains carry uninspected traffic;
escalation fatigue is real; a compromised host is out of scope.

## Head to head

| Dimension | IronCurtain v0.13.0 | Cloister |
|---|---|---|
| Problem | general personal-assistant + agent runtime | local AI *coding* environment |
| Threat model | compromised LLM; enforce at tool boundary | same premise |
| Enforcement point | one trusted process (semantic MCP proxy + policy engine) | topology: compose networks, mounts, single-purpose workers; no semantic chokepoint |
| Policy source | English constitution → LLM-compiled JSON rules | hand-written compose topology + Go worker gates; `compose-lint` + boot self-checks |
| Policy failure mode | compiler misinterprets intent → unintended permission (mitigated by default-deny + invariants) | a human misdesigns the topology → caught in PR review; no runtime interpreter to fool |
| Inference | cloud LLMs (Anthropic/Google/OpenAI) via credential-swapping MITM | local only (ollama on `infernet`; planned agency + deep-think node); no API keys exist |
| Web egress | domain allowlist, TLS-*terminating* proxy, runtime domain additions via escalation | single pinned host (`kagi.com`) through a blind socat relay, TLS end-to-end, no MITM, no runtime expansion |
| Source writes | filesystem MCP server + policy rules | scribe is the sole audited writer; the agent holds **no workspace mount at all**; reads shield-filtered via librarian |
| Supply chain | registry proxy: allow/deny lists + 2-day age quarantine, in-session installs allowed | builds run offline; dependency refresh is a deliberate human airlock, never agent-reachable |
| Escalation UX | first-class: mux/web approval flows, approval whitelisting to fight fatigue | scribe approval gates + status-page approvals; deliberately fewer prompts because fewer capabilities exist |
| Audit | append-only JSONL, budgets | append-only state service, one-way glass (subsystems can never read it), token-gated |
| Agent's own credentials | fake sentinel key swapped at proxy; real key never in container | none to protect (local inference) |
| Isolation tech | V8 isolate; Docker `--network=none`; Apple `container` per-VM on macOS 26+; srt (bubblewrap/Seatbelt) around MCP servers | Docker compose, `internal: true` networks, ro-rootfs/cap-drop/non-root everywhere; planned macOS seatbelt+PF jail for deep-think |
| Language / TCB | TypeScript + Node + npm dependency tree, Svelte UI, ~large surface | one Go binary (4 worker modes) + `alpine/socat` relays + compose; deliberately small |
| Maturity | fast-moving research prototype, broad features | narrow, invariant-driven, CI-enforced |
| License | Apache-2.0 | (this repo) |

### Where IronCurtain is genuinely ahead

- **Frontier models, safely.**  The MITM credential-swap means a fully
  compromised agent cannot exfiltrate the API key it never had.  This is
  the piece cloister sidesteps entirely by being local-only — at the
  cost of frontier-model quality.  If cloister ever wanted a cloud
  engine behind the agency, IronCurtain's fake-key/endpoint-allowlist
  proxy is the reference design.
- **Package installs mid-session.**  The registry proxy with a
  quarantine age gate lets the agent install dependencies *during* work
  with a real (if imperfect) supply-chain defense.  Cloister's airlock
  is stricter but requires a human round-trip.
- **Policy expressiveness.**  "Allow email to known team members" is a
  sentence in a constitution; in cloister that shape of rule has no home
  — capabilities are coarse (a worker has an edge or it doesn't).
- **Escalation ergonomics.**  Approval whitelisting (an approval mints a
  scoped auto-approve pattern that can upgrade escalate→allow but never
  override deny) is a thoughtful anti-fatigue design worth remembering
  if the scribe's approval holds ever grow noisy.
- **Breadth.**  Gmail/Calendar/Drive, GitHub, memory, workflows, web UI,
  Signal transport.  Cloister intentionally does none of this.

### Where cloister is structurally stronger

- **No LLM in the enforcement path.**  IronCurtain's rules are compiled
  by an LLM and verified by an LLM judge; its own docs list compiler
  misinterpretation as the top concern.  Cloister's equivalent layer is
  a YAML file linted in CI — boring, reviewable, and immune to prompt
  anything.
- **Absences instead of filters.**  IronCurtain's scholar-equivalent
  (fetch server) shares a trusted process with everything else, and an
  approved passthrough domain carries uninspected traffic.  Cloister's
  scholar has *no route* to the builder, scribe, workspace, or any host
  but `kagi.com` — exfiltration requires a topology change, not a
  policy miss.  Notable absences are the architecture.
- **The agent cannot even read freely.**  IronCurtain mediates writes
  and sensitive reads via policy; cloister's agent has no workspace
  mount at all, with shield-filtered reads (librarian) and audited
  writes (scribe) as separate workers on separate wires.
- **No cloud credential, no key-theft surface.**  The entire Layer-2
  apparatus (CA cert, MITM, sentinel keys) exists to protect secrets
  cloister simply doesn't have.
- **One-way-glass audit.**  IronCurtain's audit is append-only JSONL on
  disk near the trusted process; cloister's state service is
  network-partitioned so no subsystem — including the agent — can read
  the record of its own actions.
- **TCB size.**  A policy engine + LLM pipeline + MCP proxy + Node
  dependency tree is a lot of TypeScript standing between the agent and
  the world (their SECURITY_CONCERNS lists a compromised npm dependency
  of IronCurtain *itself* as bypassing everything).  Cloister's
  equivalent is one Go binary and socat.

### Philosophical fork: semantic mediation vs. structural absence

IronCurtain inspects *meaning* (which tool, which arguments, which
domain) and so can express fine-grained policy — and so can be wrong in
fine-grained ways: TLS termination makes it a content inspector, the
dynamic-domain feature makes egress a policy decision, and the policy
compiler makes English the source language.  Cloister refuses meaning:
relays are blind, TLS is end-to-end, egress is pinned at build time.
The cost is expressiveness; the benefit is that the security argument is
a graph-reachability proof instead of a rules audit.  These compose —
nothing stops a topology-first system from adding semantic gates *inside*
a worker (the scribe's approval holds already are one) — and that is
exactly the sane hybrid: structure for the outer walls, semantics only
where a human is in the loop.

## The macOS question: can we use his approach?

Short answer: **his macOS stack validates our deep-think design more
than it replaces it, and offers two concrete upgrades and one future
option.**

What IronCurtain actually does on macOS, in increasing strength:

1. **Docker Desktop + socat sidecar.**  VirtioFS can't bind-mount Unix
   sockets, so the agent sits on an `--internal` bridge with iptables
   DROP, and a socat sidecar forwards exactly two proxy ports.  This is
   *literally the kagi-relay pattern* — a blind single-purpose socat
   as the only path out — independently reinvented.  Convergent
   evolution is good evidence the pattern is right.
2. **Apple `container` runtime (macOS 26+, Apple silicon).**  Each
   container gets its own lightweight VM via the Virtualization
   framework: hardware-virtualization boundary instead of shared-kernel
   namespaces, `--network none` (v1.1+) restricting the VM to loopback,
   per-file Unix-socket mounts for the two proxies, and **fail-closed
   startup gates** — session aborts unless (a) the proxy sockets are
   reachable AND (b) internet probes fail.
3. **srt / Seatbelt for host-side processes.**  MCP servers run under
   `@anthropic-ai/sandbox-runtime`, which on macOS generates Seatbelt
   (`sandbox-exec`) profiles — the same deprecated-but-alive primitive
   our deep-think jail hand-writes.

Now map to cloister's macOS need, the deep-think node
([docs/deepthink.md](docs/deepthink.md)):

- **The GPU constraint rules out his layer 2 for inference.**  Apple
  `container` runs *Linux* guests in VMs; Linux guests get no Metal.
  A VM'd or containerized ollama on macOS is CPU-only — the exact
  reason deepthink.md already rejected Docker.  Note IronCurtain never
  puts GPU inference in its VMs either; it jails CPU agent workloads
  and calls *cloud* APIs for the model.  Our native-ollama +
  seatbelt + PF design remains the only architecture that keeps the
  128 GiB of unified memory useful.  No change.
- **His layer 3 corroborates our seatbelt bet.**  Anthropic ships and
  maintains Seatbelt-profile generation in srt, and IronCurtain runs
  production-ish workloads under it.  `sandbox-exec` deprecation risk
  was the reason our design added the PF backstop; that calculus now
  has industry co-signers.  We could even swap the hand-written
  `mac/sandbox/ollama.sb` for srt-generated profiles — but srt drags a
  Node runtime onto the Mac for what is, for us, one small static
  profile around one binary.  Recommendation: keep the hand-rolled
  profile, cite srt as prior art, revisit only if the profile grows.
- **Steal #1 — the dual fail-closed boot gate.**  Our design has
  `probe-deepthink-egress` as a script; IronCurtain wires the
  equivalent probe into session startup and *aborts* on failure.  We
  should do the same: the launchd service refuses to serve (relay
  stays down) unless the negative probe (no outbound from inside the
  jail) passes at boot — the scholar's fail-closed self-check,
  translated to the Mac.  Cheap, and turns a diagnostic into an
  invariant.
- **Steal #2 — the quarantine age gate idea** transfers to the model
  airlock: `bin/pull-models.sh` could refuse tags pushed to the ollama
  library within the last N days unless overridden, the same "new
  artifacts are suspect" logic as his package proxy.  Low cost, real
  supply-chain value for the one moment the Mac is unjailed.
- **Future option — cell workers on the Mac.**  If we ever want
  non-GPU cloister workers (a scholar, a librarian, relays) running
  *on* the deep-think node, Apple `container` on macOS 26+ gives
  per-VM isolation stronger than Docker Desktop's shared VM, with the
  same compose-like ergonomics.  His docs flag the residual: the
  host-only network exposes broader port-level host access than a
  two-socket sidecar, so our loopback-bind + single-relay discipline
  would still apply on top.  Parked, but the door is open.

So: adopt the boot-gate and airlock-age-gate ideas into deepthink.md's
phasing; keep native seatbelt + PF for inference; note Apple
`container` as the isolation substrate if the Mac ever hosts workers.

## Other related solutions

- **Anthropic sandbox-runtime (srt)** — open-source local sandboxing:
  bubblewrap on Linux, Seatbelt on macOS, plus a network-filtering
  proxy; no container required.  The layer IronCurtain builds on;
  Claude Code's local sandboxing uses the same machinery, with cloud
  sessions in full microVMs.  Closest thing to a standard for
  host-native agent jails.
- **OpenAI Codex CLI** — same per-OS primitives (Seatbelt on macOS,
  Landlock/seccomp on Linux) applied to a single coding agent's shell;
  policy is coarse (workspace-write, network on/off) with none of
  cloister's role separation.
- **Cloud microVM sandboxes — E2B, Daytona, Modal, Runloop,
  CodeSandbox** — Firecracker/Cloud-Hypervisor microVMs with ~150 ms
  boots, SDK-first, checkpoint/restore converging to table stakes as of
  mid-2026.  Strong isolation, but the code and often the model traffic
  live in someone else's cloud — the opposite of cloister's
  everything-stays-home premise.
- **microsandbox** — self-hosted libkrun microVMs, sub-200 ms boot, MCP
  server built in; the self-hosted middle ground between Docker
  namespaces and cloud microVMs (Linux hosts).
- **Apple `container` / Containerization framework** — Apple's own
  OCI-compatible runtime, one lightweight VM per container on Apple
  silicon; discussed above as IronCurtain's macOS 26+ backend.
- **gVisor** — user-space kernel (used inside some cloud sandboxes);
  stronger than namespaces without full VMs, but a syscall-emulation
  tax and no GPU story relevant to us.
- **devcontainer + firewall pattern** — Anthropic's reference
  devcontainer for Claude Code: container plus an iptables egress
  allowlist.  The folk baseline; cloister is roughly this idea taken to
  its logical conclusion (per-capability containers, internal-only
  networks, linted invariants).
- **Not competitors, interesting neighbors:** IronCurtain's repo
  compares itself to **RAPTOR** (Evron/Dullien et al.), **OpenAnt**
  (Knostic), and **MOAK** — autonomous vulnerability-discovery/
  exploitation pipelines.  Different problem (offense automation, not
  agent containment); notable mainly for how much sandboxing discipline
  they also needed the moment agents run untrusted artifacts.

## Sources

- [ironcurtain on GitHub](https://github.com/provos/ironcurtain) — README,
  `SANDBOXING.md`, `docs/SECURITY_CONCERNS.md`,
  `docs/designs/apple-container-runtime.md`,
  `docs/research/comparison-{raptor,openant,moak-ai}.md`
- [npm: @provos/ironcurtain](https://www.npmjs.com/package/@provos/ironcurtain)
  (v0.13.0, Apache-2.0, 2026-07-11)
- [ironcurtain.dev](https://ironcurtain.dev/) ·
  [provos.org announcement](https://www.provos.org/p/ironcurtain-secure-personal-assistant/)
- Press: [Techstrong.ai](https://techstrong.ai/features/ironcurtain-takes-a-security-first-approach-to-ai-agents-with-sandboxing-and-plain-english-policy/) ·
  [Help Net Security](https://www.helpnetsecurity.com/2026/02/27/ironcurtain-open-source-ai-agent-security/) ·
  [Kaspersky blog](https://www.kaspersky.com/blog/ironcurtain-ai-agent-security/55526/)
- Landscape: [Northflank sandbox comparison](https://northflank.com/blog/best-sandboxes-for-coding-agents) ·
  [coding-agent sandbox list (gist, 2026-05)](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e) ·
  [Ry Walker: AI agent sandboxes compared](https://rywalker.com/research/ai-agent-sandboxes)
- Cloister internals: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md),
  [docs/deepthink.md](docs/deepthink.md)
