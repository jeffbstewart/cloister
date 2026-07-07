# The librarian — reader-mode design

Status: **design, not yet implemented**.  This records the decisions from the
2026-07-07 design review so the implementation PRs have a fixed target.  When
the librarian lands, DESIGN.md absorbs the rationale and this file becomes
the reference for the tool surface.

## Problem

The agent holds a read-only mount of the workspace.  That mount is the last
place where the agent touches project bytes without mediation: it does not
enforce `.aiignore` (DESIGN.md lists this under Honest limits), it produces
no record of what the agent looked at, and it drags in the entire unix
file-navigation idiom — the agent spends context and turns chaining `cat`,
`grep`, `head`, `wc`, and `find` through its built-in tools.

## Shape

A new worker mode, the **librarian**, becomes the entire read side of the
cell, the same way the scribe is the entire write side:

- **scribe** — sole writer.  Unchanged; gains no read tools.
- **librarian** — sole reader.  Serves mechanical read tools AND
  inference-backed comprehension tools from one in-memory model of the
  agent-visible workspace.
- **agent** — holds **no workspace mount at all**.  Hard cutover: the `:ro`
  mount is removed, qwen-code's built-in filesystem tools are disabled in
  the same change, and a tmpfs stub provides the working directory.

Reads and writes split cleanly because their authorities differ: the scribe
must hold the workspace read-write and must never talk to a model; the
librarian holds it read-only and must talk to the inference engine.  Neither
capability set is a superset of the other, so combining them in one
container would give a single process both the model and the pen.

The smart ops live with the mechanical reads — not in a separate worker —
because they consume the same RAM model and the same visibility filter.
Splitting them would mean a second workspace authority enforcing the same
shield, and drift between two enforcers is exactly the class of bug the
one-authority rule exists to prevent.

## The in-memory model

The librarian's working assertion: **everything the agent is allowed to
read fits in RAM with ease.**  At boot it loads the visible tree into
memory and serves every operation from there — pure Go, stdlib only, zero
subprocesses.  A CI test walks the librarian packages' import graph and
fails on `os/exec` or any non-stdlib dependency; "the librarian cannot
shell out" is true by construction, like the scholar's three-tool menu.

Coherence with the three external writers (scribe edits, builder outputs,
the operator editing on the host).  The seam to close is read-your-writes:
the agent edits file A via the scribe, then reads or greps via the
librarian, and must see its own write.

