# Toolchains — multi-ecosystem builder packaging

Status: **design decision record (2026-07-18), not yet implemented**.
Target ecosystems: the JVM toolchain we ship today (Java/Kotlin, JDK 25 +
Gradle) plus Go, C++, Rust, and Node.  One toolchain per cell, selected at
deploy time.

## Problem

The `cloister-workers` image carries exactly one built toolset — the JDK 25
+ Gradle toolchain — and every role in every cell runs that image.  Two
things are wrong with that, and they compound as ecosystems multiply:

- **Capability where containment says none.**  Exactly one of the six
  roles executes a toolchain: the builder.  Yet the scholar (which handles
  untrusted web content) carries a full JVM and Gradle — a ready-made
  post-exploitation toolkit: sockets, classloaders, a build system that
  executes scripts by design.  The agency — the process that sees every
  prompt, in the shared infra stack — inherits a JDK it will never
  execute.  The whole security story is "each worker holds minimum
  capability"; the shared toolchain is the largest standing exception.
- **One name conflates two decisions.**  `WORKERS_IMAGE` currently pins
  both "which worker code" and "which toolchain".  They cannot be pinned
  independently, and supporting a second ecosystem under one name is
  impossible without either bloating every worker further or forking the
  image per ecosystem and dragging all six roles along.

Secondary costs of the status quo, recorded because they vanish under the
split below:

- **License surface**: distributing Corretto (GPLv2 with the Classpath
  Exception) obligates the corresponding-source pointer in
  THIRD_PARTY_NOTICES for an image where five of six roles never run
  Java.
- **Patch-cadence coupling**: every base-OS or JDK CVE forces a rebuild
  and redeploy of every worker in every cell, including workers with no
  Java anywhere.
- **Operational weight**: hundreds of MB of JDK per pull, multiplied
  across hosts and cells, for containers that use none of it.

## The capability line

The split follows the capability boundary, not the ecosystem count:

- **`cloister-workers`** — the slim, toolchain-free image: the static
  `cloister-worker` binary, its role links, passwd entries, and
  mountpoints.  No JDK, no Gradle, ideally no OS userland at all — the
  binary is CGO-free static, and even the healthcheck execs our own
  binary, so a `scratch`/distroless-static base suffices (open decision
  below).  Scribe, librarian, state-service, scholar, and the infra
  stack's agency run this.
- **`cloister-builder-<ecosystem>`** — one image per toolchain (`jvm`,
  `go`, `rust`, `cpp`, `node`): the toolchain base + the same
  `cloister-worker` binary + the builder role link + the toolchain's
  platform artifacts.  Only the builder service runs one of these.

**Per-cell selection** is a stack env var: the cell compose grows
`TOOLCHAIN_IMAGE` (consumed only by the builder service) alongside
`WORKERS_IMAGE` (everyone else).  One toolchain per cell, chosen at
deploy time.

