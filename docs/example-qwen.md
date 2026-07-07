# Project context for the coding agent (EXAMPLE)

Copy this to `QWEN.md` at the root of a project workspace and edit the
project-specific parts.  qwen-code auto-loads `QWEN.md` as system context.
It is the *soft* half of the harness contract: the `agent-harness.yaml`
manifest tells the **builder** what is runnable; this file tells **you, the
model**, how to work here.

---

## Your environment: no direct internet, no local toolchain

You are running inside a locked-down container with **no direct internet
access** and **no build tools** (no compiler, no Gradle, no test runner).
You cannot `curl`, `pip install`, `npm install`, fetch dependencies, or
reach any external service directly.  Do not attempt it, and do not write
code that assumes network access at build or test time.

For the one thing you legitimately need the web for - looking up how an API,
library, or error works - there is a gated **research** tool (the scholar,
below).  It is not a general fetch and it installs nothing: every query is
read and approved by a human first.

You **can** read files directly, edit them through the **scribe** tools,
build/test through the **builder** tools, and research through the
**scholar** tool - all described below.

## How to build and test: the `builder` MCP tools

Building and running tests happens in a separate, jailed **builder**
service exposed to you as MCP tools.  The exact tools come from this
project's `agent-harness.yaml`; call **`harness_info`** to see the live
menu, then use the named actions.  Typically:

- **`build`** - compile the project (no tests).
- **`test`** - run the test suite.  Accepts a `filter` argument to run a
  subset, e.g. `filter: "WidgetMatcherServiceTest"` or
  `filter: "com.example.sampleapp.service.*"`.  Prefer a filter while
  iterating; run the full suite before declaring done.
- **`get_log(runId)`** - fetch the full log of a prior run.  Action results
  are a compact digest (status, parsed compile errors, failed tests, a
  short tail).  When the digest is not enough, page the real log with
  `get_log` using the `runId` from the digest.

Each action runs to completion and returns the digest - a build can take
minutes; wait for it.  Only one build runs at a time; if you get
`status: "busy"`, another run is in progress - check it with `get_log` on
the active `runId` or wait and retry.

## The rule that matters most: verify before you claim success

You have real build and test tools, so **never say a change compiles,
builds, passes tests, or is "done" until a builder action confirms it.**
Concretely:

1. Make the edit.
2. Call `build` (and `test`, with a `filter` for the affected area).
3. Read the digest.  If it failed, use the parsed errors / `get_log` to fix,
   and repeat.
4. Only report success after a green run - quote the `runId`.

"It should work" is not verification.  A green builder result is.

## Editing files: use the scribe tools, never shell

Your workspace is mounted **read-only**, and your built-in file-writing
tools are disabled.  Every file change - create, edit, rename, delete - goes
through the dedicated editor (**scribe**) MCP tools.  Do NOT try to write
with shell (`>`, `sed -i`, `tee`, `cp`), a heredoc, or a built-in write
tool: those either fail against the read-only mount or aren't available to
you.  The scribe is your only write path.

Scribe tools:

- **`create_text_file(path, content)`** - create a NEW file (fails if it
  already exists; edit an existing file with `apply_diff`).
- **`apply_diff(path, diff)`** - edit an existing file with a unified-style
  diff.  Hunks are located by their surrounding **context**, not line
  numbers, so include a few unchanged lines around each change.  One file
  per call.
- **`replace_string(path, find, replace, scope?)`** /
  **`replace_regex(path, pattern, replacement, scope?)`** - literal or RE2
  substitution; `scope: "all"` for every occurrence, else the first.
- **`move_file` / `move_directory` / `copy_file` / `delete_file` /
  `delete_directory`** - restructure the tree.
- **`get_diff(opId)`** - review exactly what a prior edit applied.

Pass `dryRun: true` to any edit to preview the resulting diff without
writing.

**Build-logic files are off-limits.**  Edits to `agent-harness.yaml`, any
`*.gradle.kts`, `gradle.properties`, `gradlew`, or anything under `gradle/`
or `buildSrc/` are **refused** or held for human approval - they decide how
the build runs, so only a human may change them.  If a write comes back
refused, **STOP**: do not retry it and do not route around it with shell.
Say what you needed to change and that it needs a human, then end your turn.

## Researching APIs and libraries: the `scholar` tool

When you need to know how a library, API, framework, or error works and it
isn't already in the repo, ask the **scholar** - a separate research
service - instead of guessing.  It is your ONLY web path, and it is
deliberately narrow and slow.

- **`research(query)`** - ask a single, self-contained question in plain
  English, e.g. `"How do I set a connection timeout on Java 21's
  HttpClient?"`.  The scholar searches the web, reads a few pages, and
  returns a distilled `{answer, sources}`.  It never returns raw pages and
  cannot download anything.

Contract - read this before you call it:

- **The query is human-approved and read verbatim.**  An operator sees every
  query before it runs.  Make it self-contained and put **nothing secret**
  in it beyond the question - no code, credentials, tokens, or internal
  details you wouldn't want read aloud.
- **It is slow.**  A call can take many minutes - several searches and page
  reads, plus a human approving the query before it starts and the answer
  before you see it.  Ask one good question and wait; don't spam calls.
- **A refusal or timeout means STOP.**  If `research` comes back refused,
  declined, or "not approved in time," do **not** retry or rephrase to
  sneak it through.  Note what you wanted to learn and proceed with what
  you have, or say you're blocked and end your turn.
- **It does not lift the offline rule.**  An answer may tell you a
  dependency exists, but you still cannot add one - that needs a human (the
  dependency airlock).  Research informs your code; it fetches nothing into
  the build.
- Treat the answer as a **lead, not gospel** - it came from the open web.
  Verify against the real API and, as always, with a green builder run.

## Watching your own work

A human can watch your builds live (status, audit trail, full logs) on a
local status page.  Everything you run through the builder is recorded in
an append-only audit log.  Work as if each action is observed - because it
is.

## Project specifics (EDIT THESE)

- **Language / build system:** _e.g. Kotlin + Gradle (JDK 25)._
- **Dependencies are pre-cached.**  Builds run offline.  If you genuinely
  need a new dependency, you cannot add it yourself - say so and stop; a
  human must run the dependency airlock.  Do not restructure the build to
  work around a missing library.
- **Layout:** _e.g. `src/main/kotlin`, tests in `src/test/kotlin`._
- **Conventions / quirks:** _package names, formatting, things to avoid._
- **Definition of done:** _e.g. `build` clean and full `test` green._
