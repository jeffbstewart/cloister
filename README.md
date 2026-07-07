# Cloister

A containment-first environment for local AI-assisted coding: a jailed coding
agent that builds and tests through a **builder**, writes source only through
an audited **scribe**, and reaches the web only through a quarantined
**scholar** — each a mode of one Go binary, wired into a per-project "cell"
of Docker containers whose network topology, not its prompts, enforces the
rules.

The premise: an agent's permission system that lives inside the agent is an
LLM gating an LLM.  Cloister puts every consequential boundary somewhere the
model cannot reach — internal-only Docker networks, read-only mounts, a
single hard-wired egress relay, token-gated append-only history — and treats
prompt text as advice, never as enforcement.  The full rationale is in
[docs/DESIGN.md](docs/DESIGN.md).

## The cell

One cell per project.  The coding agent (qwen-code) holds a read-only
workspace and can reach exactly three MCP services, each a single audited
authority: the **builder** (build/test actions from the manifest), the
**scribe** (the sole writer of workspace source), and the **scholar**
(`research(query)`, the only web path); the model itself comes from a
shared GPU inference stack.  No internal network routes to the internet or
the host, and the one container holding egress is a blind relay hard-wired
to `kagi.com:443`.

The operator watches everything at `127.0.0.1:${STATUS_PORT}` — the cell's
entire host-visible surface: live queue state, the audit trail (every
action, mutation, search, and extract, including rejected ones), full run
logs, stored diffs, and the approvals page where gated operations wait for
a human decision.

The full runtime map — every container, network, mount, port, and the
invariants with their enforcers — is in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Quick start

The step-by-step Portainer walkthrough — git-backed stacks, model staging,
cache priming — is [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md).
Prebuilt images are on GHCR (`ghcr.io/jeffbstewart/cloister-agent`,
`ghcr.io/jeffbstewart/cloister-builder`); see THIRD_PARTY_NOTICES for what
they bundle.

1. **Deploy the inference stack once**: `docker/inference.yaml` (ollama + a
   localhost bridge; pre-stage model weights first — the hardened stack has
   no egress to pull them).
2. **Onboard a project**: add `agent-harness.yaml` (the action menu — see
   [docs/DESIGN.md](docs/DESIGN.md#the-manifest-contract)) and a `QWEN.md`
   (copy [docs/example-qwen.md](docs/example-qwen.md)) to the repo root.
3. **Deploy the cell**: `docker/ai-workers.yaml` with the env vars documented
   in its header (`PROJECT`, `WORKSPACE`, images, `STATE_TOKEN`, the scholar's
   policy file copied from `etc/scholar-policy.example.yaml`, …).
4. **Warm the dependency cache** through the airlock
   (`bin/update-gradle-deps.bat`) — the one deliberate, human-gated moment
   the builder gets egress.
5. `docker exec -it <project>-agent qwen` and work; watch the status page.

## Layout

- `cmd/agent-builder` — the one binary (`-worker-mode` builder |
  state-service | scribe | scholar).  `cmd/compose-lint` — topology drift
  guard, run by CI against the committed cell file.
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