- **Preferred: an inotify watcher.**  The containers are Linux, so the
  watcher can use the stdlib `syscall` inotify surface directly — the
  third-party fsnotify package would need a written dependency
  justification, and the concept doesn't require it.  The open question is
  the mount backend: Docker Desktop shares a Windows host path through a
  FUSE/virtio layer where events for changes made outside the container
  (another container's writes, host edits) are historically unreliable.
  Before the watcher is trusted, an experiment must show scribe-container
  writes, builder-container writes, and host edits each firing events in a
  watching container on the real mount type.  With a trusted watcher the
  background rescan relaxes to an hourly sweep.
- **Fallback: on-access revalidation.**  If events prove unreliable,
  single-file ops `stat` the backing file per access and reload on
  mtime/size change; tree-wide ops (search, glob, tree) run a
  metadata-only walk first, behind a short cooldown (~1–2 s); a
  once-a-minute background rescan keeps the model warm.

**Memory cap, fail loud.**  The resident set has a configured byte budget.
Over budget at boot: refuse to start, naming the largest offenders — the
signal the operator uses to tune `.aiignore`/`.gitignore` exemptions.  Over
budget mid-session (the workspace grew): deny loading the newly seen files
with an error naming the cap and surface a status-page warning; a live
session is never killed for growth it did not cause.

## Visibility and the shield

Two content states only: **allowed** and **forbidden**.  No gated reads for
now; the write side already has approval gates, and read gating can be
revisited later.

- **`.aiignore` — "part of the project, but shielded."**  Matched files
  appear in listings by name with their read and execute permission bits
  stripped (the listing API surfaces POSIX `rwx` bits, so the agent can see
  that a file exists and that it may not read it).  Any operation that
  explicitly names a jailed path is rejected immediately and audited.
  Tree-wide operations skip jailed files silently — a grep simply does not
  search them, which is not a denial.
- **`.gitignore` — "not part of the project."**  Build outputs, caches,
  generated trees.  These are invisible: absent from listings, trees,
  globs, and searches.  An explicit read still denies.  The working theory
  is that "don't version it" implies "the agent needn't read it"; if a
  workflow proves to need build-output inspection, that becomes a
  tightly-scoped additional tool, not a loosened shield.
- Build outputs must be covered by the union of the two files, and the RAM
  cap enforces honesty: an uncovered output tree blows the budget at boot.
- **`.aiignore` itself is always readable** — the agent may know its own
  constraints — and its edits join the gated-write sensitive set (held for
  human approval, like build logic).  Without the gate the agent could
  unshield itself.
- Shield changes apply **immediately and silently** at rescan.  Content
  already in the agent's context is water under the bridge.
- The shield is one shared package (`internal/shield`: parse both ignore
  files, answer "may read / may list / must gate").  The librarian applies
  it when serving; the **scribe applies the same package** on the write
  path, because an edit is a covert read — `apply_diff` and `dryRun`
  results echo file content, so mutating a jailed file is refused.  Any
  future multi-file write op skips sensitive files by default and gates
  for approval to include them.

## Audit: denials only

Successful reads are not audited — the volume would change the ledger's
character, and the interesting event is refusal.  A new typed audit detail
records each denial: the denied path or paths (batch ops may deny several)
and the tool that was invoked.  Tool shape only; arguments are not
recorded.

## Tool surface

Bounded ops, not a shell — the read-side mirror of the scribe's write
principle.  Every result is size-capped and paginated; every path is
confined by the same validating resolver the scribe uses.

Mechanical:

| Tool | Replaces | Notes |
|---|---|---|
| `read_file` | `cat` | whole file, caps enforced |
| `read_lines` | `head` / `tail` / `sed -n 'A,Bp'` | ranged reads |
| `stat_file` | `ls -l`, `wc -l`, `sha256sum`, `file` | size, mtime, line count, hash, permission bits |
| `list_dir`, `tree` | `ls`, `find -maxdepth` | permission bits surfaced; depth-limited |
| `glob` | `find -name` | name patterns across the tree |
| `search` | `grep` family | RE2; four modes: matches-with-context, files-with-matches, count-per-file, total count; glob/path filters |
| `batch_read` | `cat a b c` | several files, one call |
| `extract_section` | `sed -n '/a/,/b/p'` | pattern-to-pattern — "read this function" without line numbers |
| `recently_modified` | `ls -t`, `find -newer` | tree by mtime |
| `diff_files` | `diff a b` | two workspace files |
| `count_grouped` | `grep -o \| sort \| uniq -c` | match-frequency table |
| `validate_text` | — | encoding/UTF-8 report, pairing with the scribe's UTF-8 gates |

Inference-backed (same worker, same model, same shield):

- `summarize_file`, `summarize_directory` — map-reduce over resident
  content; context-saving alternatives to reading whole trees.
- `ask_about_file(question)` — one-shot Q&A grounded in the file's bytes.
- `find_relevant_files(question)` — semantic locate ("where is retry
  handled?").
- `explain_change(opId)` — possible later: narrate a stored scribe diff.

The list is a starting catalog, not a contract; the review gate for adding
a tool is that it replaces a unix chain the agent demonstrably wants, at a
higher level of abstraction.

## Inference routing

**Content is pushed, never pulled.**  The inference engines are stateless
completion endpoints: the librarian assembles each query, embedding the
shield-filtered file content it chooses to include, and the model answers
from exactly that.  No engine — local or remote — holds a workspace mount,
a credential, or any way to fetch bytes itself, so the shield is enforced
once, in the librarian, before content leaves the process.

The librarian drives the **existing** infer engine and model — no model
swap.  The client is engine-routed from day one: a named-engine
configuration maps op classes to endpoints, rather than a single hardwired
URL.  Planned second engine: `infernet_big`, an external network to a
LAN "deep think" node (a MacBook), for the heavier comprehension ops.  Its
address arrives via stack env only (the no-LAN-IPs presubmit invariant),
compose-lint must bless exactly which workers may hold that network, and
ops routed to it degrade gracefully when the node is absent — a laptop is
a sometimes-there machine.  Because the push model means allowed workspace
content transits the LAN to that node, which op classes may be routed
off-host is part of the engine configuration, not an implementation
detail.

## Topology

```
librarian:
  volumes:  ${WORKSPACE}:/workspace:ro     # sole reader; the agent has NO mount
  networks:
    - buildnet    # reachable by the agent (inbound MCP, :9400)
    - infernet    # reach infer for the comprehension ops
    - statenet    # denial audits to state; internal -> no egress
```

- The agent's service loses its workspace volume; `working_dir` points at a
  tmpfs stub.  `qwen-mcp-init.mjs` registers `librarian` at
  `http://librarian:9400/mcp` and the coreTools allowlist drops the
  built-in read tools alongside the already-excluded mutators and web
  tools.
- compose-lint grows invariants: the agent mounts no workspace; the
  librarian's mount is `:ro`; the librarian holds no `egress`, `frontend`,
  or `kagiegress` network.
- The scholar remains workspace-free; the librarian remains web-free.  The
  prompt-injection quarantine is untouched: web content and workspace
  content still never share a mediator.

## Phasing

0. Spike: the inotify propagation experiment on the real mount type —
   scribe write, builder write, host edit, each observed (or not) from a
   watching container.  Decides watcher-primary vs revalidation-primary.
1. `internal/shield` — ignore-file parsing and the may-read/may-list/must-
   gate decisions, with the CI import-graph assertion.
2. `internal/repo` (name TBD) — the RAM model: load, revalidate, rescan,
   cap enforcement.
3. Librarian worker mode + mechanical tools; scribe adopts `internal/shield`
   on the write path; denial audit detail.
4. Compose + compose-lint + docs; the agent cutover (mount out, qwen config).
5. Comprehension ops + engine-routed inference client.
6. Later: `infernet_big`, gated reads if ever needed, build-output
   inspection tools if a workflow demands them.
