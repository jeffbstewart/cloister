# Cloister

A containment-first environment for local AI-assisted coding: a jailed coding
agent works directly — native tools, local git, on-board toolchains — in a
**grange**, a per-task workspace the **archivist** clones from the forge and
destroys after the task.  Version control and every remote touch are the
archivist's alone; the web is reachable only through a quarantined
**scholar**; the agent's work reaches the canonical tree solely through a
human-reviewed pull request.  The workers are modes of one Go binary, wired
into a per-project "cell" of Docker containers whose network topology, not
its prompts, enforces the rules.

The premise: an agent's permission system that lives inside the agent is an
LLM gating an LLM.  Cloister puts every consequential boundary somewhere the
model cannot reach — internal-only Docker networks, a disposable workspace,
pinned blind egress relays, a bot-untouchable default branch, token-gated
append-only history — and treats prompt text as advice, never as
enforcement.  The full rationale is in [docs/DESIGN.md](docs/DESIGN.md) and
[docs/grange.md](docs/grange.md).

## The cell

One cell per project.  The coding agent (qwen-code, driven through tmux
sessions via `workbench`) holds the grange read-write and every toolchain it
needs (Go, Rust, the JVM), and can reach exactly two MCP services, each a
single audited authority: the **archivist** (checkpoints, branches, and the
whole PR flow — publish, propose, reviews, `await_review` — as a bot
identity whose credential the agent never sees), and the **scholar**
(`research(query)`, the only web path).  The model comes from a shared GPU
inference stack.  Builds run offline — no package-registry route exists —
and no internal network routes to the internet or the host; the only
containers holding egress are blind relays pinned to `kagi.com` and the
forge.

The operator watches everything at `127.0.0.1:${STATUS_PORT}` — the cell's
entire host-visible surface: live queue state, the audit trail (every
action, mutation, search, and extract, including rejected ones), full run
logs, stored diffs, and the approvals page where gated operations wait for
a human decision.

The full runtime map — every container, network, mount, port, and the
invariants with their enforcers — is in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Quick start

The step-by-step Portainer walkthrough is
[docs/GETTING_STARTED.md](docs/GETTING_STARTED.md) (being refreshed for the
grange era — the compose header in `docker/ai-workers.yaml` is the current
variable reference).  Prebuilt images are on GHCR
(`ghcr.io/jeffbstewart/cloister-workbench`,
`ghcr.io/jeffbstewart/cloister-workers`); see THIRD_PARTY_NOTICES for what
they bundle.

1. **Deploy the inference stack once**: `docker/inference.yaml` (ollama + a
   localhost bridge; pre-stage model weights first — the hardened stack has
   no egress to pull them).
2. **Harden the repository** on the forge: the ruleset + CI check
   forge-lint verifies (`docs/grange.md`, "Locking down a project for
   grange service") — the archivist refuses to provision a repo whose
   protections don't hold.
3. **Deploy the cell**: `docker/ai-workers.yaml` with the env vars
   documented in its header (`PROJECT`, `WORKBENCH_IMAGE`, `AGENT_CACHES`,
   `STATE_TOKEN`, the archivist's endpoint table + bot token, the scholar's
   policy file, …).
4. `docker exec -it <project>-agent workbench`, provision the repository
   via the archivist, and work; sessions survive disconnects.  Warm the
   dependency cache through the airlock (`bin/update-gradle-deps.bat`) —
   the one deliberate, human-gated moment the cell touches a registry.
5. Watch the status page; review the PR when `await_review` says so.

## Layout

- `cmd/cloister-worker` — the one multi-call binary; the program name (a
  role link: scholar | archivist | state-service | agency) picks the role
  and its flag set (the wiring pattern: docs/worker-roles.md).
  `cmd/compose-lint` — topology drift guard, run by CI against the
  committed compose files.
- `internal/*` — the packages.  `docker/` — Dockerfiles + compose files.
  `etc/` — config templates.  `docs/` — design + onboarding docs.
  `bin/` — operator tools (the dependency airlock).  `scripts/` — repo
  plumbing, including the presubmit scan and the runtime containment probe.

## Build & verify (from repo root)

    go build ./...
    go test ./...
    gofmt -l .              # must be empty
    go vet ./...
    go-licenses check ./... # deny copyleft
    go run ./cmd/compose-lint docker/ai-workers.yaml
    go run ./cmd/copyright-lint

CI runs all of the above on every PR, plus a secret scan; a pre-commit hook
(`git config core.hooksPath .githooks`) runs the same scan locally.

## License

Apache-2.0 (see LICENSE).  Third-party components — Go modules and the
software bundled into the published images — are listed in
THIRD_PARTY_NOTICES.
