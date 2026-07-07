# The archivist — source-control sidecar design

Status: **design, not yet implemented**.  Decisions from the 2026-07-07
design review, recorded as the fixed target for the implementation PRs.
Rationale style follows [DESIGN.md](DESIGN.md); the runtime picture is in
[ARCHITECTURE.md](ARCHITECTURE.md) (marked PLANNED).

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
librarian (planned) is sole reader.

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

## Verbs

Local — free and **unaudited** (working-tree mechanics, not boundary
crossings):

| Verb | Meaning (git realization) |
|---|---|
| `current_state()` | branch, dirty files, ahead/behind (status) |
| `history(path\|ref)` | change log, capped (log) |
| `show_change(id)` | one change with its diff (show) |
| `pending_changes(path?)` | the uncommitted delta vs the last checkpoint, whole-tree or one file (diff) |
| `start_work(name)` | new line of work off the default branch (branch + switch) |
| `abandon_work(name, deleteRemote?)` | discard a line of work: switch to the default branch, delete the local branch (branch -D).  Refuses on the default branch or a dirty tree.  `deleteRemote` also removes the published counterpart — that half is a remote op, audited |
| `checkpoint(message, paths?)` | record the working tree — all of it, or just the named paths (commit, or commit -- paths) |
| `restore(checkpoint?, path?)` | roll back: one file's local edits (restore path), one file from a checkpoint (checkout ref -- path), or the whole tree (reset --hard) |
| `set_aside()` / `resume()` | park and recover uncommitted work (stash push/pop) |
| `sync_from_upstream()` | update the local default branch and replay work on it (fetch + rebase) |

**No staging verbs.**  The index is a git realization detail, not part of
the contract (subversion has none, and staged-vs-worktree divergence is a
state class the agent can silently lose track of).  Checkpoints always
read the working tree; selective recording is `checkpoint`'s `paths`
parameter, not an `add` step.

Remote — **audited, ungated** (every GitHub touch leaves a record; none
waits for approval):

| Verb | Meaning |
|---|---|
| `publish()` | push the current work branch |
| `propose(title, body)` | open (or update) the PR for the current branch |
| `check_progress()` | PR state + CI check results |
| `read_reviews()` | review comments and threads on the agent's PR |
| `reply_to_review(thread, body)` | respond on a review thread |
| `await_review(maxWait)` | block until review activity: new comments, approval, changes-requested, or merge/close |

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

## Hardened git execution

The archivist drives the real git binary — but never with ambient trust:

- Hooks neutralized (`core.hooksPath` pointed at an empty directory) on
  every invocation; the archivist never executes repository-supplied code.
- Global/system config disabled (`GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_CONFIG_SYSTEM=/dev/null`); dangerous keys (fsmonitor, filters,
  aliases) overridden per-invocation with `-c`.
- Remote protocol restricted to https toward the pinned relays; no
  credential helpers — the token is injected per call, never stored in
  config inside the workspace.

The `.git` directory the archivist maintains still lives in the workspace
mount (host git interop is a feature — the operator can inspect the repo
normally), but every cell-side toucher of it is the archivist's own
hardened invocations.

## Identity and credential

Commits and PRs happen as a **bot account** (the operator reviews as
themselves; the bot cannot approve or merge).  Its token lives in one
place: the archivist's environment, per cell.  The agent never sees it,
and no scribe-writable or librarian-readable file contains it.
[GITHUB_SETUP.md](GITHUB_SETUP.md) is the replication recipe: bot account,
token, collaborator grant, and the branch ruleset that keeps `main`
requiring a human PR approval and green checks.

## Topology

```
archivist:
  volumes:  ${WORKSPACE}:/workspace     # rw: worktree ops rewrite files; sole .git toucher
  networks:
    - buildnet     # reachable by the agent (inbound MCP, :9600)
    - statenet     # audit records for remote ops
    - gitegress    # to the github relays ONLY (internal)

github-relay / github-api-relay:
  # kagi-relay pattern: blind socat pipes, pinned to github.com:443 and
  # api.github.com:443; the only holders of `egress` besides the kagi-relay.
```

compose-lint grows the matching invariants: the archivist holds no
`egress`/`frontend` network; the relays are pinned; the scholar's isolation
is unchanged (it gains no route to the archivist or the workspace).

Worktree interplay is documented semantics, not magic: the scribe, the
builder, and the archivist all write the same tree, so `restore`,
`set_aside`, and `sync_from_upstream` can clobber uncommitted edits —
sequencing is the agent's responsibility, and `current_state()` before
destructive verbs is the documented idiom.  Audit for remote ops rides a
new typed detail on the existing envelope (op, branch, PR number, target).

## Future backends

Subversion (or others) later means an adapter behind the same verbs:
`checkpoint` maps to `svn commit`, `history` to `svn log`,
`publish`/`propose` to whatever review flow that world offers.  The
contract deliberately never mentions refs, remotes, rebases, or staging —
those are realization details of the git adapter.

## Phasing

0. DONE (PR #34): workspace confinement rejects `.git/**` — the scribe can
   never be the archivist's confused deputy.
1. `internal/archive`: the hardened git runner (hooks off, config
   isolated, env-scrubbed) + the local verb set, with an injected clock
   and a fake-remote test rig.
2. Worker mode + MCP surface for local verbs; compose entry (no egress
   yet — local-only cells work fully).
3. The relays, remote verbs, client-side refusals, and the remote-op
   audit detail; GITHUB_SETUP.md becomes load-bearing.
4. `await_review` long-poll with progress notifications.
5. ARCHITECTURE.md updates from PLANNED to real; compose-lint invariants.
6. Someday: the subversion adapter proves the verb contract honest.
