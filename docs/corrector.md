# The corrector — code-review agent design

Status: **design, not yet implemented**.  Decisions from the 2026-07-07
design review.  Depends on the librarian ([librarian.md](librarian.md))
and the archivist ([archivist.md](archivist.md)) — the corrector is a
composition of both, and implementation follows theirs.

## Problem

Every change reaching `main` gets exactly one review: the operator's.
Agent-authored PRs deserve a systematic pass through the house's own
standards before that review, and the operator's own changes get no
second pair of eyes at all.

## Shape

A new worker mode, the **corrector** (the scriptorium official who checked
copied manuscripts against the exemplar): the cell's reviewer, composing
the other authorities rather than holding any of its own.

- **No mounts, no credential.**  Tree context comes from the librarian's
  read tools; diffs, PR context, and revision-pinned file contents come
  from the archivist; findings post to GitHub *through the archivist's*
  comment verbs, so the archivist remains the cell's only GitHub toucher.
  The corrector owns judgment, nothing else.
- **Advice, never a gate.**  Nothing blocks on a finding: no merge gate,
  no approval hold, no required check.  An LLM gating an LLM is the
  permission theater DESIGN.md exists to reject — findings are margin
  notes for humans (and input the agent may act on), full stop.
- **Author-agnostic.**  A review of PR #N does not care who wrote #N.
  The operator's own proposals get the same pass on request — by explicit
  decision, since the lens set is author-neutral and dependency pushback
  on operator PRs is, if anything, more valuable (nobody reviews the
  reviewer).

## Lenses

Each lens is its own pass with its own prompt; every finding cites
file:line and quotes what it saw — grounded in retrieved diff and tree
content, never the model's weights (the scholar's grounding rule, applied
to review).

| Lens | Looks for |
|---|---|
| correctness | logic errors, unhandled edge cases, concrete failure scenarios |
| house conventions | the CLAUDE.md/DESIGN.md rules: typed concepts over primitives, fail-closed posture, comment discipline |
| security invariants | topology/containment regressions code-side: egress surface, mount handling, secret patterns, trust-boundary drift |
| test adequacy | whether the change's tests would catch its failure modes; untested branches; timing-dependent tests |
| style | language-idiomatic code; readability; naming that matches the surrounding code; comment density and utility appropriate to the file |
| dependencies | any new dependency gets pushback by default — supply-chain surface is a security property here |
| documentation | stale docs the change obsoletes; docs the change now needs |
| simplicity | prefer the simple solution that works over the elaborate one that does roughly the same thing |
| reuse | duplicated blocks; logic that should be promoted to a shared module and referenced instead of copied |
| description | the commit/PR description matches what the change actually does |

## Triggers

Three, spanning the two audiences:

1. **Self-review** (agent-invoked): a `review_pending()` verb over the
   uncommitted working tree, so the agent critiques and fixes its own
   work *before* a PR exists.  The operator sees cleaner PRs, not robot
   chatter.  Invocation discipline lives in QWEN.md.
2. **Auto on propose** (structural): the archivist fires a review request
   at the corrector after `propose()`; residual findings land as PR
   comments before the operator looks.  Fire-and-forget — a corrector
   outage never blocks a propose.
3. **Operator request** (no agent in the loop): the status page accepts
   "review PR #N"; the corrector polls the state service for pending
   requests — the scribe's approval-polling pattern, reversed — reviews
   any PR by number regardless of author, and posts findings to the PR
   and the status page.

## What this asks of the archivist

Two read-only, VCS-agnostic contract additions (folded into
[archivist.md](archivist.md)'s implementation):

- `file_at(ref, path)` — file contents at a revision (git `show
  ref:path`; svn `cat -r`).  The librarian's RAM model reflects the
  current checkout; reviewing a PR that isn't checked out needs
  base/head-pinned reads, and the corrector must never switch the working
  tree under a live session to get them.
- PR reads generalized from "the agent's PR" to "a PR number" —
  `check_progress`, `read_reviews`, and diff retrieval take an explicit
  target.

## Topology

```
corrector:
  volumes:  none
  networks:
    - buildnet    # reach librarian + archivist MCP; reachable by agent (:9700)
    - infernet    # drive inference for the lens passes
    - statenet    # poll operator review requests; store reports
```

No egress, no workspace, no GitHub credential — compose-lint asserts all
three.  Inference is engine-routed like the librarian's; the lens passes
are the prime candidate for the planned `infernet_big` deep-think node,
and degrade to local `infer` when it is absent.

Reports are stored via the state service (the diff-store pattern), so
every review is on the status page even when its PR comments are the
delivery vehicle.

## Phasing

1. After the librarian's mechanical reads and the archivist's local verbs
   exist: the review core — lens prompts, grounded-finding format, report
   storage — driven as a library against fakes.
2. Worker mode + `review_pending()` (self-review needs no GitHub at all).
3. The archivist contract additions; review-by-PR-number.
4. Auto-on-propose wiring from the archivist.
5. The status-page request channel.
6. ARCHITECTURE.md from PLANNED to real; compose-lint invariants.
