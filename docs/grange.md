# Grange — the ephemeral-workspace transformation

Status: **direction decision, design sketch**.  Decisions from the
2026-07-18 design discussion, recorded as the target for a phased
migration.  Rationale style follows [DESIGN.md](DESIGN.md); the current
runtime picture this transforms is [ARCHITECTURE.md](ARCHITECTURE.md).

A **grange** was a Cistercian abbey's outlying farm: worked directly by
the monks rather than through tenants, with the harvest returned to the
mother house.  Here it names the ephemeral workspace: a per-task
checkout cloned from the canonical repository, worked directly by the
agent, whose only lasting produce is a branch proposed for review.

## Problem

Three pressures, one root cause:

1. **Agents fight the mediators.**  Coding agents are trained on plain
   Read/Edit/shell against a mutable checkout.  In practice they
   struggle to route file access through the scribe and librarian MCP
   surfaces and builds through the builder — the ergonomic tax is paid
   every session, in capability.
2. **The boundary already moved.**  [archivist.md](archivist.md)
   decided, explicitly, that the operator's PR review on the forge "is
   the boundary where exfiltration or malice gets noticed."  Once the
   merge path is the gate, per-write mediation defends a line the
   threat model no longer relies on: the agent has always chosen what
   the builder executes, and PR review — not the scribe — is what keeps
   `main` clean.
3. **The host tree is exposed pre-review.**  Today the workspace is a
   host directory mounted into the cell.  Agent-written files land in
   the operator's own tree before any review; the operator can build
   and run them without noticing.  The scribe never defended against
   this; ephemerality does.

## Shape

- **The grange.**  A per-task Docker volume, provisioned by the
  archivist (clone from the canonical remote using its credential),
  mounted **read-write into the agent**, destroyed when the task ends.
  Nothing agent-written outlives it except commits on a published
  branch.  The host working tree is never mounted into a cell again;
  agent work reaches the operator's machine only via a reviewed fetch.
