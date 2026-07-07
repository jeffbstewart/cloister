# GitHub setup for agent PR authorship

The permission arrangement the archivist ([archivist.md](archivist.md))
assumes: the agent authors branches and PRs under its own identity, and
**cannot modify the default branch** — every change reaches `main` through
a PR a human reviewed.  This is the setup proven on the Cloister repository
itself, generalized for replication.

## 1. A separate bot account

Create a dedicated GitHub account for the agent (e.g. `<you>-agent`).  The
agent must not act as you: GitHub refuses PR self-approval, so a separate
author identity is what makes "a human approved this" structurally true
rather than procedurally hoped.  Commits, pushes, PRs, and review replies
all happen as the bot; you review and merge as yourself.

## 2. Repository access

Add the bot as a collaborator with **Write** permission on the project
repository.  Write allows pushing branches and opening PRs; it does not
bypass rulesets (below), and the bot never needs Admin or Maintain.

## 3. Ruleset on the default branch

On the repository, add a ruleset targeting the default branch:

- **Require a pull request before merging**, with **1 required approval**.
- **Require status checks to pass** — name the CI check that gates your
  repo (this repo requires `verify`).
- **Block force pushes.**

With PR-required plus author-can't-self-approve, the bot's Write
permission cannot touch `main` by any path: direct pushes are refused,
and its PRs merge only after your approving review with green checks.

## 4. The token

Create a token for the bot and put it in exactly one place: the
archivist's environment in the cell's stack env.  It must never appear in
a repository file, an image, or anywhere the agent can read.

- **Classic PAT with `repo` scope** is the proven configuration (this is
  what Cloister itself runs).
- A **fine-grained PAT** restricted to the one repository with
  *Contents: read/write* and *Pull requests: read/write* is the
  tighter-scoped alternative; prefer it if it covers your workflow.

Set an expiration you'll actually rotate on; treat rotation like any
other secret (update the stack env, redeploy the archivist).

## 5. Working conventions

- The bot proposes; **a human merges**.  Nothing in the tooling merges a
  PR, by design.
- The exfiltration boundary is your review: the agent can push what it
  can push, and the PR diff on GitHub is where you read every byte before
  it becomes part of `main`.  Rulesets make sure nothing skips that
  reading.
- One bot account works across projects; access and rulesets are granted
  per repository.
