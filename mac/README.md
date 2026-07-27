# mac/ — the jailed deep-think node

Node-side packaging for a native, **jailed** `ollama serve` on Apple
Silicon: the deep-think node from [docs/deepthink.md](../docs/deepthink.md),
which is the authority for *why*.  This tree is the *how*, and records what
was actually built and verified on real hardware (M4 Max, 128 GiB, macOS)
on 2026-07-26.

## What it delivers (verified)

A native ollama that Metal-accelerates but **cannot reach the network** and
**cannot see the filesystem** beyond what it needs:

- **No egress.**  Deny-default seatbelt sandbox; outbound is allowed only
  to loopback (ollama's scheduler talks to its own runner subprocess over
  127.0.0.1).  The jailed ollama's own phone-home attempts fail with
  `connect: operation not permitted` — telemetry, model-recommendation
  pings, and pulls are all physically blocked.
- **Filesystem contained.**  Reads system frameworks + the GPU stack + its
  own model store (**read-only** — seeding happens outside the jail, so a
  compromised ollama cannot alter the weights it serves); writes only in a
  small runtime tree and the OS temp dir; the operator's home is invisible.
- **Metal works jailed.**  GPU discovery succeeds in ~0.1 s;
  `qwen3-coder-next` decodes ~66 tok/s (CPU fallback would be ~5-10).
- **One LAN surface.**  A blind `socat` relay on `*:11434` forwards to the
  loopback-only ollama; it parses nothing.
- **No sleep while serving.**  `caffeinate -s` wraps the serve on AC.

## What is here vs. still TODO

deepthink.md's jail is four layers plus install/probe tooling.  This tree
implements the two that need no root, and the launcher that ties them
together:

| Layer | Status |
|---|---|
| 1. Seatbelt sandbox (`sandbox/ollama.sb`) | **done** |
| 4. Blind socat LAN relay | **done** (in the launcher) |
| `caffeinate -s` no-sleep | **done** (in the launcher) |
| 2. PF per-uid outbound backstop | TODO (needs sudo) |
| 3. `probe-deepthink-egress` | TODO |
| Dedicated service user | TODO (runs as the operator today) |
| `launchd` plists (auto-start) | TODO (foreground launcher today) |
| Relay inbound source pinning | TODO (socat `range=<workstation>/32` on the listen, PF inbound rule when that layer lands) |

Note on inbound: the loopback-bound ollama's DNS-rebinding guard 403s
unrecognized Host headers (which is why the workstation dials the node as
`deepthink.internal` — see docs/deepthink.md), but it ADMITS any private
IP literal — the natural form of a LAN request — so it is no defense
against a deliberate LAN client.  Such a client cannot pull models (no
egress) or alter weights (store mounted read-only), but can unload
residents (`keep_alive: 0`), occupy the single slot, and burn power;
source pinning above is the real answer.

The launcher (`bin/deepthink-serve.sh`) is a foreground, watch-the-logs
tool for bring-up and the shakedown.  The launchd/PF/probe/service-user
work is the next pass and turns this into an unattended service.

## Prerequisites

- Apple Silicon Mac, macOS with `sandbox-exec` (deprecated but functional).
- Homebrew: `brew install ollama socat`.
- A populated model store at `/opt/deepthink/models`.  Populating it —
  importing each model into the ollama store, offline and
  integrity-checked — is handled out-of-band by separate tooling and is
  out of scope for this node, which only *serves* what is already present.
- The store root, operator-owned, laid out as:

  ```
  /opt/deepthink/
    models/    OLLAMA_MODELS — the served store (read-only to the jail)
    run/       ollama's HOME (.ollama keypair, etc.) — writable
    tmp/ cache/ logs/         — writable runtime scratch + logs
    ollama.sb  the seatbelt profile (copy of sandbox/ollama.sb)
  ```

  Create it once (root owns `/opt`, so this is the one step that needs
  sudo today):

  ```sh
  sudo mkdir -p /opt/deepthink/{models,run,tmp,cache,logs}
  sudo chown -R "$(id -un)":staff /opt/deepthink
  cp mac/sandbox/ollama.sb /opt/deepthink/ollama.sb
  ```

