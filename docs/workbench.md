# The workbench session manager

*Status: built (PR #126).  `cmd/workbench`, shipped as
`/usr/local/bin/workbench` in the agent image (docker/workbench).*

The operator's door into a cell, and the owner of the workspace's
lifetime.  It exists because `provision` and `dispose` moved off the
agent's MCP surface ([archivist.md](archivist.md), "Two surfaces"): once
the model cannot name those verbs, something outside the model has to
call them, and that something is a deterministic program driven by a
human — not another agent.

## The loop

```
workbench
  live tmux session?  ── yes ──> attach.  Done.
        │ no
        ▼
  workspace_state
        ├── corrupt      ── report, offer nothing, exit non-zero
        ├── provisioned  ── [start the agent | shell | dispose | quit]
        └── empty        ── pick a repo (recent list, or a URL)
                            └── provision ──> back to the menu
```

Attaching happens **first**, before the archivist is dialed at all: a
task already in flight must not become unreachable because its
archivist is sick.

The agent runs under tmux, and the manager **waits** rather than
`exec`ing into it.  Coming back is the point — when the agent exits, the
operator lands on the menu with "dispose" one keystroke away, which is
what makes provision → work → dispose a loop instead of three things to
remember.  A detach (`Ctrl-Z d`) leaves the session running and ends the
invocation; that is the other half of why the agent lives in tmux.

## What it will and won't do for you

- **Dispose is offered; discarding is a separate act.**  A refusal over
  unpublished work is the system doing its job, so the archivist's own
  message is printed verbatim and the operator must type `DISCARD` — not
  `y` — to destroy it.  The manager never retries a refused dispose with
  `force` on its own initiative.
- **A CORRUPT workspace is reported, never acted on.**  Dispose refuses
  a markerless tree at *any* force — the rail that keeps it structurally
  unable to wipe a mounted host tree — so escalating would be theater.
  Recovery is host-side, and the message says so.
- **Nothing is offered that the state does not allow.**  A live
  workspace has no "provision" option, because provision requires an
  EMPTY one; that refusal exists in the archivist regardless, and a menu
  that offered it would just be a slower way to read the error.

## The recent-repositories list

A convenience index, never a source of truth: the archivist's
disk-derived state is the only authority on what is provisioned, and
this file only remembers what was asked for.  So every read or write
failure is non-fatal and merely mentioned — a lost list costs one typed
URL, while a workbench that refused to start over a damaged cache file
would cost the whole session.

- **Where**: `$HOME/caches/cloister/repos`, on the per-user
  `AGENT_CACHES` bind.  Deliberately not `$HOME` itself, which is a
  per-project volume and would forget every time — the whole value is
  that the *next* cell already knows your repositories.  Override with
  `WORKBENCH_REPOS`.
- **Format**: `epoch \t repo \t branch`, sorted on load, order on disk
  never trusted (the house rule for every epoch-second ledger).
  Unreadable lines are dropped individually.
- **One entry per repository**, holding the line of work last
  provisioned from it and offered back as the default: resuming is the
  common case, and a minted codename (`agent/brisk-otter`) is exactly
  the thing nobody remembers.
- **Written only after a provision succeeds.**  That is what keeps
  typos and unreachable hosts out of the list, and it is why `f <n>`
  (forget) is a convenience rather than a necessity.

## Configuration

Every flag has an environment default, so the image needs no arguments:

| Flag | Environment | Default |
|---|---|---|
| `-archivist` | `ARCHIVIST_OPERATOR_URL` | `http://archivist:9600/operator/mcp` |
| `-session` | `WORKBENCH_SESSION` | `agent` |
| `-repos` | `WORKBENCH_REPOS` | `$HOME/caches/cloister/repos` |
| `-agent` | `WORKBENCH_AGENT` | `qwen` |
| `-grange` | `GRANGE_ROOT` | `/grange` |

## Boundaries

The session manager and the agent share a container, a network
namespace, and a uid.  **This is not a security boundary**, and nothing
here should be described as one: a determined process in that container
can dial `/operator/mcp` directly.  What the split buys is that the
model cannot *name* the lifecycle verbs, which is what accidents are
made of.  The load-bearing boundaries are elsewhere and unchanged — the
forge ruleset, the endpoint allowlist, and the bot credential living
only in the archivist.
