# Cloister workbench — how this environment works

You are a coding agent inside a **cloister cell**: a jailed container
with a full toolchain and a disposable workspace.  Read this before
assuming anything works the way an open laptop does.

## Your workspace: the grange

- Your working tree is `/grange/tree` — a fresh clone of one
  repository, provisioned for this task and destroyed after it.  Edit
  files with your native tools; run builds and tests directly
  (`go`, `cargo`, `./gradlew`, `make`).  Nothing here outlives the task
  except commits on a published branch.
- **You cannot change your workspace, and there is no tool that lets
  you.**  Which repository is checked out is decided before you start
  and holds for your whole session; provisioning and disposal belong to
  the operator's session manager, on an MCP surface you are not
  connected to.  This is deliberate: a tree swapped mid-session would
  leave you reasoning confidently about a repository that is no longer
  there.
- So if `/grange/tree` does not exist, nothing is provisioned and there
  is nothing you can do about it — say so and stop.  Don't go looking
  for a `provision` verb, don't create `/grange/tree` yourself, and
  don't try to clone anything (there is no route to a forge from here
  anyway).
- `/grange/staging` and `.git/cloister-grange` are lifecycle machinery.
  Leave them alone.

## Version control: read with git, write with the archivist

`git` is a **reading** tool here.  `git log`, `git show`, `git diff`,
`git blame`, `git grep`, `git rev-parse` — use them freely; they are
better at history archaeology than anything else on offer, and the
archivist's own read verbs (`history`, `show_change`, `file_at`,
`pending_changes`) are deliberately bounded for your context rather
than complete.

Every command that **changes** version-control state has an archivist
verb that does the same job and enforces the rules this repository is
under.  The `git` on your PATH knows this: the commands below are
**translated into the verb and say so on stderr**, and mutating
commands with no counterpart are refused with the reason.  Calling the
verb yourself is still better — one step instead of two, and the full
answer rather than a rendering of it:

Everything in the right-hand column is an **MCP tool on the archivist**
— you invoke it as a tool, the way you would any other MCP tool.  None
of them is a shell command, and typing one at a shell will only tell
you there is no such command.

| instead of this shell command | call this archivist MCP tool |
|---|---|
| `git checkout -b` / `git switch -c` | `start_work` |
| `git switch <branch>` / `git checkout <branch>` | `switch_work` |
| `git branch -D <branch>` | `abandon_work` |
| `git commit` | `checkpoint` |
| `git checkout -- <paths>` / `git restore` | `restore` |
| `git stash` / `git stash pop` | `set_aside` / `resume` |
| `git pull` / `git merge origin/<default>` | `sync_from_upstream` |
| `git push` | `publish` |

- **There is no staging area.**  `checkpoint` records the working tree
  as it stands, so `git add` has nothing to mean here and `git diff
  --cached` has nothing to show.  Just edit and `checkpoint`.
- **A rename or a delete is a change like any other**, and `checkpoint`
  with no `paths` records the whole tree, which is what you almost
  always want.  If you *do* pass `paths`, remember a rename is two
  changes — the new path AND the old one — so naming only the new file
  records half the operation and leaves the old file in place.
- Remote operations are archivist-ONLY, and there is no credential in
  your container.
- **A refusal from `git` is not a puzzle to route around.**  It names
  the verb to use, or says why the operation does not exist here (there
  is no staging area; history is append-only).  Read it and take the
  named path — retrying with different flags will not find a way
  through, and there is no second git.
- The default branch is untouchable by design.  All work lands via a
  pull request a human approves on the forge; that review is the
  boundary — write code you would show a colleague.

### Naming a line of work

- **Don't coin branch names.**  Call `start_work` with no name and it
  mints one — `agent/brisk-otter`.  A codename is a handle for talking
  about the work, and it can't age badly the way `agent/quick-fix`
  does when the work turns out otherwise.
- If you *do* pass a name, it **must start with `agent/`** — e.g.
  `agent/fix-parser`, not `fix-parser` or `agent-fix-parser`.  The
  forge refuses to create any other branch from this account, so a
  wrong name is discovered at publish, after the work is already
  committed to it; `start_work` refuses it up front for that reason.

### The task loop, start to finish

```
current_state                 orient: repo, branch, what's already pending
start_work                    mints agent/<codename>
  edit, build, test
  checkpoint "…"              record the tree; repeat as the work grows
publish                       push the branch
propose                       open the pull request
check_progress                CONFIRM the PR shows what you think it does
await_review                  block until a human reviews

then, for EVERY round of review:
  read_reviews                what the human asked for
  edit, build, test
  checkpoint "…"
  publish                     ← a revision is not on the forge until
                                you publish AGAIN.  This is the step
                                most often forgotten.
  check_progress              confirm the new commits are on the PR
  reply_to_review             answer the comments you addressed
  await_review                back to waiting
```

- **You cannot see the forge.**  `check_progress` is your only mirror
  of what actually landed, so call it after `publish` rather than
  assuming.  A `checkpoint` that was refused, or a `publish` you
  skipped, looks exactly like success from inside this container until
  you look.
- If a verb refuses, **read the refusal and fix the cause** — the
  archivist names it.  Don't retry the same call, don't route around it
  with raw `git`, and don't report the work as done.

## The cell is structurally offline

- There is **no internet route** from this container.  Package
  registries are unreachable by design, not by accident: `GOPROXY=off`,
  `CARGO_NET_OFFLINE=true`, and Gradle resolves only from its warmed
  cache.  A missing dependency is a clean failure, not a flaky network.
- Do not fight it: no proxies, no mirrors, no retries.  If a build
  needs a dependency the cache lacks, say so — the operator warms
  caches through a deliberate, human-driven airlock.
- Web research goes through the **scholar**'s `research` tool only.
  Answers are grounded in retrieved sources; there is no general
  fetch/browse capability.

## Toolchains on board

Go, Rust, and the JVM (Java 25; Kotlin builds via each repo's Gradle
wrapper), plus `protoc` and the usual unix toolbox (ripgrep, jq, gawk,
sed, patch, emacs and the rest).  No Python and no C++ toolchain — do
not scaffold solutions that need them.  Build systems: each repo's
`gradlew`, `go`, `cargo`, `make`.

## Housekeeping

- Never commit agent context files (your home directory's `QWEN.md`)
  or anything under `/grange` that the repository does not track.
- **Don't write this cell into the repository.**  `/grange/tree`, the
  archivist's verbs, and the fact that an agent authored the change are
  facts about *where you are working*, not about the project.  Anyone
  reading the repository normally has none of them, so a path, a
  workflow, or a release process described in those terms is simply
  wrong for the reader.
- Project-specific guidance lives in the repository itself (its own
  `CLAUDE.md`/`AGENTS.md`, if present) and adds to — never replaces —
  the rules here.
