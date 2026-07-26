# The deep-think node — jailed macOS ollama design

Status: **design; Gate 1 (seed + serve, unjailed) shaken down on real
hardware 2026-07-25 — see "First light" below; the jail itself is not
yet implemented**.  Decisions from the 2026-07-07 design review.  This is the node the agency ([agency.md](agency.md))
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
  thrash), **`OLLAMA_KEEP_ALIVE=-1`** — never idle-unload: under the
  agency's pinned model sets nothing can evict a resident, so a timeout
  unload only re-buys a cold load (the agency preloads pinned models it
  finds cold, and its keep_alive on that load must not be undone by the
  next real request resetting the timer from this default).
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

## First light: Gate 1 shakedown (2026-07-25)

The first real seed-and-serve on the target hardware (M4 Max, 128 GiB),
native and **unjailed** — Gate 2 adds the jail.  Recorded here because
the runbook and its gotchas are the part worth keeping; the model picks
and sizes live in vivarium's `docs/MAC_SETUP.md`.

**Tooling actually needed** (all native, no container):

- Go toolchain (go.dev or brew).  `vivarium` is stdlib-only, so
  `go build ./cmd/vivarium` pulls nothing else — build from a trusted
  checkout, no signed-binary ceremony.
- Homebrew `ollama` (0.32.4 here), the native Metal server.  Its own
  `brew` caveats print the exact `OLLAMA_FLASH_ATTENTION=1
  OLLAMA_KV_CACHE_TYPE=q8_0` tuning this doc prescribes.
- The vivarium checkout's `signing/allowed_signers` as the trust
  anchor.  Confirm its fingerprints against the paper record before
  trusting the checkout — recompute from the key bytes, not the comment
  lines an attacker would also edit:
  `awk '/^[^#]/ {print $3,$4}' allowed_signers | ssh-keygen -lf -`.

**Store layout** — one root, three dirs, outside any home:

    /opt/deepthink/models    # OLLAMA_MODELS: the served store
    /opt/deepthink/staging   # vivarium seed work-dir (transient)
    /opt/deepthink/bin       # the from-source vivarium binary

**Serve** (loopback only; `MAX_LOADED_MODELS=1` for the shakedown so the
two heavies never co-reside before the wired limit is raised):

    OLLAMA_MODELS=/opt/deepthink/models OLLAMA_HOST=127.0.0.1:11434 \
    OLLAMA_FLASH_ATTENTION=1 OLLAMA_KV_CACHE_TYPE=q8_0 \
    OLLAMA_MAX_LOADED_MODELS=1 ollama serve

**Seed** (the airlock's core; runs against the LAN status server):

    OLLAMA_MODELS=/opt/deepthink/models vivarium seed \
      -source http://<nas-ip>:8091 \
      -targets t2 -models qwen3.6-27b \
      -allowed-signers /path/to/allowed_signers \
      -store /opt/deepthink/staging -ollama-bin ollama -create

Selection is a union: `-targets t2` pulls this machine's tier, and
`-models` adds named models on top (the think-fast 27B is tagged t1, so
name it to include it).  So this seeds qwen3-coder-next + qwen3.6-27b
today and skips gpt-oss (t2 but no gguf yet — gotcha 1); after the
2026-07-30 gpt-oss amendment the same command also pulls it.  (The
union replaced an earlier AND-filter that would have dropped the t1
27B when `-targets t2` was set — vivarium seed change, 2026-07-26.)

**Verify** — one hello-world per model over `/api/generate` proved both
serve and decode.  Measured cold: qwen3-coder-next ~63 tok/s (A3B MoE),
qwen3.6-27b ~22 tok/s (dense 27B) — the ~3x gap is exactly
`decode ~= bandwidth / active-bytes` on real silicon.

### Gotchas found (and fixed)

1. **gpt-oss is originals-only.**  gpt-oss-120b and -20b are preserved
   as native MXFP4 safetensors with no gguf, so ollama cannot serve
   them and `vivarium seed` skips them with a "no gguf artifact" note.
   By design: their gguf repacks were inside the 14-day acquisition
   quarantine and clear 2026-07-30, after which a manifest amendment
   adds the gguf.  The servable-today pair is therefore
   qwen3-coder-next + qwen3.6-27b.

2. **Split-gguf import needs a directory, not a shard file.**
   `ollama create` from `FROM <...-00001-of-00004.gguf>` fails with
   "split GGUF has 1 shards, expected 4" — handed one shard it gathers
   only that one.  `FROM <the shard directory>` makes ollama gather and
   merge every shard.  Fixed in vivarium `internal/seed`: point FROM at
   the shard directory when the artifact is multi-shard.  Hits every
   split model — coder-next now, and likely the gpt-oss ggufs later.

3. **A failed import must not cost the download.**  The first run
   fetched-and-verified 45 GiB, then died on gotcha 2 before persisting
   any state, so a naive retry would re-fetch all of it.  vivarium's
   seed now adopts already-staged bytes by re-hashing them locally
   (same guarantee as hashing in flight, no network) and persists
   progress even when a model fails — the retry re-hashed in seconds
   instead of re-downloading.

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
