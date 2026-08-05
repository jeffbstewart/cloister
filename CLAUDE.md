# Cloister

Containment-first local AI coding environment: a jailed coding agent works
directly — native tools, local git, on-board toolchains — in a **grange**, a
per-task workspace volume the **archivist** clones from the forge and destroys
after the task.  Version control and every remote touch are the archivist's
alone; the web is reachable only through a quarantined **scholar**; the
boundary that keeps `main` clean is the forge's human-reviewed PR gate
(docs/grange.md).  Workers are modes of one Go binary, wired into per-project
"cells" (docker/cell.yaml).

## Layout
- `cmd/cloister-worker` — the one multi-call binary; the program name (a role
  link: scholar | archivist | state-service | agency) picks the role and its
  flag set, with a `-worker-mode` fallback under the generic name.  Adding or
  changing a role: follow docs/worker-roles.md (dispatch, flags, MCP surface,
  healthcheck, tests, image links).
- `cmd/workbench` — the operator's session manager inside the agent
  image: owns the workspace lifetime (provision → agent → dispose) over
  the archivist's operator MCP surface, which the agent cannot name
  (docs/workbench.md).
- `internal/*` — the packages. `cmd/compose-lint` — topology drift guard.
- `docker/` — Dockerfiles + compose. `etc/` — config templates. `docs/` — design.
  `bin/` — operator tools. `scripts/` — repo plumbing invoked by tooling (git
  hooks, the presubmit scanner). `lifecycle/` — runnable build/verify/deploy
  pipelines a human or agent invokes directly (e.g. `lifecycle/verify.sh`); this
  is the right home for such a script, not `scripts/`.

## Build & verify (from repo root)
Run the full gate set with **`bash lifecycle/verify.sh`** (stops at the first
failure).  On a Windows operator box, invoke Git Bash by full path — a bare
`bash` there resolves to `C:\Windows\System32\bash.exe`, the WSL launcher,
which has a different filesystem view and fails outright without an installed
distro:

    & "C:\Program Files\Git\bin\bash.exe" lifecycle/verify.sh

It runs, in order:
    go build ./...
    GOOS=linux go build ./...   # the deploy target; catches build-tag splits a Windows build misses
    go test ./...
    gofmt -l .              # must be empty
    go vet ./...
    go-licenses check ./... # deny copyleft (benign "non-Go code can't be inspected" asm warnings are expected, not failures)
    go run ./cmd/compose-lint docker/cell.yaml docker/abbey.yaml
    go run ./cmd/copyright-lint   # headers present + year current (policy embedded from cmd/copyright-lint/copyright.yaml)

Codify pipelines instead of retyping them: if you run the same multi-command
sequence 3+ times, add it to `lifecycle/`.

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
- The operator's host tree never enters a cell: the grange volume is the only
  workspace, held by agent + archivist alone.  compose-lint refuses host
  workspace mounts, pins grange mounts and every internal network's
  membership, and refuses the retired mediators (builder/scribe/librarian) by
  name.
- Agent-authored bytes reach the canonical tree only through a human-reviewed
  PR; the bot cannot touch the default branch.  Enforced by the forge ruleset
  (forge-lint verifies it; the archivist's provision gate re-verifies live) and
  the archivist's audited client-side refusals — the bot credential exists only
  in the archivist.
- Builds run offline: no package-registry route exists in a cell (GOPROXY=off,
  CARGO_NET_OFFLINE, cache-only Gradle).  Dependency refresh is the human
  airlock, which refuses over uncommitted build logic or a live agent session.
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
