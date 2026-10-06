# Threat model

Status: DRAFT 2026-10-05. Consolidates assumptions that were spread
across README, DESIGN, ARCHITECTURE, grange, archivist, abbey,
git-proxy, JAILED_CLAUDE and heretic into one place, and adds the half
that those docs state only in passing: **exfiltration**. Nothing here
changes a control; where this document and a control disagree, the
control is the fact and this document is the bug.

## 1. The two halves

Cloister gives a coding agent a free hand with a branch of sensitive
source: a full shell, native file tools, local git, every toolchain, and
any bytes it likes in its workspace. That freedom is deliberate (agents
fight mediated file access; see grange.md). It creates two obligations,
and the design is the pair of them:

1. **Integrity.** Nothing the agent writes becomes canonical, runs on the
   operator's machine, or reaches the world, except through a human
   review that reads every byte.
2. **Confidentiality.** Nothing the agent *learns* from the source leaves
   the cell except through a channel a human reads whole, or a channel
   that has been declared, by the operator, to be acceptable
   disclosure.

The first half is the one the existing docs argue at length. The second
is the one a prompt-injected agent will actually attempt, because it is
the one that pays: a model with the source in context, and an
attacker-controlled instruction to summarize it somewhere readable.

## 2. Adversary

**The agent is assumed compromised.** Not malicious by construction —
prompt-injected, colluding, misaligned, or merely confused. The controls
do not depend on which, and must not: a control that works only against
a confused agent is a usability feature, not a security one.

How it gets compromised (vectors into the context window):

- **Web content** retrieved through the scholar. The deepest risk
  (DESIGN.md): attacker-influenceable text landing in the same context
  that can write a diff.
- **The source tree itself.** A poisoned comment, test fixture, README,
  or commit message in the branch the agent was handed. This vector
  needs no network: an inbound PR from a stranger, a vendored
  dependency, or a file the previous task's agent left behind.
- **Dependencies** admitted through the airlock: READMEs, docs, build
  scripts, and the code the model reads when it studies a library.
- **The model weights.** Untrusted; assumed capable of behaving as if
  injected without any injection.

What the compromised agent can do (capabilities, by construction):

- Read every byte of the grange, including history reachable from the
  checked-out branch.
- Write any bytes into the grange, including build logic, hooks, and
  dotfiles.
- Execute anything the agent image can run, as the agent uid, for as
  long as the task lasts.
- Ask the archivist to push to `agent/**` and to open or update pull
  requests, with any content it chooses for commits, branch names,
  titles and descriptions.
- Ask the scholar to run a web query, subject to operator approval of
  the query text.
- Send inference requests to the agency.
- Attempt to persist across tasks within a cell (the `~/.bashrc` class
  of hole; see ARCHITECTURE.md).

What it cannot do, and why this is a fact rather than a rule, is the
subject of section 5.

