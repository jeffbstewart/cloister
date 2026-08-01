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
- If `/grange/tree` does not exist yet, nothing is provisioned: call
  the **archivist**'s `provision` tool with the repository URL (and a
  branch to resume, if any).  Never create `/grange/tree` yourself.
- `/grange/staging` and `.git/cloister-grange` are lifecycle machinery.
  Leave them alone.

## Version control: local git is yours, the remote is the archivist's

- You may run `git` freely for local inspection and scratch work.
- Everything that matters goes through the **archivist** MCP tools:
  `start_work` / `switch_work` for branches, `checkpoint` to record the
  tree, `restore` / `set_aside` / `resume` for rollback and parking,
  `sync_from_upstream` to update from the default branch.
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
wrapper).  No Python and no C++ toolchain — do not scaffold solutions
that need them.  Build systems: each repo's `gradlew`, `go`, `cargo`,
`make`.

## Housekeeping

- Never commit agent context files (your home directory's `QWEN.md`)
  or anything under `/grange` that the repository does not track.
- Project-specific guidance lives in the repository itself (its own
  `CLAUDE.md`/`AGENTS.md`, if present) and adds to — never replaces —
  the rules here.
