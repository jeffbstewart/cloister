# Worker roles — the multi-call wiring pattern

How a worker role is wired end-to-end, and the checklist for adding one.
`cmd/cloister-worker` is the one binary of Cloister; every worker is a
role of it.  This page is descriptive of the code as it stands — when
the two disagree, the code wins and this page gets fixed.

## Role dispatch

The workers image bakes one link per role name, and the program name
(argv[0] basename, `.exe` trimmed) selects the role from the `roles`
map in `cmd/cloister-worker/main.go`.  Under the generic binary name a
LEADING `-worker-mode <role>` (space or `=` form) selects instead, and
a leading `-healthcheck` keeps the pre-multi-call container
HEALTHCHECK form working.  There is deliberately no default role, and
`healthcheck` is a pseudo-role outside the table — `-worker-mode
healthcheck` stays an unknown role.

A role's entry point is a `roleParser`:

    type roleParser func(args []string) (run func(), err error)

Parsing must be side-effect-free: it builds the role's own
`flag.FlagSet` (std `flag`, `flag.ContinueOnError`, per the global
convention — a flag from the wrong role is a startup error, never a
silent no-op), parses, and returns the deferred bootstrap.  On a parse
error `main` exits 2 (`flag.ErrHelp` exits 0); the flag set has
already printed the message.

## Anatomy of a role

Each role owns two pieces, engine and surface, deliberately split:

- **The engine package** (`internal/repo`, `internal/archive`,
  `internal/workspace`, …) knows nothing about MCP or HTTP.  It is the
  domain logic, tested against real dependencies.
- **The MCP package** (`internal/librarian`, `internal/scribe`, …)
  wraps the engine in a tool surface: a `Config` struct of
  collaborators (nil fields degrade features rather than fail), a
  `Server` holding `cfg` + `*mcp.Server`, a `New(cfg)` that calls
  `mcp.NewServer(&mcp.Implementation{Name, Version}, nil)` and
  `registerTools()`, and a `Handler()` returning a mux with `/mcp`
  (`mcp.NewStreamableHTTPHandler`) and `/healthz` (200 "ok").

Tool registration is one `s.mcp.AddTool(&mcp.Tool{Name, Description,
InputSchema: &jsonschema.Schema{...}}, s.handler)` per tool, with
small per-package schema helpers (`str(desc)`, `integer(desc)`).
Handlers have the signature

    func (s *Server) name(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error)

and decode their arguments into a locally declared anonymous struct
with json tags via the package's `decode(req, &a)` helper.

**Tool errors are results, never Go errors.**  The `error` return is
reserved for transport failures; a failed operation returns
`errResult(msg), nil` (a text result with `IsError` set) so the caller
sees a tool-level error, not a protocol error.  Result helpers
(`textResult`, `jsonResult`, `errResult`) are duplicated per package
by design — they are three lines each, and a shared helper package
would couple every surface to one result dialect.  Message prefixes in
use: `bad arguments:` (undecodable/invalid input), `denied:` (shield
denial, audited), `rejected:` (policy refusal, audited), `internal:`
(the server's own fault).

## The bootstrap file

`cmd/cloister-worker/<role>.go` holds the role's flag set and boot:

1. `fs := flag.NewFlagSet("<role>", flag.ContinueOnError)`
2. `common := registerCommon(fs, ":<port>")` — registers `-addr` (the
   role's default port) and `-healthcheck`, so `<role> -healthcheck`
   probes the right port with no per-role code.
3. Role flags, defaults from env where deployment sets them
   (`envOr("STATE_URL", "")`).  Durations are `fs.Duration`, never a
   unit-suffixed int.
4. `return common.runOrProbe(func() { run<Role>(<role>Options{...}) }), nil`
   — an options struct carries the parsed values; secrets come from
   env inside `run<Role>` (`os.Getenv("STATE_TOKEN")`), which
   `log.Fatalf`s on missing required config.
5. The run function builds the engine, wraps it in the MCP package's
   `New`, and hands off to `serveHTTP(&http.Server{Addr, Handler:
   srv.Handler()}, "<role> (…context…)")` — the shared harness in
   `serve.go` that serves until SIGTERM/SIGINT and drains for 10s.

## Healthcheck

No per-role code.  `-healthcheck` → `runOrProbe` → `probeHealthz`
does an in-process GET of `http://127.0.0.1:<port>/healthz` and exits
0/1, so the scratch image needs no curl.  Compose declares
`test: ["CMD", "/usr/local/bin/<role>", "-healthcheck"]`.

## Audit wiring (only for audited verbs)

A role that audits declares its own one-method `Auditor` interface
(satisfied by `sink.NewClient(stateURL, token)`) and gets a typed
detail vocabulary: a new `internal/audit/<role>.go` with the Decision
consts and `Detail` type, a `Kind` const in `internal/audit/audit.go`,
a `decodeDetail` case, and a row in `internal/audit/wire_test.go`.
Unaudited surfaces (the librarian's reads except denials, the
archivist's local verbs) skip all of this.

## Tests

- **Dispatch**: `cmd/cloister-worker/main_test.go` has tables every
  role joins — `TestResolveRole`, `TestEveryRoleHasAParser`,
  `TestWrongRoleFlagIsAnError` (another role's flag must fail), and
  `TestRoleParsersAcceptTheirOwnFlags` (bare + `-healthcheck` forms).
- **Surface**: in-process MCP client over
  `mcp.NewInMemoryTransports()` — connect the server side to
  `srv.mcp` (the test lives in the package), connect an
  `mcp.NewClient`, and drive `session.CallTool`, asserting on the
  text/JSON content and `IsError`.  No httptest.

## Ports

| port | role |
|---|---|
| :9200 | builder |
| :9201 | state-service |
| :9300 | scribe |
| :9400 | librarian |
| :9500 | scholar |
| :9600 | archivist |
| :9700 | corrector (planned) |
| :11434 | agency (infra stack) |

## Checklist for a new role

1. Engine package under `internal/` (if it doesn't already exist).
2. MCP package under `internal/<role>/`: Config, Server, New,
   Handler, registerTools, handlers, result helpers, tests.
3. `cmd/cloister-worker/<role>.go`: roleParser + options + run.
4. `cmd/cloister-worker/main.go`: the `roles` map, the `workerModes`
   string, the package doc block (role stanza + file list).
5. `cmd/cloister-worker/main_test.go`: the four dispatch tables.
6. `docker/workers/Dockerfile`: the role-link loop.
7. Role lists in `README.md` and `CLAUDE.md`; the role's design doc
   under `docs/`.
8. Every new `.go` file carries the Apache header with the current
   year (copyright-lint enforces it).
9. Compose service + compose-lint invariants land TOGETHER, in the PR
   that introduces the topology — never with the bare role.  No
   commit may exist where a worker is in the topology unjailed; until
   the jail PR, the role exists only for tests and local runs.
