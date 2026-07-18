# Cloister

Containment-first local AI coding environment: a jailed coding agent that
builds/tests through a **builder**, writes source only through an audited
**scribe**, and reaches the web only through a quarantined **scholar** — each a
mode of one Go binary, wired into per-project "cells" (docker/ai-workers.yaml).

## Layout
- `cmd/agent-builder` — the one binary (builder | scribe | scholar | librarian | state-service | agency).
- `internal/*` — the packages. `cmd/compose-lint` — topology drift guard.
- `docker/` — Dockerfiles + compose. `etc/` — config templates. `docs/` — design.
  `bin/` — operator tools. `scripts/` — repo plumbing.

## Build & verify (from repo root)
    go build ./...
    GOOS=linux go build ./...   # the deploy target; catches build-tag splits a Windows build misses
    go test ./...
    gofmt -l .              # must be empty
    go vet ./...
    go-licenses check ./... # deny copyleft
    go run ./cmd/compose-lint docker/ai-workers.yaml docker/inference.yaml
    go run ./cmd/copyright-lint   # headers present + year current (policy embedded from cmd/copyright-lint/copyright.yaml)

## Conventions (do not regress)
- Domain IDs are structs wrapping a private string with a validating parser — no
  string aliases/coercion (see internal/runid).
- Durations are time.Duration; never a primitive with a unit in its name.
- On-disk ledgers/logs use bare epoch-second time_t; sort on load, don't trust order.
- audit.Record = a required Header envelope + ONE Detail interface field
  (kind-discriminated on the wire: "kind" + nested "detail"); build with
  audit.New, set rec.Detail, read via typed accessors (rec.Mutation(), …).
- Tests for foo.go live in foo_test.go; never give a source file a test-sounding name.

## Security invariants (topology + tests, NOT prompt text)
- The scholar holds no `egress` network; its only route out is the kagi-relay,
  pinned to kagi.com. compose-lint + the boot self-check enforce this.
- All inference rides through the agency (the sole inference door): `infer` sits
  on `modelnet` alone, consumers dial http://agency:11434/v1. compose-lint
  enforces it on both compose files.
- Research answers must be grounded in retrieved results, structurally — never
  the model's weights.
- The audit trail is one-way glass: subsystems append, never read.
- No secrets, API keys, or home/LAN IPs in the repo — the presubmit enforces it.

## Working here
Changes land via PR; CI must be green before merge.