- **One fat, identity-free workbench image.**  All toolchains ride
  together — JVM, Go, Rust, C++, built from
  [toolchains.md](toolchains.md)'s bases layered stable-to-volatile —
  plus the agent CLIs on top, so build and test run natively in the
  agent's own container and **any cell can serve any project**: the
  fungibility the cellarer (below) needs.  The image carries no
  identity — bot tokens and git author identity are injected at
  provision time, per human, never baked into a layer.  CI images
  derive from the same base so works-in-my-cell drift stays dead; the
  scholar stays on the slim workers image, so the compose-lint
  invariant becomes "only the agent image carries toolchains."
  Builds stay **offline**: dependencies come from per-user
  content-addressed stores mounted read-only (see "Stores, warming,
  and the airlock" below); no registry route exists anywhere in the
  cell.
- **The agent CLI is fungible; the model door is not.**  qwen-code
  and Claude Code both ship in the workbench image, either one
  pointed at the agency's endpoint — local models only.  A cloud
  engine is a separate, undecided policy door (a pinned relay plus
  the credential-swap pattern, per COMPETITION.md); no cell reaches a
  cloud API as a side effect of CLI choice.
- **The archivist is the hinge — sized to the forge's blast radius.**
  Sole holder of the forge credential, sole author of remote
  operations (publish, propose, review reads), and now also the
  grange's provisioner.  Its design in
  [archivist.md](archivist.md) stands, with two amendments: it gains
  provision/dispose verbs for grange lifecycle, and it drops the
  assumption that a scribe writes beside it.  Against the LAN-jailed
  Gitea (below), its credential-custody role relaxes — the agent may
  hold its own scoped bot token and drive git and the forge API
  directly, leaving the archivist as the provisioning/lifecycle owner
  (or a thin provisioner, where the CLI's native git flow suffices).
  Against a world-readable forge, custody stays archivist-only.
- **The mediators retire.**  Scribe, librarian, and builder are removed
  once the grange path is proven.  A clean clone has clean provenance —
  the presubmit already guarantees the repo is secret-free — so the
  read shield's job shrinks to nothing; write audit's meaningful unit
  becomes the commit; build results were never trusted testimony once
  CI became the build of record.  The librarian retires outright — no
  out-of-tree read role survives it.  The builder's live build view on
  the status page retires with the builder; local builds are untrusted
  scratch work, and nothing replaces the view.
- **Local git is the agent's.**  Inside the agent's own container,
  hooks and config are not a privilege boundary — they execute as the
  agent, which is already the agent's privilege — so the agent runs
  git freely in the grange.  The credential and every remote verb stay
  archivist-only, and any *other* worker that touches a grange's
  `.git` keeps the archivist's hardened invocation discipline.
- **Everything else is untouched.**  The egress lattice (kagi-relay,
  agency as the sole inference door), the one-way-glass audit, and the
  compose-lint discipline are orthogonal to this change and remain the
  containment story for a compromised agent.

## Invariants (replacing the write-path invariants)

1. **Agent-authored bytes reach the canonical tree only through a
   human-reviewed PR.**  Enforced by forge configuration (below), not
   by cell-side mediation.
2. **The world-facing credential never enters any cell.**  `git push`
   is egress, and push access is an exfiltration channel — *to
   wherever the remote is readable*.  Against a LAN-jailed forge that
   audience collapses to the LAN, so a per-human, scoped local-forge
   bot token may ride in the cell: the worst case is attributed
   graffiti on protected local branches.  The credential that reaches
   the world — GitHub, the release mirror — stays operator-held,
   outside every cell, always.  A deployment whose working forge is
   world-readable (GitHub-direct) keeps the forge credential
   archivist-only.
3. **The grange is ephemeral.**  Provisioned from the remote at task
   start, destroyed at task end; a task resumes by re-cloning its
   branch, never by reviving a stale volume.
4. **Builds run offline.**  No package-registry route exists in the
   cell.  The pressure to open one will come — it is the same
   ergonomics argument that motivates this design — and must be
   refused: a registry reachable by the model is an exfiltration
   oracle even read-only.  Dependency refresh stays a human airlock
   that re-warms the caches.
5. **The agent holds no statenet edge.**  The merged agent container
   inherits the agent's wires only.  Build auditing as a subsystem
   record ends (accepted: an untrusted scratch build needs no trusted
   log); remote-operation audit continues in the archivist.
6. **Forge configuration is linted security config.**  The branch
   ruleset is now load-bearing topology that lives outside the repo;
   `forge-lint` (below) verifies it the way compose-lint verifies the
   compose files.

## Forge requirements

What any revision-control host must provide, stated forge-agnostically.
The forge is really "a remote with branch protection the agent's
identity cannot bypass" — GitHub today, Gitea if local-first ever
matters ([archivist.md](archivist.md)'s VCS-agnostic verbs anticipated
exactly this substitution).

| # | Requirement |
|---|---|
| R1 | The default branch accepts no direct pushes, no force-pushes, no deletion — changes land via PR only.  Repository admins (the operator) keep a bypass; the bypass list must contain no one and nothing else, and `forge-lint` pins it |
| R2 | Merge requires ≥1 approval from a human other than the author; the bot's approval must not satisfy the requirement; new pushes dismiss prior approvals (else: get approval, push a malicious commit, merge) |
| R3 | Merge requires the required status checks (the presubmit suite) green |
| R4 | Merge is performed by a human; the bot identity cannot merge |
| R5 | The bot credential is minimally scoped: push non-default branches, open/update PRs, read/write comments on this one repository — no settings, no admin, no other repos |
| R6 | Agent-modified CI definitions must not execute with secrets, and must not be able to weaken required checks without human sign-off |
| R7 | The protection configuration is readable via API so `forge-lint` can verify R1–R6; the lint runs with an operator credential, outside any cell |
| R8 | The bot can create/update only branches in its own namespace (`agent/**`).  The archivist refuses out-of-namespace pushes client-side (definite, free); the forge enforces it server-side where its rules can express it |

### GitHub realization

Extends [GITHUB_SETUP.md](GITHUB_SETUP.md), which already establishes
the bot account and the `main` ruleset.

- **R1–R4:** a branch ruleset on `main`: require a pull request before
  merging, required approvals ≥ 1, **dismiss stale approvals on new
  commits**, require status checks, block force pushes, restrict
  deletions.  Grant bypass to the **repository-admin role only** — the
  operator keeps an override for emergencies, and `forge-lint` asserts
  the bypass list is exactly that and nothing more (the bot is a Write
  collaborator, so it can never appear in it).  To make the bot's
  approval worthless (R2), add **require review from Code Owners**
  with a `CODEOWNERS` naming the operator as owner of `*` — then only
  the operator's approval satisfies the rule.
- **R5:** migrate the bot from its classic PAT to a **fine-grained
  PAT** scoped to the single repository with Contents: read/write and
  Pull requests: read/write, nothing else.  The bot remains a Write
  collaborator; Write cannot touch settings, rulesets, or webhooks.
- **R6:** this is GitHub's soft spot.  A push to a same-repo branch
  runs *modified* workflow files with access to repository Actions
  secrets, before any review.  Mitigations, in order: (a) **zero-secret
  CI** — the presubmit suite (build, test, vet, lints) needs no
  secrets; keep it that way as policy, and audit Actions secrets to
  empty; (b) `CODEOWNERS` on `.github/**` so workflow changes cannot
  *merge* without the operator; (c) if a secret-needing job ever
  appears, put the secret in an **environment with required
  reviewers**, never repo-level; (d) no self-hosted runners for this
  repo.  Path-level *push* blocking (push rulesets) is
  Enterprise-only — do not rely on it.
- **R8:** a second ruleset targeting **all branches except `agent/**`
  and `main`** (include all, exclude the namespace; `main` has its own
  stricter ruleset) with *restrict creations* and *restrict updates*,
  bypass again admin-role only.  The bot's fine-grained PAT cannot be
  branch-scoped, so this ruleset is the server-side confinement; the
  archivist's client-side refusal is the belt under it.
- **R7:** `forge-lint` reads the ruleset via the API (`gh api`) and
  asserts R1–R6.  Reading protection config needs repo-admin scope, so
  the lint runs on the operator's machine or a trusted scheduled job —
  never inside a cell, and never with the bot's token (which must not
  have admin).  Run it like compose-lint: on demand, in CI where the
  token allows, and as a periodic drift check.
- **Cost note:** on **private** repositories, branch
  protection/rulesets enforcement and required code-owner review
  require a paid plan (GitHub Pro for personal accounts, Team or
  Enterprise for organizations).  On Free, private-repo rules are
  configurable but not enforced.  Public repos get all of the above
  free.

### Gitea realization

Everything below is free and self-hosted (a Gitea on abbot would keep
the whole loop on-LAN — the local-first hedge).

- **R1–R4:** protected branch on `main`: disable push, required
  approvals ≥ 1, **dismiss stale approvals**, enable status checks with
  the presubmit contexts, block force-push and deletion.  For R2, use
  the **approvals whitelist** (only whitelisted reviewers' approvals
  count) containing the operator and not the bot — Gitea's direct
  equivalent of the code-owners trick, without a paywall.
- **R5:** the bot is an ordinary local user with Write on the one repo
  and a **scoped access token** (repository scope only).  It cannot
  administer the repo, so it cannot touch protection settings.
- **R6:** Gitea's **protected file patterns** on the protected branch
  give a structural guard on `.gitea/workflows/**` that GitHub only
  offers at Enterprise tier.  The sharper edge is the runner: Gitea
  Actions means a **self-hosted act_runner executing agent-authored
  code** — it must be treated as cell-grade untrusted execution
  (ephemeral container runner, no secrets, egress-restricted; a
  cloister-shaped jail, not a daemon on a trusted host).  The
  zero-secret CI policy applies identically.
- **R8:** expressible with pattern rules — a wildcard protection on
  `**` whose push whitelist is the operator alone, alongside an
  `agent/**` rule whitelisting the bot — but Gitea's behavior when
  multiple protection rules match a branch needs verification in the
  pilot before R8 is trusted to the forge side.  Until then the
  archivist's client-side refusal carries R8 alone on Gitea.
- **R7:** branch-protection settings are readable via
  `/repos/{owner}/{repo}/branch_protections`; `forge-lint` grows a
  Gitea backend behind the same assertion set.  Gitea admins bypass
  protection by default, which matches the operator-override decision;
  the lint asserts the admin set is exactly the operator.

## Sequencing

The archivist is the prerequisite for everything: it holds the
credential, and in this design it also provisions the grange.  Each
milestone lands via PR with compose-lint updated in the same change, so
the linted topology never lags the real one.

- **M0 — forge-lint + forge hardening.**  Codify R1–R7 against the
  current GitHub setup (stale-approval dismissal, CODEOWNERS,
  fine-grained PAT migration, Actions-secrets audit) and build the lint
  that asserts them.  This is load-bearing *today* — the PR gate is
  already the declared boundary — so it comes first and de-risks
  everything after.
- **M1 — archivist.**  Implement [archivist.md](archivist.md) as
  designed, plus grange lifecycle verbs: `provision(branch?)` (clone
  into a fresh per-task volume; new branch or resume an existing one)
  and `dispose()` (destroy the volume; refuse if unpublished
  checkpoints exist, overridable).
- **M2 — the grange replaces the host mount.**  Cells mount the
  per-task volume where the host directory used to be; scribe,
  librarian, and builder keep operating against it unchanged.  A
  deliberately boring swap — but the host tree stops being exposed to
  the cell here, which is the transformation's first security win,
  banked before any mediator is touched.
- **M3 — open the tree.**  Mount the grange read-write into the agent;
  the agent uses native file tools and local read-only git.  The
  scribe and librarian go idle and are then removed from the compose
  topology.  This is the milestone that resolves the qwen friction.
- **M4 — the workbench image.**  One fat, identity-free image: the
  toolchain bases as the stable lower layers, the agent CLIs on top
  (so vendor bumps don't rebuild the world), offline caches
  pre-warmed; retire the builder.  Derive CI images from the same
  base to limit works-in-my-cell drift.
- **M5 — cutover of the record.**  Rewrite the security invariants in
  CLAUDE.md and [ARCHITECTURE.md](ARCHITECTURE.md); honesty pass on
  COMPETITION.md (the "no workspace mount at all" and "cannot even
  read freely" claims are traded away deliberately — the
  structural-absence story moves from the filesystem to the network,
  the credential, and the merge path).

Parallel and optional tracks: cell consolidation and the Gitea-local
forge, sketched below.

## Cell consolidation on abbot

Granges break the cell↔project binding: a workspace is provisioned per
task from whatever repo the task names, so cells stop being per-project
fixtures and become **fungible, task-scoped instances**.  Abbot's RAM
supports 4–6 concurrently.

**Shape: slots with leases, cells as cattle.**  Not N long-lived cells
that get reassigned — a cell is *instantiated* per task and torn down
with it; the fat workbench image means any slot serves any project and
ecosystem.  What persists is a small pool of
**slots**, handed out by an allocator — proposed name: the
**cellarer**, the monastic officer in charge of the abbey's provisions:

- A slot is: a uid (the multi-user workspace plan's unit), a compose
  project name, and a grange volume namespace.  4–6 slots configured.
- A **lease** is the slot grant, kept alive by a heartbeat from the
  running cell.  Cell lifetime = task lifetime; lease expiry means the
  task died without cleanup.
- The **reaper** reclaims expired slots.  Before teardown, if the
  grange holds unpublished checkpoints, the archivist publishes them
  to an `agent/rescue-<task>` branch — reaping must never be able to
  destroy work, only local state.  This is what makes aggressive
  reaping safe: branches survive, volumes never matter.
- Shared services stay singletons: one agency, one state service, one
  set of relays.  Cells multi-tenant onto them; audit records carry
  the slot/task identity.

**Risks:**

- **Inference is the scarce resource, not RAM.**  4–6 agents against
  one local model stack means the agency needs explicit queue
  fairness — per-slot scheduling and budgets — or one runaway session
  starves the rest.  This is the real engineering cost of
  consolidation and should be designed in the agency, not bolted on.
- **Cross-cell bleed.**  Cells share abbot's kernel and Docker daemon;
  intra-host isolation is compose networks and uids.  compose-lint
  must grow per-instance assertions: no shared or fixed network names
  across cell instances, no cross-cell edges, volumes namespaced by
  slot.  A fixed name that was harmless in a one-cell world becomes a
  covert channel in a six-cell world.
- **Blast radius.**  A container escape in any cell now reaches every
  project's cells on the host.  Accepted (it was already true whenever
  two projects shared a host), but consolidation makes it the norm —
  worth stating rather than discovering.
- **One bot credential, many cells.**  All cells' archivists share the
  bot identity; attribution comes from branch names and audit records,
  and the forge's rate limits are shared.  Acceptable at 4–6; revisit
  if the pool grows.
- **Reaper vs. live work.**  The rescue-branch discipline above is
  load-bearing; a reaper that can delete an unpublished grange is a
  data-loss bug class.  The archivist's `dispose()` refusal and the
  reaper's rescue path must share one code path.

## Stores, warming, and the airlock

"Pre-warmed offline caches" hides a lifecycle question: what does
warming mean across projects and across fungible cells?  The answer
splits the nouns.

- **Workspaces** (granges) are per-task and ephemeral: create →
  operate → publish → dispose.  They never *contain* dependency
  caches; they mount them.
- **Stores** are per-user, per-ecosystem, durable, and
  content-addressed: the Go module cache, Gradle's dependency cache,
  cargo's registry.  Content addressing is why "across projects" is a
  non-problem — entries key on `module@version` + hash, so one store
  set per user serves all of that user's projects, and warming
  project B just adds entries beside project A's.  This is
  `BUILD_HOME` (per-user, cross-project) promoted to first-class.
- **Cells mount the stores read-only.**  A shared *writable* cache is
  a poisoning channel between concurrent cells (cell A rewrites a
  cached jar, cell B executes it); RO kills it and makes concurrency
  trivially safe.  The ecosystems cooperate: Go's `GOMODCACHE` is
  RO-clean (the separate `GOCACHE` stays per-cell), Gradle ships a
  first-class `GRADLE_RO_DEP_CACHE`, cargo works RO when complete
  (vendoring as fallback), and C++ dependencies bake into the
  workbench image — an airlock already passed at image build.
- **Warming is an airlock operation on the stores, not a workspace
  phase.**  A warming container with registry egress and NO agent in
  it, outside the cell topology, driven only by committed lockfiles
  (`go.sum`, Gradle verification metadata, `Cargo.lock`) — the
  existing `internal/warming` refusal-and-copy-paste ritual carries
  over unchanged.  Trigger: lockfile change, human cadence — never a
  task start.
- **Dependency review precedes dependency fetch.**  An agent branch
  that adds dependencies fails its coverage check; the human warms
  *from that branch's lockfile* — whose module paths are agent-chosen
  strings that direct network fetches.  Defenses: the warmer pins its
  registries (`GOPROXY=proxy.golang.org` and equivalents, never the
  module path's own host), the airlock ritual is a human reading the
  short, legible lockfile diff before opening, and the quarantine age
  gate (COMPETITION.md) refuses freshly published versions.  The
  agent may propose dependencies; bytes move only after a human has
  seen the names.
- **Provisioning gains a coverage check, not a warm.**  Clone → mount
  stores RO → compare the branch's lockfiles against the stores →
  fail fast with the airlock instructions, instead of letting the
  agent burn a session against a mid-build wall.
- **Build caches start cold, on purpose.**  `GOCACHE`, Gradle's build
  cache, cargo's `target/` are compiled outputs — writable by nature,
  so per-cell and ephemeral.  Each fresh cell recompiles the closure
  once; accept it, and revisit (snapshot seeding at provision) only
  if it measurably hurts — the alternative is exactly the shared
  writable cache the RO stores exist to forbid.

## Gitea-local development, GitHub as release mirror

The forge-requirements table is host-agnostic on purpose.  Running
Gitea as the *working* forge — sited on its own box, not abbot: the
canonical repo should not live on the host that runs untrusted cells,
and the forge survives abbot rebuilds — and demoting GitHub to a
*publication* target changes the system's character more than any
other option in this doc:

**Shape.**

- Gitea on abbot holds the canonical repo.  All development traffic —
  agent PRs, operator review, presubmit CI — happens on-LAN against
  it, under the R1–R8 configuration (all free in Gitea; the paid-plan
  requirement for private-repo protection dissolves).
- CI runs on a jailed **act_runner**: cell-grade untrusted execution
  (ephemeral container, no secrets, egress-restricted), since it
  builds agent-authored branches.  This jail is required work, not
  optional — required status checks (R3) are only as trustworthy as
  the runner.
- GitHub becomes a **one-way, release-gated mirror**.  Cutting a
  release is an operator act: `bin/release vX.Y.Z` tags `main` on
  Gitea, pushes `main` + the tag to GitHub with an operator-held
  credential, and drafts the GitHub release.  Nobody else holds GitHub
  push access; the bot has **no GitHub identity at all** in this
  world.  (The alternative — Gitea's built-in continuous push
  mirroring — publishes every merge immediately; release-gating is
  the better match for "versioned releases," and keeps publication a
  deliberate act.)
- Backups decouple from publication: the mirror only captures released
  states, so Gitea's data gets its own backup (nightly dump, or a
  second continuous mirror to a *private* remote), independent of the
  release cadence.

**What it buys.**

- **Push-as-egress collapses to the LAN.**  Invariant 2 called the
  forge credential an exfiltration channel; with Gitea, a malicious
  push reaches a server in the same room.  World-visibility requires
  the operator's deliberate release step.  This is the single biggest
  containment upgrade available to the grange design.
- Local-first restored: GitHub outage, policy change, or pricing no
  longer touches the development loop.
- The full R1–R8 surface without a subscription.

**What it costs.**

- Gitea joins the TCB: it holds the canonical tree, so patching,
  backup, and access control for the Gitea host become
  security-critical operator work that GitHub used to do.
- The act_runner jail and the `forge-lint` Gitea backend move from
  optional to prerequisite.
- Divergence discipline: the flow is strictly Gitea → GitHub.  The
  GitHub copy is read-only publication; granting anyone push there
  (or merging a stray GitHub PR) forks the canon.  `forge-lint` on
  the GitHub side shrinks to asserting exactly that: no collaborators,
  no open write paths.

Sequencing-wise this slots in after M1 (the archivist speaks to a
remote; which remote is configuration) and can run as a pilot behind
the same `forge-lint` assertions before any cutover.

## Open questions

- **Gitea rule precedence** — the docs answer it: when several
  protection rules match a branch, only the **first, in page order**,
  applies (rules are drag-reorderable), so R8 is `main` first,
  `agent/**` second, a `**` fallback last — and forge-lint must
  assert rule *order*, not just contents.  Remaining pilot task:
  confirm the ordering behaves live before trusting R8 server-side.
- **Agency fairness** — the scheduling/budget mechanism for 4–6
  concurrent cells needs its own design pass in
  [agency.md](agency.md).
- **Cellarer placement** — standalone worker mode, or a host-side
  operator tool?  It holds no secrets and gates no security boundary
  (slots are bookkeeping), which argues for the simplest thing that
  works.
