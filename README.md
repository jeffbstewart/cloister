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
authority:

```
 agent ── infernet ─────► infer        (the model; shared GPU stack)
 agent ── buildnet ─────► builder :9200  build/test actions from the manifest
 agent ── buildnet ─────► scribe  :9300  the SOLE writer of workspace source
 agent ── researchnet ──► scholar :9500  research(query) — the only web path

 builder/scribe ── statenet ─────► state :9201   append-only logs/audit/status
 scholar ──────── scholarstate ──► state          (no route to builder/scribe)
 scholar ──────── kagiegress ────► kagi-relay ── egress ──► kagi.com:443
 state ────────── statepub ──────► status relay ──► 127.0.0.1:${STATUS_PORT}
```

Every `*net` is `internal: true` — no route to the internet or the host.  The
only container holding an `egress` network is the kagi-relay, a blind socat
pipe hard-wired to `kagi.com:443`; even a fully compromised scholar can reach
nothing else, and it refuses to start if a boot-time probe finds any other
route.  All containers run non-root with `cap_drop: ALL`,
`no-new-privileges`, read-only root filesystems, and pid/memory limits.

The operator watches everything at `127.0.0.1:${STATUS_PORT}`: live queue
state, the audit trail (every action, mutation, search, and extract —
including rejected ones), full run logs, stored diffs, and the approvals
page where gated operations wait for a human decision.

## Quick start

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
4. **Warm the dependency cache** through the airlock — the one deliberate,
   human-gated moment the builder gets egress.
5. `docker exec -it <project>-agent qwen` and work; watch the status page.

## Layout

- `cmd/agent-builder` — the one binary (`-worker-mode` builder |
  state-service | scribe | scholar).  `cmd/compose-lint` — topology drift
  guard, run by CI against the committed cell file.
- `internal/*` — the packages.  `docker/` — Dockerfiles + compose files.
  `etc/` — config templates.  `docs/` — design + onboarding docs.
  `scripts/` — ops helpers, including the runtime containment probe.

## Build & verify (from repo root)

    go build ./...
    go test ./...
    gofmt -l .              # must be empty
    go vet ./...
    go-licenses check ./... # deny copyleft
    go run ./cmd/compose-lint docker/ai-workers.yaml

CI runs all of the above on every PR, plus a secret scan; a pre-commit hook
(`git config core.hooksPath .githooks`) runs the same scan locally.

## License

Apache-2.0 (see LICENSE).  Third-party components — Go modules and the
software bundled into the published images — are listed in
THIRD_PARTY_NOTICES.
