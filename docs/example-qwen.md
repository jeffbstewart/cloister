# Project context for the coding agent (EXAMPLE)

Copy this to `QWEN.md` at the root of a project workspace and fill in the
project specifics.  The agent reads it through the librarian at the start
of each session (its baked-in, project-agnostic prompt — see
`docker/agent/QWEN.md` — instructs it to).  Keep this file to things the
harness prompt cannot know: this project's layout, conventions, and
definition of done.  There is no need to re-explain the tools or the
containment rules here.

---

## What this project is

_One or two sentences: what the software does and who uses it._

## Build and test

- **Language / build system:** _e.g. Kotlin + Gradle (JDK 25)._
- **Manifest actions:** _what `build`, `test`, and any extra
  `agent-harness.yaml` actions mean here; useful `filter` examples, e.g.
  `filter: "WidgetMatcherServiceTest"` or
  `filter: "com.example.sampleapp.service.*"`._
- **Dependencies are pre-cached; builds run offline.**  If you genuinely
  need a new dependency, say so and stop — a human must run the
  dependency airlock.  Do not restructure the build to work around a
  missing library.

## Layout

_e.g. `src/main/kotlin` for production code, tests in `src/test/kotlin`,
generated code under `build/` (not visible to you)._

## Conventions and quirks

_Package naming, formatting rules, patterns to follow or avoid, files
that look editable but aren't yours to touch._

## Definition of done

_e.g. `build` clean and the full `test` suite green; quote the runId._
