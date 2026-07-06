# SVN → GitHub migration

Cloister is migrating from a private Subversion repository (module
`stewart.net/agent-builder`) into this repo, restructured as it lands. The
repo root becomes the Go module; containers, config, and docs get their own
homes. Each change lands as a PR, reviewed in the GitHub code-review UI —
much of this code is being read closely for the first time, so expect
substantive review on each package.

Per-PR flow: branch off `main` → migrate + clean the package (rewrite imports
`stewart.net/agent-builder/` → `github.com/jeffbstewart/cloister/`,
`go mod tidy`) → push → open PR → review → address comments → merge → next.

## Target layout

```
cloister/
├── go.mod go.sum  LICENSE NOTICE THIRD_PARTY_NOTICES
├── README.md  CLAUDE.md  .aiignore  .gitignore
├── cmd/{agent-builder, compose-lint}/
├── internal/
│   ├── runid/ workspace/ audit/ approval/ cellstate/ digest/ manifest/ runner/
│   │   composelint/ mcpserver/ scribe/ scholar/
│   ├── egress/            # core: subsystem, session, handles, ledger, providers
│   │   ├── wire/          #   guarded client + capped GET/POST + scrubber (leaf)
│   │   ├── policy/        #   policy, deny-list, SERP set (leaf)
│   │   ├── search/        #   Searcher, Hit, kagi/brave  (→ wire)
│   │   └── extract/       #   Retriever, Extracted, kagi extract (→ wire)
│   └── status/{web, sink}/    # were statusweb / statesink
├── etc/scholar-policy.example.yaml
├── docker/{agent, builder}/  docker/ai-workers.yaml  docker/inference.yaml
├── docs/DESIGN.md  docs/example-qwen.md
├── scripts/probe-scholar-egress.ps1
└── .github/workflows/{ci.yml, images.yml}
```

Renames: `cell-stack.yaml`→`docker/ai-workers.yaml`, `compose.yaml`→`docker/inference.yaml`,
`qwen/`→`docker/agent/`, `toolchains/jdk25-gradle/`→`docker/builder/`.

### egress split — cycle avoidance

`egress/wire` is the shared leaf (guarded transport, capped GET/POST, scrubber,
and `ErrResponseTooBig`) so `search`/`extract` import `wire` without importing
the core. `egress/policy` is a second leaf. The core `egress` package
(Subsystem, Session, handles, ledger, target-check sentinels, providers wiring)
imports policy, wire, search, extract, and may re-export the seam types
(`Hit`, `Extracted`, `Searcher`, `Retriever`) so `scholar` imports only
`egress`. Exact file→package placement is a review item in the egress PRs.

## PR sequence with prerequisites

"Prereqs" = PRs that must be **merged** before this PR's branch is cut, so its
CI (`go build ./...`, `go test ./...`) is green.

| #  | PR (migrates) | Prereqs (merged) |
|----|---------------|------------------|
| 1  | Scaffold: go.mod, CLAUDE.md, README stub, .aiignore, .gitignore, ci.yml, presubmit hook, this doc | — |
| 2  | `runid` | 1 |
| 3  | `workspace` | 1 |
| 4  | `egress/policy` | 1 |
| 5  | `egress/wire` | 1 |
| 6  | `composelint` | 1 |
| 7  | `audit` | 2 |
| 8  | `approval` | 2 |
| 9  | `cellstate` | 2 |
| 10 | `digest` | 2 |
| 11 | `egress/search` | 5 |
| 12 | `egress/extract` | 5 |
| 13 | `manifest` | 10 |
| 14 | `runner` | 2, 9 |
| 15 | `egress` (core) | 4, 5, 11, 12 |
| 16 | `status/web` | 7, 8, 9 |
| 17 | `status/sink` | 16 |
| 18 | `mcpserver` | 7, 10, 13, 14 |
| 19 | `scribe` | 3, 7, 8, 13 |
| 20 | `scholar` | 7, 8, 15 |
| 21 | `cmd/` (agent-builder + compose-lint) | 3, 6, 14, 15, 17, 18, 19, 20 |
| 22 | Deployment: docker/ + etc/ + renames + compose-lint CI step | 21 |
| 23 | `images.yml` (binary + images → GHCR) | 22 |
| 24 | Distilled README.md + docs/DESIGN.md + docs/example-qwen.md + scripts/ | 1 |

Note: `scribe`/`scholar` depend on the state-sink **client** only via
interfaces they define themselves — they do NOT import `status/sink`;
`cmd/` wires it.

## Gates

**Presubmit (local pre-commit + CI secret scan):** `scripts/presubmit-check.sh`
blocks IP-address literals, UUIDs, long hex strings, API-key shapes, and
credential assignments in added lines, and refuses `.env`/`*.key`/`*.pem`/
`secrets/` files outright. Known-safe values go in
`scripts/presubmit-allowlist.txt`. Enable locally once per clone:

```
git config core.hooksPath .githooks
```

**CI (`ci.yml`, every PR):** `go build ./...`, `go test ./...`, `gofmt -l`
(fail if dirty), `go vet`, `go-licenses check ./...` (deny copyleft), the
secret scan re-run server-side, and — once PR 22 lands —
`go run ./cmd/compose-lint docker/ai-workers.yaml`.

## Authoritative images (`images.yml`, PR 23)

On push to `main` and on tags: build the static Linux binary
(`CGO_ENABLED=0 GOOS=linux go build ./cmd/agent-builder`), then
`docker buildx build --push` the **builder** image (bundles the binary) and
the **agent** image to GHCR (`ghcr.io/jeffbstewart/cloister-builder`,
`…-agent`), tagged by SHA + semver; attach the binary to GitHub Releases.
amd64 to start; the buildx matrix stays ready for arm64. This retires the
private-registry batch build scripts.

## Known changes during migration

- Parameterize the private test-registry address as
  `${REGISTRY:-ghcr.io/jeffbstewart}` in `docker/ai-workers.yaml` (PR 22);
  the presubmit guards against reintroducing the literal.
- The live scholar policy stays uncommitted; only
  `etc/scholar-policy.example.yaml` ships.
- Drop the per-directory `LICENSE` from the old `builder/` tree (root go.mod +
  root LICENSE make it moot).
- Kebab-case doc filenames under `docs/`.
- Flag/fix anything surfaced during each package read-through.
