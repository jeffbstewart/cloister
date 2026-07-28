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
- **Dismiss stale pull request approvals when new commits are pushed** —
  without this, an approved PR can gain a malicious commit and merge on
  the old approval (grange.md R2).
- **Require review from Code Owners**, with a committed
  [`CODEOWNERS`](../.github/CODEOWNERS) naming you as owner of `*` and of
  `/.github/` — then only YOUR approval satisfies the rule (the bot's is
  worthless), and workflow changes cannot merge without you.
- **Require status checks to pass** — name the CI check that gates your
  repo (this repo requires `verify`).
- **Block force pushes** and **restrict deletions**.
- Bypass list: **empty**, or at most the repository-admin role.

With PR-required plus owner-only approval, the bot's Write permission
cannot touch `main` by any path: direct pushes are refused, and its PRs
merge only after your approving review with green checks.

### The namespace ruleset (R8)

Add a second ruleset targeting **all branches**, excluding
`refs/heads/agent/**` and the default branch, with **restrict creations**
and **restrict updates** (bypass: admin role at most).  The bot can then
create and update only `agent/**` branches server-side; the archivist
refuses out-of-namespace pushes client-side as the belt under it.

### Verifying it: forge-lint

`forge-lint` (cmd/forge-lint) asserts all of the above — the grange
design's R1-R8 — against the live repository via the API:

    FORGE_LINT_TOKEN=<your admin PAT> go run ./cmd/forge-lint

Run it with YOUR credential, never the bot's (reading collaborator roles
and Actions secrets takes admin, which the bot must not have; sections
the token cannot read report UNVERIFIED, and only an operator run can
clear them).  CI runs the same lint with its limited read token and
`-allow-unverified`, so any readable rule that drifts fails the PR.
`etc/forge-lint.yaml` pins the expected identities; the Actions-secrets
audit expects **zero secrets** — the presubmit suite needs none, keep it
that way.

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