## Run

```sh
mac/bin/deepthink-serve.sh
```

It stops anything on the ports, starts `caffeinate -s sandbox-exec -f
/opt/deepthink/ollama.sb ollama serve` on loopback `:11435`, brings up the
`socat` relay on `*:11434`, prints the LAN URL, and tails both logs.
**Ctrl-C** stops the node and releases caffeinate.

Paths (`/opt/deepthink`, `/opt/homebrew/bin/ollama`) are constants near the
top of the script; `sed` them if your layout differs.

## Concurrency: two lanes, one request at a time

The plan keeps both model lanes resident and serves one request at a time
across them.  Ollama has **no cross-model concurrency knob** — its
`OLLAMA_NUM_PARALLEL` is per-model — so the split is:

- **Node (this tree):** `OLLAMA_MAX_LOADED_MODELS=2` + `OLLAMA_KEEP_ALIVE=-1`
  hold both lanes resident and pinned; `OLLAMA_NUM_PARALLEL=1` serializes
  each.  (These are set in the launcher.)
- **Agency (the caller):** global single-flight is expressed by the
  agency's per-node `maxInFlight: 1` admission gate (cloister
  `internal/agency`), which runs one request at a time on the node
  regardless of which lane it targets and queues the rest
  interactive-ahead-of-batch.  See `etc/agency-routes.example.yaml`.

## Verify

With the node running:

```sh
# Metal came up (not CPU fallback):
grep 'inference compute' /opt/deepthink/logs/serve.log     # ...library=Metal...

# egress is blocked from inside the jail (ollama's own phone-home):
grep 'operation not permitted' /opt/deepthink/logs/serve.log

# it answers through the LAN relay:
curl -s http://127.0.0.1:11434/api/generate \
  -d '{"model":"qwen3-coder-next","prompt":"hello","stream":false}' | head -c 200
```

## Gotchas found during bring-up (so you don't rediscover them)

The seatbelt profile (`sandbox/ollama.sb`) is deny-default; getting a
Metal-accelerated, multi-process ollama to run inside it took four
non-obvious fixes, all commented in the profile:

1. **Silent `SIGABRT` at startup** — a hand-rolled deny-default profile
   aborts before logging anything if it does not `(import "system.sb")`
   (dyld cannot map the shared cache) and grant `file-map-executable` on
   the binary's tree.  Import Apple's base profile; do not enumerate dyld
   paths by hand.
2. **`getcwd failed: Operation not permitted`** — llama-server calls
   `getcwd()` at startup, which walks the cwd's *ancestors*.  Launch with
   cwd inside the sandbox-readable tree (`/opt/deepthink/run`) **and**
   allow reading the ancestor dir nodes (`/` and `/opt`).  Otherwise it
   aborts before touching the GPU — which looks like a GPU-discovery hang.
3. **GPU discovery times out → CPU fallback** — full Metal context
   creation (unlike `--list-devices`) reaches `com.apple.windowserver.active`,
   `com.apple.tccd.system`, and CFPreferences; denied, it stalls past
   ollama's 30 s watchdog.  Allow those (local services, not egress).
4. **ollama can't kill its stuck runner** — `(allow signal (target self))`
   is not enough; ollama must signal its runner children.  Allow
   `(target children)`.

The Metal/GPU ruleset itself (the specific `iokit-open` user-client
classes, `ipc-posix-shm` prefixes, and `file-issue-extension` for the
compiler XPC) is derived from
[aldur/sandboxed-ai](https://github.com/aldur/sandboxed-ai) (MIT); ollama's
differences from a single-process llama-server (fork/exec a runner,
loopback outbound to it) are the local adaptation.  See THIRD_PARTY_NOTICES.