**The outside attacker** is whoever can influence one of the vectors:
a web page the scholar will fetch, a package the airlock will admit, a
pull request or file in the repository, or the model itself. They are
assumed patient (one task's leak is enough), literate in the agent's
tooling, and able to read anything that is world-readable — including
`agent/**` branches on a public remote.

## 3. Assets, in priority order

| # | Asset | Why it is on the list |
|---|---|---|
| A1 | **Source confidentiality** — the contents of the branch, its history, and anything derivable from them | The agent has it in full; the attacker wants it; a single successful write to any readable place loses it permanently. |
| A2 | **Canonical tree** (`main`) | Agent-authored bytes become authoritative only via human-reviewed PR. |
| A3 | **Forge credential** | Push access is egress. Whoever holds it can write to wherever the remote is readable. |
| A4 | **Operator's host and home directory** | The agent's bytes must not land on the operator's machine unreviewed; the operator's credentials must never be reachable from a cell. |
| A5 | **Network and registries** | Every route out is an exfiltration channel; a registry is "an exfiltration oracle even read-only" (grange.md). |
| A6 | **Audit trail** | The record of what the agent did must be unreadable and unalterable by the agent. |
| A7 | **Other cells** | Project A's agent must not read, poison, or signal project B's. |
| A8 | **Inference host and weights** | The model server must not phone home; weights are served read-only. |
| A9 | **Authenticity of human approval** | "A human decided this" must be a fact about the world, not a claim a process can make (abbey.md; WebAuthn planned). |

A1 is first because it is the asset the existing docs treat as implied.
Everything in section 4 is organized around it.

## 4. Exfiltration: every byte path out of a cell

Confidentiality is a question of enumeration. The agent has the source
in context; the question is where context can go. Each path below is
either **absent** (no route exists), **pinned** (a route exists to one
fixed, non-attacker-readable destination), **human-read** (a route
exists and a human reads its entire content before it crosses), or
**disclosed** (the operator has decided the destination may hold the
bytes). Anything not in one of those four states is a finding.

| Path | State | Control | Residual |
|---|---|---|---|
| Direct internet egress from the agent | absent | No `egress` network on the agent; compose-lint pins memberships | Container escape (accepted, section 6) |
| DNS | absent | `dns: 127.0.0.1` on every internal service; lint rule; scholar boot probe | — |
| Package registry (even read-only: names and query order are a channel) | absent during tasks | No registry route in a cell; airlock opens only with no live session and committed build logic, in a container with no agent | The human sees names, not the reason the agent chose them |
| Scholar web query | human-read | The operator approves the query text — "the entire exfiltration budget, read whole" (DESIGN.md); scholar has no workspace | Low-bandwidth leak through query wording the human does not recognize as source-derived; attenuated, not eliminated |
| Scholar retrieval (raw URLs) | human-read | Model-constructed URLs need per-retrieval approval | Redirects Kagi follows server-side |
| **`git push` to `agent/**`** | **disclosed** | Archivist is the only pusher; remote fixed at build time; refuses default-branch and force-push | **On a public remote, `agent/**` is world-readable: exfiltration is one push away and the push is a legitimate operation.** See 4.1. |
| PR title, body, comments; commit messages; branch names | disclosed | Same channel as above via the API relay | Same as above: free text to a readable place |
| Inference requests to the agency | pinned | `infer` on `modelnet` only; local weights; Mac node cannot phone home | The jailed-Claude overlay sends the request — source included — to Anthropic: **broken by design**, declared per repo by the disclosure gate (JAILED_CLAUDE.md) |
| Audit/state API | absent (as a read path) | Append-only, token-gated; the agent has no route to `state` | Appended text is operator-readable only; the operator is not the attacker |
| Shared caches between cells | pinned (interim) | Per-user cache binds; fixed names forbidden | The per-user cache is still read-write: a poisoning or signaling channel between concurrent tasks of one project (grange.md M4) |
| Build artifacts, images, logs | absent | Nothing leaves the cell but through the paths above | Operator-run fetches are reviewed |
| Operator's terminal / clipboard | human-read | The operator reads agent output | Social engineering: "paste this into X." Out of scope: a human is in the loop by construction |
| Timing, resource, and covert channels within the host | out of scope | — | Shared kernel; see section 6 |

### 4.1 The push channel is the finding

Everything else in the table is absent, pinned, or human-read. The push
channel is the one place a compromised agent can move arbitrary bytes
to a place it does not control and the attacker may read, and it can do
so with a legitimate request the archivist is designed to honor.

Three facts bound it:

1. The destination is fixed at build time (archivist endpoint table).
   The agent chooses the content, not the place.
2. The archivist refuses the default branch and force-pushes, so the
   bytes land in `agent/**` and nowhere canonical.
3. The operator reads the PR diff before merge.

None of those is a confidentiality control. (1) means the leak goes to
one place; (2) means it does not become canonical; (3) means the human
sees it *after* it has been pushed. If the remote is readable by the
attacker, the leak completed at push time, and review discovers it
rather than prevents it.

Therefore the confidentiality property of Cloister is exactly the
readability of the remote:

- **Public GitHub remote:** `agent/**` is world-readable. A1 is not
  protected against a compromised agent. This configuration is correct
  only for source that is already public (this repository, for
  instance), and the disclosure gate should say so per repo.
- **Private GitHub remote:** readable by the repository's collaborators
  and by GitHub. A1 is protected to the extent the operator trusts those
  parties. The bot credential's scope matters: it must be unable to
  change visibility or collaborators (forge-lint should verify this).
- **Jailed local Gitea on the LAN** (gitea.md): readable by LAN clients
  only. A1's exposure collapses to the LAN. This is the configuration
  the existing docs call "the single biggest containment upgrade
  available," and this section is the reason why.

The operator's decision about which of these to use *is* the
confidentiality policy, and the disclosure gate exists so that decision
is made per repository, deliberately, and cannot be inherited by
accident.

### 4.2 What "human-read" must mean

A channel counts as human-read only if the human reads the entire
content, not a summary, not a sample, and not something an agent
produced *about* the content. The scholar query qualifies because the
query is short and shown whole. A PR diff qualifies for integrity (every
byte that will become canonical is in it) but not for confidentiality
(the bytes have already left). Anything an LLM summarizes for the human
is advice; it does not make the channel human-read.

## 5. Enforcement principle

One rule (DESIGN.md): *contained means enforced at a boundary the model
cannot reach.* A permission system inside the agent — an LLM judging
whether an action is risky — is an LLM gating an LLM. Real enforcement
is topology the model cannot route around, mounts it cannot write,
validation that refuses rather than sanitizes, and history it cannot
rewrite. Policy is the topology; the security argument is
graph-reachability, not a rules audit.

Corollaries this document relies on:

- **Absence beats filtering.** A channel that does not exist cannot be
  argued with. Where a channel must exist, it is pinned to one
  destination and the relay that carries it parses nothing.
- **Refuse, never translate.** A refusal cannot silently do the wrong
  thing (git-proxy.md).
- **Narrow contracts across trust boundaries.** Expressiveness across
  the boundary is attack surface.
- **Prompt text is advice.** Everything in CLAUDE.md, the system
  prompt, and the agent's instructions is a usability measure. If a
  property in this document depended on the model following
  instructions, it would be listed as unenforced.
- **A control never observed failing closed is not a control.** Every
  "absent" above has a probe or a lint that would fail if it stopped
  being absent.

The invariant-by-enforcer table lives in ARCHITECTURE.md and is not
duplicated here.

## 6. Out of scope, by decision

- **Container escape.** A kernel or Docker escape from any cell reaches
  every cell on the host. Accepted: the host is single-tenant in the
  sense that matters (one operator's projects), and the alternative is
  a VM per cell, which is a different project.
- **A compromised host or operator account.** The jail is built on the
  host's kernel, daemon, and the operator's compose files. If those are
  the attacker's, nothing here holds.
- **The two human operators.** Flat superusers who trust each other;
  the boundary between them is administrative.
- **Side channels within the host** (timing, resource contention, disk
  usage as a signal between cells). Known; not defended.
- **Social engineering of the human** through agent output. The human
  is the control; this document does not model the human failing.
- **Availability.** A compromised agent can burn compute, fill disk,
  and occupy the single inference slot. Annoying, bounded by the task,
  not a security property.
- **Correctness of the model's work.** Nothing here makes the agent
  write good code. The PR review does that, or does not.

## 7. Residual risks, accepted or open

| Risk | Status |
|---|---|
| Push-channel disclosure on readable remotes (4.1) | **Open.** Policy, not a control; the disclosure gate makes it explicit per repo. Jailed Gitea is the mitigation. |
| Scholar query as a low-bandwidth leak | Accepted. Attenuated by human review of the whole query; not eliminated. |
| Read-write per-user cache between concurrent cells of one project | Accepted, interim (grange.md M4). Read-only content-addressed stores are the fix. |
| One bot credential across all cells | Accepted. Attribution by branch name and audit record, not by credential. |
| Jailed-Claude overlay sends source to Anthropic | Disclosed by design, per repo, via the gate. Path allowlist (`/v1/messages` only) and server-tool refusal at the proxy bound it to inference. |
| Mac inference node runs as the operator; PF backstop and egress probe are TODO | Open. |
| The proxy, CA key, and mutation addon in the jailed-Claude overlay are trusted code in the agent's decision path | Accepted; they join the bot token in the small set whose compromise compromises everything. |
| `~/.bashrc`-class persistence within a cell | Fixed for the known hole; the class remains: anything durable and agent-writable is a persistence channel. |

## 8. Open questions for the next pass

1. Should the archivist refuse pushes when the repository's disclosure
   level does not match the remote's visibility (e.g. a "private source"
   repo with a public remote)? That would make 4.1 a control instead of
   a policy.
2. Should branch names, commit messages, and PR text be size-bounded or
   templated by the archivist, to shrink the free-text channel? Cost:
   agents fight that too.
3. Is there a cheap way to make the scholar query review *stronger* than
   "the human reads it" — e.g. refusing queries that contain any
   identifier present in the grange?
4. Does the source-tree injection vector (section 2) deserve a control,
   or is it correctly covered by "the agent is compromised anyway"?
5. The heretic bench and refusal-as-fault (heretic.md): state explicitly
   that this is a reliability position about a model with no reach, and
   where it stops applying.
