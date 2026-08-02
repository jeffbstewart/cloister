# The abbey — shared doors, slim cells

Status: **direction decision (2026-08-02), landing now**.  Everything
runs on one host (abbot); the only things outside it are the forges
(GitHub, the jailed Gitea) and some models (the deep-think node, an
eventual frontier engine).  Rationale style follows
[DESIGN.md](DESIGN.md); the runtime picture is
[ARCHITECTURE.md](ARCHITECTURE.md).

## Problem

The cell was the unit of everything: its own scholar, its own kagi
relay, its own state service, its own status page.  That made sense
when a cell was a project's whole world.  It stopped making sense once
the [grange](grange.md) transformation retired the mediators and the
[cellarer](grange.md#cell-consolidation-on-abbot) plan pointed at 4–6
fungible cells on one host:

1. **Multiplied egress.**  N cells meant N containers holding
   `egress`.  The egress lattice is the containment story; it should
   have as few holders as physically possible, and per-machine is the
   floor.
2. **Fragmented budget.**  Each scholar burned against its own ledger,
   so a per-user daily cap could not exist — `dailyCap: 200` meant 200
   *per cell*, and the fleet had no number at all.
3. **Fragmented memory.**  N audit trails, N approvals pages.  The
   operator checks N places, and an agentic risk assessor over
   approvals would have N queues to read instead of one.
4. **Duplicated secrets.**  One Kagi key per cell, on disk N times.

## Shape

The **abbey** is the shared stack: the machine's doors and its memory.
Cells attach to it by network name, the way they already attach to the
agency.  A **cell** shrinks to what is genuinely per-project.

```
abbey (one per machine)                 cell (one per project)
  agency ─────── the inference door       agent ───── the workbench + grange
  infer ──────── the GPU node             archivist ─ VCS + the forge credential
  scholar ────── the research door
  kagi-relay
  github relays  the forge door (git two-hop + API)
  state ──────── memory + the approvals desk
  status ─────── the one page
```

A cell is now **two services and a volume**.

- **The scholar is a door, not a cell member.**  One research service,
  one kagi-relay, one Kagi key, one burn ledger — so a daily cap is
  finally a *fleet* number.  Its policy becomes per-machine
  (per-project divergence was never used and is not missed).
- **The state service is the fleet's memory and its permission desk.**
  One audit trail, one approvals page.  Both humans are **flat
  superusers** for now: each sees the other's records and may approve
  the other's requests.  This is the same call already made for the
  Gitea co-admin — administrative boundary, not a security one.
- **The forge relays are a door too.**  They are pinned to
  `github.com`/`api.github.com` — nothing project-specific about them,
  so N cells meant 3N identical socat pipes.  One set serves every
  archivist, and machine-wide `egress` holders drop to exactly three:
  kagi-relay, github-egress, github-api-relay.
- **Cells keep what is theirs**: the agent (its grange, its
  toolchains) and the archivist (its credential, its endpoint table,
  its per-cell grange volume).

## Networks

The abbey publishes four networks cells attach to by name —
`infernet` (already), plus `researchnet`, `statenet`, and `gitegress`.
Everything else stays abbey-internal, and `archivistnet` (agent →
archivist) never leaves the cell.  Adjacencies this creates, stated plainly
rather than discovered later:

- Every agent shares `researchnet`; every archivist shares `statenet`.
  Agents listen on nothing, so agent-to-agent reach is not a surface.
  Archivist-to-archivist reach exists on `statenet` (both are MCP
  servers) — accepted, because the archivist is deterministic Go with
  no model driving it ([jail threat model](grange.md)): the jail there
  is thoroughness, not model containment.  A cell's agent still cannot
  reach another cell's archivist — that edge is the per-cell
  `archivistnet`, and it never leaves the cell.
- Every archivist shares `gitegress` with the forge relays.  The
  relays are blind socat pipes with literal destinations, so a shared
  wire grants no reach a cell did not already have: the only thing on
  the other side is the pinned forge.
- `egress` holders drop to three for the whole machine: kagi-relay,
  github-egress, github-api-relay.

## Authentication — why a security-key touch, not an identity claim

Today the approvals page trusts whoever reaches it.  That was
tolerable when it published to `127.0.0.1`; it is thin the moment it
binds the LAN for monitoring, and it is the wrong shape for a
permission desk.  The available options and their verdicts:

- **Source IP** — unusable: the status path runs through a blind socat
  relay, which eats the client address.  Even with it, an address is
  not an authentication.
- **Password/session** — an escaped or colluding bot can replay a
  secret it observes.  Secrets are the wrong primitive here.
- **Security-key touch (WebAuthn)** — a registered hardware key,
  physically touched per decision.  **This is the one that carries a
  structural argument**: no software agent — escaped, colluding, or
  merely confused — can forge a touch.  It makes "a human decided
  this" a fact about the world rather than a claim about a network.

That argument is the reason to build it: the whole system's premise is
that the consequential boundary lives somewhere the model cannot
reach.  An approval that any process on the LAN can click does not
meet that bar; an approval that requires a physical touch does.

**Phasing.**  Topology first (this change), attribution second (the
audit envelope grows an `origin`, so records say which cell they came
from, and approvals record *which human* decided — worth doing under
flat trust, because retrofitting attribution into an unattributed
history is miserable), then WebAuthn over the approvals desk.  A
permissions model — humans in roles, scoped rights — comes only when
there is a reason; flat superusers is the honest description of two
people who trust each other.

## The risk assessor (later, and where it sits)

An agentic risk-assessment engine over pending approvals is the point
of centralizing the desk: one queue to read.  It sits **where the
operator sits** — on the publish side, reading the same surface the
browser reads — never as a subject of the audit it reads, so the
one-way glass holds.  Its output is the corrector's covenant: advice,
never a gate.  A human still touches the key.  Auto-approving a
low-risk tier is a separate door to open deliberately, later, on the
strength of the attributed history this phase starts collecting.
