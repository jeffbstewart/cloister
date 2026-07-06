# Cloister

Containment-first local AI coding environment: a jailed coding agent that
builds/tests through a **builder**, writes source only through an audited
**scribe**, and reaches the web only through a quarantined **scholar** — each a
mode of one Go binary, wired into per-project "cells" (docker/ai-workers.yaml).

## Layout
- `cmd/agent-builder` — the one binary (builder | -scribe | -scholar | -state-service).
- `internal/*` — the packages. `cmd/compose-lint` — topology drift guard.
- `docker/` — Dockerfiles + compose. `etc/` — config templates. `docs/` — design.
  `scripts/` — ops helpers.

## Build & verify (from repo root)
    go build ./...
    go test ./...
    gofmt -l .              # must be empty
    go vet ./...
    go-licenses check ./... # deny copyleft
    go run ./cmd/compose-lint docker/ai-workers.yaml

## Conventions (do not regress)
- Domain IDs are structs wrapping a private string with a validating parser — no
  string aliases/coercion (see internal/runid).
- Durations are time.Duration; never a primitive with a unit in its name.
- On-disk ledgers/logs use bare epoch-second time_t; sort on load, don't trust order.
- audit.Record = a required Header envelope + at most one typed detail
  (Command/Mutation/Research/Search/Extract); build with audit.New.
- Tests for foo.go live in foo_test.go; never give a source file a test-sounding name.

## Security invariants (topology + tests, NOT prompt text)
- The scholar holds no `egress` network; its only route out is the kagi-relay,
  pinned to kagi.com. compose-lint + the boot self-check enforce this.
- Research answers must be grounded in retrieved results, structurally — never
  the model's weights.
- The audit trail is one-way glass: subsystems append, never read.
- No secrets, API keys, or home/LAN IPs in the repo — the presubmit enforces it.

## Working here
Changes land via PR; CI must be green before merge.