The project↔builder agreement already exists in code and stays the
enforcement point: `agent-harness.yaml` carries a required `toolchain:`
field, and the builder refuses a manifest whose toolchain does not match
the id baked into its image (internal/manifest — "toolchain %q does not
match this builder image").  The slim image deliberately ships **no**
toolchain id file, so running the builder role on it dies loudly via the
existing empty-toolchain-id check.

## The toolchain package contract

Each `cloister-builder-<ecosystem>` image provides, at fixed paths:

1. **The toolchain itself**, version-pinned (compilers, build tools).
2. **`/etc/cloister-worker/toolchain`** — the id the builder reports and
   the manifest must match (e.g. `jvm-jdk25-gradle9`, `go1.25`,
   `rust-1.88`, `cpp-clang19-cmake`, `node22`; exact strings are an open
   decision below).
3. **`/etc/cloister-worker/toolchain.yaml`** — NEW: machine-readable
   airlock metadata.  Three things:
   - the **warm command** — argv to run with temporary egress attached;
   - the **gate globs** — build-affecting files that must be committed
     before the airlock may open;
   - the **offline-enforcement env** the image bakes for normal builds.

   This generalizes `bin/update-gradle-deps.bat` into one
   toolchain-agnostic `bin/update-deps`: gate on the globs, attach the
   bridge network, exec the warm command read from the image's own
   metadata, always disconnect.  The warm logic stays a platform artifact
   baked into a read-only rootfs, exactly like `warm-deps.gradle` today —
   never agent-writable, never injectable at runtime.
4. **Cache layout under `$HOME`** (the `BUILD_HOME` bind): `~/.gradle`,
   `~/go/pkg/mod`, `~/.cargo`, `~/.npm`, `~/.conan2`.  This keeps the
   per-user, cross-project cache sharing; `BUILD_HOME` is per user, so
   caches from different toolchains coexist naturally under one home.
5. **Arbitrary-uid, read-only-rootfs compatibility** — the same jail
   profile every worker runs today.

## Per-ecosystem notes

| Ecosystem | Warm command | Warming executes project code? | Offline enforcement (baked env) |
|---|---|---|---|
| jvm (today's) | the existing `warm-deps.gradle` init script | **yes** — Gradle configuration runs build scripts; the commit-first gate stays mandatory | `--offline` in actions |
| go | `go mod download all` | no (fetch only, sumdb-verified) | `GOFLAGS=-mod=readonly`, `GOPROXY=off` |
| rust | `cargo fetch` | no (`build.rs` runs at build, not fetch) | `CARGO_NET_OFFLINE=true` |
| node | `npm ci --ignore-scripts` into the cache | **not if** `--ignore-scripts` — install scripts then run later, *offline*, during the build; never with egress open | `npm config offline=true` |
| cpp | vcpkg or conan populate into the `$HOME` cache | **yes for source builds** (conan/vcpkg compile) — gate like Gradle | toolchain-file pins; no fetch at build |

Two rows are the security payoff of the metadata design: npm's
postinstall scripts and Gradle/conan's build-time code are the "arbitrary
code while egress is open" hazard, and the gate-globs + warm-command
shape lets each ecosystem place its own line.  Warming and code execution
are separated wherever the ecosystem allows it (go, rust, node); where it
doesn't (jvm, cpp), the commit-first gate carries the weight, as it does
today.

The baked offline env is belt-and-suspenders — the builder has no egress
network in the first place — but it converts network hangs into fast,
named errors: "module not in cache" instead of a TCP timeout.

**C++ is the honest outlier.**  There is no one package manager, so the
cpp image ships compilers + cmake/ninja + ONE blessed dependency manager
(vcpkg or conan — open decision below), and projects wanting
system-package dependencies need a derived image — a per-project
`FROM cloister-builder-cpp` — as the documented escape hatch, not a
hidden path.

## Naming, tagging, CI

Follow the agent image's precedent: the toolchain version leads the tag —
`cloister-builder-go:1.25-sha-<commit>`,
`cloister-builder-jvm:25-sha-<commit>` — so a pinned pull can never
silently change toolchain, and a toolchain bump is visible in the tag,
not buried in contents.

`images.yml` builds the static binary once and fans out over a matrix:
`docker/toolchains/<ecosystem>/Dockerfile` each, plus the slim
`docker/workers`.  Cost: ~5 more image builds per main push; toolchain
layers cache well.  If it grates, path-filter toolchain builds later —
not part of this design.

## What a JVM (Java/Kotlin) cell looks like after the split

- The `builder` service runs `cloister-builder-jvm` — almost literally
  today's Dockerfile *extracted*: corretto:25, findutils (the
  gradle-wrapper xargs fix), `warm-deps.gradle`, and the `LANG=C.UTF-8`
  locale fix for Kotlin's backtick test-method filenames.  That locale
  fix is pure JVM-toolchain concern and currently rides along in every
  scholar and agency container — a small, concrete illustration of why
  the split follows the capability line.
- Every other service in the cell runs slim `cloister-workers`.  The JDK
  leaves the scribe, librarian, state, scholar, and agency entirely.
- The manifest names `toolchain: jvm-jdk25-gradle9` (the id renamed once,
  deliberately, when this lands).  The airlock keeps its exact current
  gate semantics, driven through `bin/update-deps` + metadata instead of
  a Gradle-specific script.

## Enforcement

- **Manifest handshake** (exists today): manifest `toolchain:` must equal
  the image's baked id; mismatch refuses all actions with an
  operator-actionable message.
- **compose-lint**: the builder service's image reference must use
  `${TOOLCHAIN_IMAGE...}` and every other worker's must use
  `${WORKERS_IMAGE...}` — the raw variable text is visible to the linter,
  so drift back to a toolchain-bearing shared image fails CI.
- **Slim image carries no toolchain id**, so misdeploying the builder on
  it fails at startup, not at first build.

## Non-goals (recorded, not solved)

- **Polyglot cells** (one repo, two toolchains): v1 stays one toolchain
  per cell.  The escape hatch if it ever matters is a second builder
  service with its own `TOOLCHAIN_IMAGE` and port — which would need
  per-action toolchain routing in the manifest.  Real design work,
  deferred with this note.
- **Toolchain version matrices** (jvm 21 and 25 side by side): just
  another image tag; no design needed.
- **Node here is the build toolchain**, unrelated to the node runtime the
  qwen agent image happens to ship.

## Open decisions (need a call before execution)

1. **Ecosystem id strings** — proposed: `jvm-jdk25-gradle9`, `go1.25`,
   `rust-1.88`, `cpp-clang19-cmake`, `node22`.  Ids appear in every
   project's manifest, so renames are churn: pick once.
2. **C++ dependency manager** — vcpkg or conan, exactly one blessed in
   the image.
3. **Slim image base** — `scratch` (near-zero CVE feed, pure Apache-2.0
   contents, no shell for an intruder) vs. a minimal distro (debuggable
   with `docker exec`).  Leaning `scratch`; the operator loses in-container
   shells either way of little value against a static Go binary.
