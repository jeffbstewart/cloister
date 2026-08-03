# The git proxy

*Status: DESIGNED, not built.  The prompt-level rule it will enforce
ships first (docker/workbench/AGENTS.md, "read with git, write with the
archivist").*

## Why

The agent has a real git and a real working tree, and the archivist owns
version control.  That is two vocabularies for one set of operations,
and the boundary was drawn in the wrong place: the stock prompt was
emphatic about `git push` — which is already structurally impossible (no
credential, no route) and fails loudly — and permissive about local
writes, which always succeed and are silent.

The archivist caches no git state (`Publish` reads the current branch at
call time; the grange derives its state from disk), so a raw `git
commit` does **not** desynchronize it.  The cost is different and
subtler:

- Every mutating git command has an archivist verb that does the same
  job **plus** enforces something the raw command doesn't — the
  default-branch refusal, the `agent/` namespace check (R8), message
  validation, the attribution-trailer refusal, the endpoint-derived
  identity.  Reaching for raw git is the same action with the
  guardrails removed.
- A raw `git commit` leaves no trace in the MCP transcript, so a session
  reconstructed afterwards is missing half its history.

So git stays, as a **reading** tool, and the writes route to the verbs.

## The cut: refs and HEAD, not read and write

The useful classification is not read/write.  Commands that touch only
the working tree are already safe, because `checkpoint` records the tree
wholesale — git's index is an implementation detail the archivist
ignores.  What matters is whether a command moves **refs or HEAD**,
because those are the state the archivist's verbs are meant to author.

| class | examples | proxy behavior |
|---|---|---|
| reads | `log`, `show`, `diff`, `blame`, `grep`, `rev-parse`, `describe`, `status` | pass through unchanged |
| worktree-only writes | `mv`, `rm`, `clean` | pass through — `checkpoint` records the tree anyway |
| ref/HEAD writes with an exact verb | `commit`, `checkout -b`, `switch`, `branch -D`, `stash`, `restore`, `pull`, `push` | translate to the archivist verb |
| everything else | `rebase`, `commit --amend`, `cherry-pick`, `revert`, `merge`, `tag`, `reset --soft`, `bisect` | refuse, naming why |

## Visible translation, never illusion

**The proxy announces every translation on stderr:**

```
[cloister] git commit -m "fix parser"  →  archivist checkpoint
```

This is the load-bearing design decision, and it is the same principle
that made `provision`/`dispose` *absent* from the agent's MCP surface
rather than hidden ([archivist.md](archivist.md), "Two surfaces"): the
model must not hold beliefs that quietly diverge from reality.  An
emulated git reintroduces exactly that failure one layer down.  If
`commit --amend` silently became a new checkpoint, the agent would
believe it rewrote history, it would not have, and it would reason from
that false belief for the rest of the session — with a transcript that
looked correct throughout.

The visible translation gets both halves: the reflexive `git commit`
works, *and* the agent's map stays accurate.  It also teaches — after
two of those lines, a model starts calling `checkpoint` directly.

## Why the full surface cannot be emulated

Four reasons, in descending order of how fundamental they are:

1. **Semantic mismatch, not missing features.**  The archivist has no
   staging area.  `git add`, `git diff --cached`, and `git reset HEAD
   <path>` reference a noun the target language does not contain.  That
   is untranslatable in principle.
2. **No counterpart for history rewriting.**  The archivist's model is
   append-only checkpoints plus `restore`.  Amend, rebase, and
   cherry-pick have nothing to map onto; emulating them means either
   lying or implementing surgery the archivist deliberately withholds.
3. **Flag combinatorics.**  `git commit` alone carries ~40 flags, and
   every one silently ignored is a divergence between what was asked
   and what happened.
4. **Interactive forms** (`rebase -i`, `add -p`, `bisect`) need an
   editor and a loop with nothing to map to.

Hence: translate a small closed core, refuse the rest with a named
reason.  Of git's ~150 porcelain commands an agent reaches for perhaps
fifteen, and the core above covers nearly all of them.

## Strictness

**An unrecognized flag on a translatable command is a refusal, not a
guess.**  Same discipline as the house flag-parsing rule: a flag we do
not understand means we cannot promise the semantics, and approximating
is how a request quietly becomes a different request.

**An unrecognized command is also a refusal**, not a passthrough — a
loud error we see immediately beats a silent bypass.  The escape hatch
below is what keeps that from costing a session.

## Logging

Log the **translations** and the **refusals**.  Do not log passthrough.

Passthrough is the high-volume, zero-signal case: every `go build`
stamps VCS info, every Gradle version plugin calls `describe`, and
recording all of it would bury the two events that carry meaning and
grow the log without bound.  A translation says the agent reached for
git where a verb already exists — a prompt-tuning signal.  A refusal
says it wanted something not on offer — a candidate for a new verb, a
new translation, or a line in AGENTS.md.  Together the log is a
curriculum for what to build next.

Destination: a file under the agent's HOME (the per-project volume), not
`/grange` — the grange is destroyed at dispose, and the log's whole
value is being read afterwards.  Not the state service: the agent holds
no route to it by design, and it never touches the record of its own
actions.

## Escape hatch

`CLOISTER_GIT_PASSTHROUGH=1` runs the real binary unchanged.  The first
build that breaks on an unrecognized command must not cost a session to
unblock, and nested scratch repositories (`cargo new` runs `git init`)
will find the proxy too.

## Not a boundary

The real binary is moved somewhere the proxy alone names.  This is
**obscurity, not enforcement**: `dpkg -L git`, a `find` for executables,
or `/usr/lib/git-core/` all lead back to it in seconds, and a determined
process in this container can call it.  What it buys is that the wrong
move is no longer the reflexive one — the same claim, with the same
limits, as the two-surface split.  The load-bearing boundaries are
elsewhere and unchanged: the forge ruleset, the endpoint allowlist, and
the bot credential living only in the archivist.

## Open questions

- **Toolchain callers.**  Go's `-buildvcs` stamping runs `git status
  --porcelain`, `rev-parse HEAD`, and `show -s`; Gradle version plugins
  run `describe`.  All reads, so all pass — but this should be confirmed
  against a real warmed build before the proxy is load-bearing.
- **`git rm --cached`** touches the index without touching the tree,
  which has no meaning in a no-staging model.  Probably a refusal.
- **Which verb set the proxy dials.**  `internal/operator` is the
  operator surface's client; the proxy needs a sibling for the agent
  verbs.  Worth sharing the transport and the `*RefusedError` shape.
