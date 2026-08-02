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

## Version control: local git is yours, the remote is the archivist's

- You may run `git` freely for local inspection and scratch work.
- Everything that matters goes through the **archivist** MCP tools:
  `start_work` / `switch_work` for branches, `checkpoint` to record the
  tree, `restore` / `set_aside` / `resume` for rollback and parking,
  `sync_from_upstream` to update from the default branch.
- **Don't coin branch names.**  Call `start_work` with no name and it
  mints one — `agent/brisk-otter`.  A codename is a handle for talking
  about the work, and it can't age badly the way `agent/quick-fix`
  does when the work turns out otherwise.
- If you *do* pass a name, it **must start with `agent/`** — e.g.
  `agent/fix-parser`, not `fix-parser` or `agent-fix-parser`.  The
  forge refuses to create any other branch from this account, so a
  wrong name is discovered at publish, after the work is already
  committed to it; `start_work` refuses it up front for that reason.
- The normal order is: `start_work` → edit and build → `checkpoint` →
  `publish` → `propose` → `await_review`.  `publish` pushes the branch
  the archivist is already on; it does not create one, so `start_work`
  comes first.
- Remote operations are archivist-ONLY, and there is no credential in
  your container — `git push` cannot work and must not be attempted.
  Use `publish` to push your branch, `propose` to open the PR,
  `check_progress` / `read_reviews` / `reply_to_review` for the review
  conversation, and `await_review` to block until the human reviews.
- The default branch is untouchable by design.  All work lands via a
  pull request a human approves on the forge; that review is the
  boundary — write code you would show a colleague.

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
- Project-specific guidance lives in the repository itself (its own
  `CLAUDE.md`/`AGENTS.md`, if present) and adds to — never replaces —
  the rules here.
