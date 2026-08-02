# The archivist — source-control sidecar design

Status: **M1 COMPLETE** — the archivist is built, jailed, and linted
(PR #34: `.git/**` confinement; PR #95: the hardened runner and the
local verb set in `internal/archive`; PR #96: the worker mode and its
MCP surface in `internal/archivist`; PR #98: the jail — endpoint table,
remote verbs, relays, topology, compose-lint invariants; PRs #99/#100:
the provision gate and the grange lifecycle; PR #101: `await_review`).
Decisions from the 2026-07-07 design review, amended 2026-07-29 for the
grange transformation ([grange.md](grange.md)): the archivist gains the
grange lifecycle verbs, a one-instance-one-workspace binding, and
per-endpoint egress.  "Execution sequencing" below records what landed
where.  Rationale style follows [DESIGN.md](DESIGN.md); the runtime
picture is in [ARCHITECTURE.md](ARCHITECTURE.md).

## Problem

The agent authors changes but cannot version them: no checkpoints, no
recovery story beyond "git is on the host", no way to propose work for
review or to act on review feedback.  Meanwhile the repository metadata
itself is a hazard — `.git` contents are code (hooks execute on every git
run, config names commands), and until PR #34 nothing stopped the scribe
from writing there on the agent's behalf.

## Shape

A new worker mode, the **archivist**: the cell's sole authority over
version control, the way the scribe is sole writer of source and the
librarian is sole reader.

- **Sole owner of `.git`.**  Workspace confinement rejects `.git/**`
  outright for every other worker (landed in PR #34), so the archivist can
  trust that repository metadata has exactly one writer: itself.
- **Full PR authorship, jailed-teammate model.**  The agent branches,
  checkpoints, publishes, proposes PRs, reads and replies to review
  comments, and waits for review.  The operator reviews on GitHub — that
  is the boundary where exfiltration or malice gets noticed, by explicit
  decision; the GitHub-side permissions make `main` untouchable by the
  agent's identity (see [GITHUB_SETUP.md](GITHUB_SETUP.md)).
- **VCS-agnostic verbs.**  The tool contract speaks in semantic
  operations, not git incantations, so a future subversion (or other)
  adapter can satisfy the same surface.  Git-only concepts stay out of the
  contract.
- **One instance, one workspace, one human.**  An archivist is bound at
  startup to a single workspace mount and a single endpoint table
  (credentials included).  The table holds one **human principal's** bot
  identities — that human's GitHub bot, that human's Gitea bot, one per
  endpoint — so the actor a grange speaks as is *derived* at provision
  time from (instance's human × repo's host), never chosen: there is no
  identity parameter to get wrong, and no archivist ever aggregates two
  humans' credentials.  n workspaces means n configured instances;
  rebinding a workspace to a different human is a deliberate operator
  act (recreate the instance with the other envelope), never a
  provision-time choice.  Mounting one workspace into several places
  would mean one archivist per mount — deferred until something needs
  it.  Fungibility lives one level up: any repository whose protections
  pass the provision gate can be provisioned into any instance whose
  endpoint table knows its host.

## Verbs

Local — free and **unaudited** (working-tree mechanics, not boundary
crossings):

| Verb | Meaning (git realization) |
|---|---|
| `current_state()` | branch, dirty files, ahead/behind (status) |
| `history(path\|ref)` | change log, capped (log) |
| `show_change(id)` | one change with its diff (show) |
| `file_at(ref, path)` | file contents at a revision, without touching the working tree (show ref:path) — how the corrector reads base/head context for a PR that isn't checked out |
| `pending_changes(path?)` | the uncommitted delta vs the last checkpoint, whole-tree or one file (diff) |
| `start_work(name)` | new line of work off the default branch (branch + switch) |
| `switch_work(name)` | return to an existing local line of work, or the default branch (switch); uncommitted changes ride along when git can carry them cleanly |
| `abandon_work(name, deleteRemote?)` | discard a line of work: switch to the default branch, delete the local branch (branch -D).  Refuses on the default branch or a dirty tree.  `deleteRemote` also removes the published counterpart — that half is a remote op, audited |
| `checkpoint(message, paths?)` | record the working tree — all of it, or just the named paths (commit, or commit -- paths) |
| `restore(checkpoint?, path?, all?)` | roll back: one file's local edits (restore path), one file from a checkpoint (checkout ref -- path), the whole tree to a checkpoint on the current line of work (reset --hard while unpublished; a content restore once published — see "Published history is append-only"), or every local edit (`all` — explicit, so an empty call can never be the destructive one) |
| `set_aside()` / `resume()` | park and recover uncommitted work (stash push/pop) |
| `sync_from_upstream()` | update the local default branch and replay work on it (fetch; the replay is a rebase only while unpublished — see "Published history is append-only") |

**No staging verbs.**  The index is a git realization detail, not part of
the contract (subversion has none, and staged-vs-worktree divergence is a
state class the agent can silently lose track of).  Checkpoints always
read the working tree; selective recording is `checkpoint`'s `paths`
parameter, not an `add` step.

**Published history is append-only.**  The client-side force-push
refusal (below) makes every published branch forward-only, and the
local verbs inherit the consequence rather than fighting it.  While a
branch is unpublished, history is the agent's scratchpad: `restore` may
`reset --hard` to any checkpoint and `sync_from_upstream` replays by
rebase.  The moment the branch is published, both switch realization:
`restore(checkpoint)` brings the checkpoint's *content* into the
worktree, recorded by the next checkpoint — selective revert is forward
motion, never history rewrite — and `sync_from_upstream` merges from
the default branch, because either rewind would need the force-push the
archivist refuses.  The alternative — permitting force-push inside the
agent's own `agent/**` namespace, where stale-approval dismissal
already defuses approve-then-swap — was considered and rejected: one
refusal with no exceptions is easier to audit, and a merge-commit PR
flow never needs the branch linear.

Remote — **audited, ungated** (every GitHub touch leaves a record; none
waits for approval):

| Verb | Meaning |
|---|---|
| `publish()` | push the current work branch |
| `propose(title, body)` | open (or update) the PR for the current branch |
| `check_progress(pr?)` | PR state + CI check results — defaults to the current branch's PR, takes an explicit PR number |
| `read_reviews(pr?)` | review comments and threads — same explicit-target rule; includes the PR's diff for a caller that wasn't its author |
| `reply_to_review(thread, body)` | respond on a review thread |
| `await_review(maxWait?)` | block until review activity on the agent's PR: new comments, approval, changes-requested, or merge/close |

The PR-read verbs take an explicit target rather than assuming "the
agent's PR" because the corrector ([corrector.md](corrector.md)) reviews
any PR by number — including the operator's own proposals — through this
same contract.  `file_at` exists for the same consumer: reviewing a PR
must never switch the working tree under a live session.

`await_review` completes the authorship loop: the agent publishes,
proposes, then *waits on the operator* without being told to look — a
long-poll against the GitHub API behind the relay, bounded interval and
`maxWait`, emitting MCP progress notifications while it waits (the same
pattern the scribe uses for approval holds).  The operator reviews when
they review; nobody has to announce it.

Client-side refusals, belt-and-braces under the GitHub ruleset: pushing
the default branch, force-pushing, and tag deletion are refused by the
archivist itself and audited as refusals.  A misconfigured ruleset or an
over-scoped credential must not become an incident.

## Grange lifecycle

[grange.md](grange.md)'s amendment: the archivist is also its
workspace's provisioner.  Two lifecycle verbs — audited, since they are
the workspace's boundary events, and served on their **own MCP surface**
(see "Two surfaces" below), so the agent cannot name them:

| Verb | Meaning |
|---|---|
| `provision(repo, branch?)` | populate the EMPTY workspace: resolve the repo URL against the endpoint table (unknown host: refuse), run the provision-time verification (grange.md) and refuse on failure naming the failing requirement and the lock-down runbook, clone through the endpoint's relay with its credential, create `agent/<name>` or check out the resumed branch, set the repo-local author identity from the endpoint, and write the provenance marker last |
| `dispose(force?)` | return the workspace to EMPTY.  Refuses while unpublished work exists — a dirty tree, checkpoints ahead of the published branch, or set-aside work (`set_aside`'s stash counts: parked work is the easiest thing to destroy silently); `force` overrides all three.  A workspace without the provenance marker is refused regardless of `force` |

- **Contents, not volumes.**  The archivist never touches the Docker
  control plane — a docker socket beside the credential would be a cell
  escape.  The volume is created and destroyed host-side (compose
  today, the cellarer later) and arrives mounted empty; `provision`
  fills it, `dispose` empties it.  Grange invariant 3 ("never revive a
  stale volume") holds because `provision` refuses a non-empty
  workspace and a resumed task is always a fresh clone of its branch.
- **Clone-first, promote by rename.**  The volume is the grange ROOT; the
  archivist manages `tree/` (the exported checkout) and `staging/` (the
  pre-promote clone) under it.  `provision` clones into `staging/`, reads
  the repository's own `.github/forge-lint.yaml` from that checkout and
  runs the gate against it, and only on success **atomically renames**
  `staging/` to `tree/` — so validation reads a real operator-approved
  file (no new mounted config, no drift), and a refused gate or an
  interrupted clone leaves the exported tree EMPTY, never CORRUPT.  The
  full clone is needed for the workspace regardless, so it doubles as the
  validation checkout; a sparse pre-validation is held in reserve for
  outsized repos.  Keeping `tree/` a subdirectory (not the mount point)
  is what makes the promote a rename.
- **The provenance marker** (`.git/cloister-grange`: repo, branch,
  provision time as epoch seconds) is `provision`'s last write and
  `dispose`'s precondition — the rail that makes `dispose` structurally
  unable to wipe a host tree, because a mounted host tree never carries
  the marker.  It lives inside `.git`, deliberately outside the
  worktree, so no worktree verb can touch it: `set_aside` cannot stash
  it, `restore` cannot revert it, `checkpoint` can never commit it into
  the repository, and it never appears in `pending_changes`.  Post-M3
  the agent can delete it, but that only blocks cleanup of a tree the
  agent already fully controls; the operator recycles the volume
  host-side.
- **State derives from disk, never memory** (restart-safe): an empty
  directory is EMPTY; marker + `.git` is PROVISIONED; anything else is
  CORRUPT, where every verb refuses and recovery is host-side.
- **The provision gate reuses `internal/forgelint`**: snapshot under
  the bot's own token, the R1–R8 checks, then grange.md's gate policy —
  refuse on any VIOLATION, tolerate UNVERIFIED only on the admin-only
  residue.  The dispose refusal is shaped as a reusable predicate: the
  future reaper's rescue-branch path must share it, not reimplement it.
- The stores/lockfile coverage check stays out of M1 — it needs the
  read-only store mounts, which land with the workbench image (grange
  M4).

### Two surfaces: the agent's and the operator's

The archivist serves two MCP endpoints from one process:

| Path | Client | Verbs |
|---|---|---|
| `/mcp` | the coding agent | everything within a task: branches, checkpoints, restore, the PR flow |
| `/operator/mcp` | the workbench session manager | `provision` and `dispose`, nothing else |

They are separate `mcp.Server` instances, which is the whole point.  The
lifecycle verbs are not *hidden* from the agent and not *advertised but
refused* — they are **absent from the registry its calls resolve
against**, so naming one answers "unknown tool".  A guessed name buys
nothing, which is what a description saying "operators only" or a
`listTools` filter would not give.

The reason is epistemic, not adversarial.  A workspace swapped under a
live session leaves the model reasoning from a context that describes a
repository which is no longer there — stale beliefs that still look
like evidence, with no signal that anything moved.  Making the
workspace's lifetime **the session's lifetime**, owned by the human
outside the agent, makes that unrepresentable rather than merely
discouraged; it is how grange invariant 3 stops depending on the model
choosing well.  The session manager provisions, runs the agent to
completion, and disposes — so a new workspace is always a new session,
with a context that was built for it.

This is emphatically **not** a security boundary: the session manager
and the agent share a container and a network namespace, so a
determined process can dial either path.  It bounds what the model can
*name*, which is what accidents are made of.  The boundaries that are
load-bearing stay where they were — the forge ruleset, the endpoint
allowlist, the bot credential living only in the archivist.

Both surfaces take the **same serialization lock**: provision and
dispose move the very state every agent verb reads, so an operator
dispose can never interleave with an in-flight checkpoint.  Two
registries, one server, one lock.

## Hardened git execution

The archivist drives the real git binary — but never with ambient trust:

- Hooks neutralized (`core.hooksPath` pointed at an empty directory the
  runner owns) on every invocation; the archivist never executes
  repository-supplied code.
- Global/system config disabled (`GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_CONFIG_SYSTEM=/dev/null`).  The repo-local config — agent-written
  post-M3, and an execution surface, not a preference file (filter/diff/
  merge drivers, `remote.*.uploadpack`, `core.worktree`) — is guarded by
  **allowlist refusal**, not override: the exec-capable key space is
  open-ended (driver sections are named by whoever writes the config),
  so the archivist refuses to run git at all in a workspace whose
  `.git/config` carries a key outside the benign-clone-key allowlist.
  Per-invocation `-c` overrides of the known program-naming keys remain
  as belt, along with `--git-dir`/`--work-tree` pinned per invocation
  and untrusted-input parsing of everything read back out of the
  repository (branch names, history records — NUL-framed so a crafted
  commit subject cannot forge a record).
- Remotes restricted to the endpoint allowlist (below), checked before
  git ever runs; `http.followRedirects` off on every invocation (a
  redirect is a way off the relay).  No credential helpers — the
  endpoint's token is injected per call through the environment
  (`GIT_CONFIG_COUNT`/`KEY`/`VALUE` carrying `http.extraheader`), never
  argv (world-readable in /proc) and never stored in config inside the
  workspace.  The design once said "askpass"; the workers image is
  FROM scratch with no shell to run one, and the env-config route has
  the same secrecy property with no exec at all.

The `.git` directory the archivist maintains lives in the grange
volume, never a host mount.  Until grange M3 every cell-side toucher of
it is the archivist's own hardened invocations; after M3 the agent runs
git freely inside its own container ([grange.md](grange.md), "Local git
is the agent's") and the hardened discipline binds every *other* worker
that touches a grange's `.git`, the archivist first among them.

## Endpoints, identity, and credential

Commits and PRs happen as a **bot account** (the operator reviews as
themselves; the bot cannot approve or merge).  Which bot follows from
the instance: each archivist mounts a read-only **endpoint table** —
deployment config, holding credential *paths*, never secrets — with one
entry per reachable git endpoint, all of them the instance's one human
principal's bots:

- **name** — also the relay's name: `github.com`, `gitea`.
- **canonical URL prefix** — how repositories are designated everywhere
  (`https://github.com/`, the Gitea front URL).
- **wire URL prefix** — where the bytes actually go:
  `https://github.com/` resolved to the relay by network alias, or
  plain `http://gitea:<port>/` through the gitea relay.
- **credential file** — a read-only mounted token file, one per
  endpoint.  The agent never sees it; no scribe-writable or
  librarian-readable path contains it.
- **bot identity** — commit author name/email.  The GitHub and Gitea
  bots are different actors, so identity rides with the endpoint, not
  the instance.  `provision` writes it repo-locally for interop, but
  `checkpoint` pins author and committer per invocation from the
  table — an agent that edits `.git/config` (possible post-M3) cannot
  spoof the author of an archivist checkpoint.

The table is the allowlist: a remote URL whose host has no entry is
refused before git ever runs — which structurally refuses `file://`,
ssh, and bare paths, and therefore every host repository.  The
archivist is never pointed at the operator's own tree.

**Egress is always a named relay** (the kagi-relay pattern: a blind
socat pipe, `fork` re-resolving per connection).  The archivist reaches
the two forge hosts by two different routes, because git and the Go API
client resolve names differently:

- **git → `github.com`** goes through a **two-hop** relay pair.  Git
  must dial the literal `github.com` (so its TLS handshake verifies
  github.com's real certificate end-to-end through the transparent
  pipe), which means gitegress must alias `github.com` to a relay.  But
  a single relay that both *holds* that alias and *resolves* `github.com`
  for its own socat target would resolve the name to itself — Docker's
  embedded resolver answers the alias authoritatively — and loop.  So
  the alias and the upstream resolution are split across two containers:
  `github-relay` (front) holds the `github.com` alias git dials and
  pipes to a distinctly-named `github-egress`; `github-egress` resolves
  the real `github.com` (it is on no network that aliases the name).
- **the PR verbs → `api.github.com`** need no alias at all: the Go
  client is a guarded transport that dials the api relay by its service
  name while presenting SNI `api.github.com`, so TLS still verifies the
  real API certificate and the single `github-api-relay` resolves the
  real host with no loop.  The relay's cell address is the endpoint
  table's `apiRelay`.
- **The gitea endpoint speaks plain http to the jailed instance**: its
  relay pipes to the LAN port that reaches only Gitea, never through
  the TLS front — the https alternative would expose every vhost the
  reverse proxy fronts, trading a protocol nicety for real reach.
  Canonical designations keep the Gitea front URL; a per-invocation
  `url.<wire>.insteadOf=<canonical>` maps them to the wire.  Accepted
  cost, already priced by the LAN-jail threat model: the Gitea token
  transits the LAN in cleartext on the relay→Gitea hop (worst case
  remains attributed graffiti on protected branches).  Operational
  note: the Gitea-side lockdown that restricts that port to the TLS
  front alone (docs/gitea.md, untracked) must also admit the cell
  hosts' relays before it is applied, or the archivist's route dies
  with it.

[GITHUB_SETUP.md](GITHUB_SETUP.md) is the replication recipe for the
GitHub bot: bot account, token, collaborator grant, and the branch
ruleset that keeps `main` requiring a human PR approval and green
checks.

## Topology

```
archivist:
  volumes:
    - grange:/workspace       # a dedicated volume, NEVER ${WORKSPACE} —
                              # the operator's host tree does not enter
    - endpoints.yaml (ro) + one read-only token file per endpoint
  networks:
    - buildnet     # reachable by the agent (inbound MCP, :9600)
    - statenet     # audit records for remote + lifecycle ops
    - gitegress    # to the endpoint relays ONLY (internal)

github.com / api.github.com / gitea relays:
  # kagi-relay pattern: one blind socat pipe per endpoint, named for it.
  # https relays carry a network alias equal to their hostname, so git
  # dials the real name, Docker DNS resolves it to the relay, and TLS
  # verifies end-to-end; the gitea relay pipes plain http to the jailed
  # instance's LAN port.  The https relays join the kagi-relay as the
  # only holders of `egress`; the gitea relay holds only the LAN route.
  # The archivist has no other resolver — Docker DNS knows only the
  # aliases, so the DNS side channel is gone for free.
```

compose-lint grows the matching invariants, landing in the same PR as
the topology — no commit exists where the archivist is unjailed: the
archivist's networks are exactly buildnet + statenet + gitegress;
gitegress membership is the archivist and the two relays it dials
(github-relay, github-api-relay); gitforward carries only the git
two-hop (github-relay, github-egress); every relay's socat destination
is a literal (no `${}` a deploy could repoint); the git front carries
the `github.com` alias git dials; only the egress-holding relays
(kagi-relay, github-egress, github-api-relay) hold `egress`; the
scholar's isolation is unchanged (it gains no route to the archivist or
the workspace).

The archivist is a **cell member**, instantiated with its cell — never
a fleet standing outside the cells.  The agent finds it the way it
finds the scribe today: the service name on the cell's own buildnet.
Pairing an agent with its archivist is cell membership, not discovery
machinery, and the cell instance itself carries the human/workspace
identity.

Worktree interplay is documented semantics, not magic: until the
mediators retire (grange M3) the scribe, the builder, and the archivist
all write the same tree, and after M3 the agent's native tools take
their place as the other writer.  Either way `restore`, `set_aside`,
and `sync_from_upstream` can clobber uncommitted edits — sequencing is
the agent's responsibility, and `current_state()` before destructive
verbs is the documented idiom.  Audit for remote ops rides a new typed
detail on the existing envelope (op, branch, PR number, target).

## Future backends

Subversion (or others) later means an adapter behind the same verbs:
`checkpoint` maps to `svn commit`, `history` to `svn log`,
`publish`/`propose` to whatever review flow that world offers.  The
contract deliberately never mentions refs, remotes, rebases, or staging —
those are realization details of the git adapter.

## Execution sequencing (M1 record; planned 2026-07-29, completed 2026-07-31)

0. DONE (PR #34): workspace confinement rejects `.git/**` — the scribe
   can never be the archivist's confused deputy.
1. DONE (PR #95): **`internal/archive`** — the hardened git runner
   (hooks off, config allowlist-guarded, env-scrubbed, injected clock)
   and the full local verb set, tested against real git on temp repos
   with a local bare repo as the fake remote.  No network surface, no
   MCP, no compose.
2. DONE: **Worker mode** — the `archivist` role link and flag set, the
   MCP surface for the local verbs (internal/archivist), the
   healthcheck.  Exercised through tests and local runs only;
   deliberately NO compose entry — the archivist never exists in the
   topology unjailed.  The checkpoint identity is pinned to a TODO
   placeholder until the endpoint table (step 3) supplies the real
   one; nothing deployed exercises it before then.
3. DONE: **The jail, one PR** — the per-endpoint relays
   (`github-relay`/`github-api-relay`, each carrying its hostname as a
   network alias, literal socat destinations), the internal gitegress
   network, the compose service on the dedicated grange volume, the
   endpoint table (internal/endpoint) with the remote verbs behind it
   (publish + the forge PR verbs via internal/forge), the client-side
   refusals, the remote-op audit detail (KindRemote), and the
   compose-lint invariants pinning all of it.  Topology and lint moved
   together.
4. DONE: **Grange lifecycle** — `provision`/`dispose` (internal/archive's
   `Grange`), the provenance marker, the forgelint-backed provision gate
   (internal/archivist's `ForgeGate`), and `KindLifecycle` audit records.
   The archivist boots WITHOUT a checkout now (lazy open): provision clones
   into a `staging/` sibling, reads the repo's OWN `.github/forge-lint.yaml`
   there and gates against it, then **atomic-renames** staging to the
   exported `tree/` and writes the marker last — so a refused gate or a
   crash leaves the workspace EMPTY, not CORRUPT.  The grange volume is the
   ROOT (`/grange`) holding `tree/` and `staging/`, not the checkout
   itself; compose + compose-lint moved with it.  Verbs serialize behind
   the one server lock, provision and dispose included.
5. DONE: **`await_review`** — the long-poll with progress notifications.
   Activity is measured from the moment of the call (a baseline of the
   PR's reviews and comments, so feedback already readable never
   retriggers; merge/close are absolute and always terminal), polled at
   a bounded interval under a bounded `maxWait` whose expiry is a
   successful "timeout" answer, not an error.  A few consecutive failed
   polls end the wait loudly rather than spinning to the deadline.  The
   one verb that does not hold the serialization lock for its whole run:
   it resolves its target under the lock, then waits on the captured
   forge client with the lock released, so a minutes-long wait never
   wedges the rest of the surface.  One audit record per call that
   reaches the endpoint, at the wait's end.
6. DONE: **Record cutover** — this doc's status, ARCHITECTURE.md's
   PLANNED sections (the archivist and its relays are now solid parts
   of the deployed picture), grange.md's M1 milestone.  (The CLAUDE.md
   invariant rewrite stays at grange M5.)

GitHub is M1's only forge adapter; the endpoint table and the adapter
seam are shaped for the Gitea backend, which follows once the pilot
answers its protection-read questions ([grange.md](grange.md)).
Someday: the subversion adapter proves the verb contract honest.
