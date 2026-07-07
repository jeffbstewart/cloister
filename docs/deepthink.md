# The deep-think node — jailed macOS ollama design

Status: **design, not yet implemented**.  Decisions from the 2026-07-07
design review.  This is the node the agency ([agency.md](agency.md))
dials over `infernet_big`: a 2024 MacBook Pro with 128 GiB of unified
memory, serving heavyweight chain-of-thought models the workstation GPU
cannot hold.

## Why native, why a macOS jail

Docker on macOS cannot pass Metal to a container — a containerized
ollama is CPU-only, which defeats the machine.  So the deep-think node
runs ollama **natively**, and the containment that compose topology
provides in a cell is rebuilt from macOS primitives: a `sandbox-exec`
seatbelt profile, a dedicated service user, a PF firewall backstop, and
`launchd` — plus a blind socat relay as the only LAN-visible surface,
the kagi-relay pattern run as sole ingress.

## Model lanes

Two lanes, matching the agency's class structure:

- **deep-think** — heavyweight chain-of-thought for review lenses and
  hard comprehension.
- **think-fast** — a mid-size reasoning model for lighter batch work,
  resident alongside.

**Picks as of 2026-07** — explicitly dated; we expect these to be
non-optimal within months, and that is fine:

- deep-think: `gpt-oss-120b` — MoE with ~5 B active parameters, native
  adjustable reasoning effort, 128k context, ~65 GiB weights.  On this
  hardware class it decodes several times faster than any dense 70 B,
  which is decisive when chain-of-thought multiplies every answer by
  thousands of thinking tokens.
- think-fast: `qwen3:32b` (hybrid thinking, ~20 GiB) — with
  `deepseek-r1:32b` as the bench-off alternative.

Considered and passed over, for the record: `deepseek-r1:70b` (dense —
real reasoning at ~10 tok/s; a 3k-token think is a five-minute wait);
`qwen2.5:72b` (no reasoning mode; note there is no `qwen3:72b` — the
Qwen3 dense line stops at 32 B); `qwq` (superseded by qwen3's thinking
mode, famously verbose).

### How to pick the next ones

The picks rot; the method doesn't.  A deep-think candidate must clear:

1. **Reasoning-capable** — a genuine thinking mode, not just a big
   instruct model.
2. **Decode speed ≈ memory bandwidth ÷ active bytes per token.**  The
   M4 Max moves ~546 GB/s; a dense 70 B at q4 (~40 GiB touched per
   token) yields ~10 tok/s, an MoE touching ~5 GiB yields 40+.  Favor
   MoE/sparse models for CoT lanes; thinking tokens make decode speed
   the user experience.
3. **Fits with its KV.**  Weights (ollama library lists per-tag sizes)
   plus 128k-context KV cache must sit inside the raised Metal wired
   budget alongside the other resident lane.
4. **Context ≥ 128k** natively, not via rope stretching that wrecks
   quality.
5. **Benches on our workload.**  Fresh community leaderboards shortlist;
   the decision comes from running both lanes' real prompts (a corrector
   lens pass, a librarian summarize) and reading the agency's last-N
   status entries for duration and tokens.  Recency matters more than
   any advice frozen in this file.

## Memory and server configuration

- **Raise the Metal wired limit**: macOS defaults reserve too much for
  the OS; `sysctl iogpu.wired_limit_mb` up to ~110000 on a 128 GiB
  machine leaves ample headroom while freeing ~45 GiB the default would
  strand.
- **`OLLAMA_FLASH_ATTENTION=1` + `OLLAMA_KV_CACHE_TYPE=q8_0`** — halves
  KV memory at negligible quality cost; this is the difference between
  64k and 128k being affordable.  A dense-70B KV at 128k is ~40 GiB in
  fp16, ~20 GiB at q8; gpt-oss's sliding-window layers are cheaper
  still.
- **`OLLAMA_CONTEXT_LENGTH=131072`**, **`OLLAMA_NUM_PARALLEL=1`** (each
  slot multiplies KV at full context; the agency queues instead),
  **`OLLAMA_MAX_LOADED_MODELS=2`** (both lanes resident, no eviction
  thrash), **`OLLAMA_KEEP_ALIVE`** long, per the cell's existing
  posture.
- **Honest limit: prefill.**  Apple Silicon prefill is compute-bound;
  feeding 100k tokens takes real minutes regardless of model.  The
  agency's caller deadlines and which-engine-served visibility keep
  that from being mysterious.

## The jail

Layered, fail-closed, mirroring the cell's posture:

1. **Seatbelt profile** (`sandbox-exec`, deny-default): file reads
   limited to the ollama binary/libraries and the model store; file
   writes limited to the model store, its temp, and a cache dir;
   `network-inbound` allowed on loopback:11434 only; **no
   network-outbound grant** — a jailed ollama physically cannot phone
   home, telemetry included.  `sandbox-exec` is deprecated-but-
   functional (Apple's own daemons still ship seatbelt profiles); the
   deprecation is why layer 2 exists.
2. **PF backstop**: the service runs as a dedicated user, and a PF
   anchor blocks all outbound for that uid.  A seatbelt regression
   changes nothing.
3. **Probe**: `probe-deepthink-egress` attempts outbound connections
   from inside the same sandbox profile and fails loudly if anything
   connects — the scholar's runtime probe, translated.

**The airlock twin.**  Model pulls need outbound https, so the node
starts unjailed for staging — as a deliberate, scripted human act, never
a service state: the pull script stops the jailed service, runs the pull
in the foreground (PF anchor temporarily released for the staging user,
not the service user), and the service relaunches jailed.  Same
philosophy, and the same ALWAYS-closes discipline, as
`bin/update-gradle-deps.bat`.

**The relay.**  Ollama binds `127.0.0.1` only.  A launchd-managed socat
listens on the LAN interface and forwards to loopback — the node's only
LAN-visible surface, and it parses nothing.  The agency dials it via an
env-provided address (no LAN addresses in the repo).

## Sleep

The launchd job wraps the serve in **`caffeinate -s`**: the machine
cannot sleep while the jailed ollama runs on AC power, and becomes an
ordinary laptop the moment the service stops.  Lid-closed operation
requires AC (add `pmset disablesleep 1` or an external display to taste
— noted, not prescribed).  Sleep-when-idle with wake-on-LAN was
considered and deferred: the agency's presence handling would tolerate
it, but it adds moving parts for a machine that is on the shelf
deliberately when this service runs.

## Packaging

Everything versioned and reviewable in a new **`mac/`** tree — the
node-side analog of `docker/`:

```
mac/
  sandbox/ollama.sb          # the seatbelt profile
  launchd/                   # plists: jailed serve (caffeinate-wrapped), relay
  bin/install-deepthink.sh   # idempotent: user, PF anchor, plists, sysctl
  bin/pull-models.sh         # the airlock twin: unjailed staging, always re-jails
  bin/probe-deepthink-egress.sh
```

## Phasing

1. `mac/` tree: profile, plists, install + pull + probe scripts; manual
   verification against the real MacBook (probe green, pull works,
   relay reachable from the workstation).
2. Models staged per the dated picks; both lanes resident; timing
   measured at target context.
3. The agency (its phase 3) learns the node: presence probes, fallback
   chains, `infernet_big` env address.
4. GETTING_STARTED gains a deep-think section; ARCHITECTURE.md's
   deep-think entries go from PLANNED to real.
