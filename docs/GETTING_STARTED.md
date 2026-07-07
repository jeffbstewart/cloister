# Getting started — Portainer, straight from this repo

How to stand up a working Cloister installation with **git-backed Portainer
stacks**: Portainer pulls the compose files from this repository, everything
machine-specific lives in stack environment variables, and taking an update
is a "Pull and redeploy" click.  You never hand-maintain a compose file, so
your deployment cannot drift from the repository.

What deploys and why is in [ARCHITECTURE.md](ARCHITECTURE.md) and
[DESIGN.md](DESIGN.md).  This is the hands-on path.

## Prerequisites

- Docker Desktop (WSL2 backend on Windows) with GPU support enabled, and an
  NVIDIA GPU with current drivers — the inference stack requests the GPU
  via a compose device reservation.
- Portainer managing that Docker engine.
- A [Kagi](https://kagi.com) API key for the scholar's search/extract.  Set
  the account's custom billing limit LOW — it is your spend backstop.
- A project to onboard, in a git working tree.  **The builder currently
  ships one toolchain: JDK 25 + Gradle — Java and Kotlin projects** (see
  [Toolchains](#toolchains-java--kotlin-today), below).

## 1. Stage model weights

The inference stack is hardened — no egress — so it cannot pull models.
You stage weights into a host directory first, using an ordinary ollama
wherever you have one (the host install is easiest):

```
# Windows example: point ollama's model store at the staging directory
setx OLLAMA_MODELS c:\ai_models     # then restart the ollama service/app
ollama pull <model>
```

Anything ollama can pull lands in that directory; the jailed `infer`
container mounts the same directory read-only and sees every staged model.

**Picking a model** is deliberately out of scope for this guide — model
quality, quantization trade-offs, and VRAM fit change monthly, and any
recommendation written here would be stale before you read it.  Use
current, external sources:

- The [ollama library](https://ollama.com/library) lists each model's
  parameter counts and the on-disk size per tag/quantization — the size
  must fit comfortably inside your GPU's VRAM alongside the context
  window.
- Community coding-model leaderboards and recent benchmark roundups tell
  you what currently punches at your VRAM class; search for them fresh
  rather than trusting a bookmarked one.
- The practical test is your own workload: stage two candidates and
  compare them on a real task from your project.

## 2. Deploy the shared inference stack

Once per machine, before any cell.

1. Portainer → **Stacks → Add stack → Repository**.
2. Name: `cloister-infer` (anything you like).
3. Repository URL: `https://github.com/jeffbstewart/cloister` — public, no
   authentication.
4. Repository reference: `refs/heads/main`.
5. Compose path: `docker/inference.yaml`.
6. Environment variables: `MODELS_DIR` = your staging directory from step 1
   (defaults to `c:/ai_models` if unset).
7. Deploy, then verify from the host — this proxy port is one of the only
   two localhost ports the whole system publishes:

```
curl http://127.0.0.1:11434/api/tags     # lists your staged models
```

The stack is long-lived: deploy once and leave it up (model loads are
expensive, and every project cell shares it).

## 3. Onboard the project

In the project repository:

- **`agent-harness.yaml`** at the root — the manifest of build/test actions
  the builder may run (the agent's entire action menu; see
  [DESIGN.md](DESIGN.md#the-manifest-contract)).
- **`QWEN.md`** at the root — PROJECT-SPECIFIC guidance for the agent
  (layout, conventions, definition of done).  Copy
  [example-qwen.md](example-qwen.md) and fill it in; the agent reads it
  through the librarian at session start.  The harness mechanics
  (tools, containment rules) are baked into the agent image and need no
  per-project copy.

Outside the project repository (host paths, never committed):

- **Scholar policy**: copy
  [`etc/scholar-policy.example.yaml`](../etc/scholar-policy.example.yaml)
  somewhere on the host and fill in every field — the policy is
  fail-closed and requires them all.

## 4. Deploy the project cell

One stack per project.  Same flow as step 2, with compose path
`docker/ai-workers.yaml`, and these stack environment variables (each
required one is guarded in the compose file, so a missing value fails the
deploy naming the variable):

| Variable | Value |
|---|---|
| `PROJECT` | short id, e.g. `myproject`; prefixes containers and volumes |
| `WORKSPACE` | host path of the project repo, e.g. `c:/projects/myproject` |
| `AGENT_IMAGE` | `cloister-agent:<qwen>-sha-<commit>` — pin from [GHCR](https://github.com/jeffbstewart?tab=packages), or `cloister-agent:latest` |
| `BUILDER_IMAGE` | `cloister-builder:sha-<commit>` or `cloister-builder:latest` |
| `OPENAI_MODEL` | the staged model the agent drives, as `ollama list` names it |
| `STATE_TOKEN` | per-project secret, e.g. a fresh GUID; lives only here |
| `STATUS_PORT` | localhost port for the status pages — unique per project |
| `KAGI_API_KEY` | the scholar's search/extract token |
| `SCHOLAR_POLICY` | host path of the policy file from step 3 |
| `TZ` (optional) | status-page timezone, default UTC |

Deploy, then open `http://127.0.0.1:<STATUS_PORT>` — the dashboard, audit
trail, run logs, diffs, and the approvals page where gated operations wait
for you.

## 5. Prime the Gradle cache (the dependency airlock)

Builds run offline; the builder has no egress.  Dependencies enter through
the **airlock** — a deliberate, human-gated moment of temporary egress,
automated by [`bin/update-gradle-deps.bat`](../bin/update-gradle-deps.bat):

```
bin\update-gradle-deps.bat <PROJECT> <workspace-path>
```

What it does, in order — worth knowing because the airlock is a security
boundary, and so you can perform the steps manually on a non-Windows host:

1. **Gate**: refuses to open if the build-affecting files
   (`agent-harness.yaml`, `*.gradle.kts`, `gradle/`, `buildSrc/`,
   `gradlew`) have uncommitted changes in the workspace.  Egress plus
   unreviewed build logic is the one dangerous combination — Gradle
   executes build scripts during configuration — so review and commit
   them first.
2. **Open**: attaches `<PROJECT>-builder` to a network with egress
   (docker's default `bridge`).
3. **Warm**: `docker exec`s `./gradlew --refresh-dependencies --no-daemon
   --init-script /etc/agent-builder/warm-deps.gradle build -x test
   warmAllDeps` — the init script is baked into the builder image (a
   platform artifact the agent cannot write) and resolves every
   configuration: test runtimes, coverage tooling, annotation processors.
4. **Close — always**, even when the warm fails:
   `docker network disconnect bridge <PROJECT>-builder`.

The cache persists in the per-project `gradle` volume, so this is
per-project and per-dependency-change, not per-session.

## 6. Work

```
docker exec -it <PROJECT>-agent qwen
```

The agent's MCP servers (builder, scribe, scholar) register automatically
on container start.  Keep the status page open; approvals for gated writes
and research queries appear there.

## Taking updates

Your stacks track this repository.  When it changes, the stack page's
**Pull and redeploy** re-fetches the compose file and recreates what
changed — that is the entire upgrade procedure.  Notes:

- Redeploying restarts containers: don't pull a cell mid-session or with
  approvals pending.  Named volumes are keyed by `PROJECT`, so history,
  staged approvals, and caches survive redeploys.
- Image updates are separate from compose updates: bump `AGENT_IMAGE` /
  `BUILDER_IMAGE` in the stack env deliberately.  Pinned tags
  (`cloister-agent:0.19.4-sha-<commit>`) never change contents underneath
  you; `latest` moves with main.

## Toolchains: Java + Kotlin today

The published builder image is a **JDK 25 + Gradle** toolchain; the cell as
shipped builds Java and Kotlin projects.  Supporting another ecosystem
(Go, C++, Rust, …) is real work, not configuration, and lands in two
parts:

- **Per-toolchain packaging**: each toolchain needs its own builder image —
  compilers and build tools baked in, the agent-builder sidecar layered on
  top, an offline-dependency warming path equivalent to the Gradle init
  script, and a cache volume layout for its ecosystem.
- **A refactor when the second toolchain arrives**: today "the builder
  image" is a single name (`cloister-builder`); a second toolchain forces
  per-toolchain image naming and publishing, and the manifest/caches
  conventions will need to generalize with it.

Until then, pointing a cell at a non-JVM project gives you a working agent,
scribe, and scholar — but no build/test actions worth having.
