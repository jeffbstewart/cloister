# The git proxy

*Status: BUILT — `cmd/git-proxy`, installed as `/usr/bin/git` in the
agent image.  The prompt-level rule it enforces is
docker/workbench/AGENTS.md, "read with git, write with the archivist".*

## What it does

One question, asked of every invocation: **is this a git command we
positively know to be read-only?**  If yes, the real git runs
unchanged.  Everything else is refused with a reason naming the
archivist MCP tool to call instead.

There is no third answer.

## Why there is no third answer

The first version of this proxy had one.  It translated a "closed core"
of mutating commands — `commit`, `push`, `checkout`, `branch`, `stash`,
`restore`, `pull` — into archivist MCP tool calls, announced each
translation on stderr, and refused the rest.  The reasoning was that a
*visible* translation buys convenience without the epistemic cost of an
illusion.

Six independent review passes over that design found the same class of
defect in every corner of it:

| invocation | what it did | what it should have done |
|---|---|---|
| `git checkout main -- file.go` | switched branches (moved HEAD) | restored one file |
| `git commit -m title -m body` | recorded "body", dropped "title" | subject + body |
| `git merge other-branch` | synced the *default* branch | merged `other-branch` |
| `git restore a.txt b.txt` | restored `a.txt` only | both |
| `git branch -d one two` | deleted `two` only | both |
| `git push origin other` | published the *current* branch | pushed `other` |
| `git checkout -b x main` | branched off the default | branched off `main` |
| `git stash push -m wip -- a.txt` | parked the **whole tree** | parked one file |

Every one of those reported success.

The lesson is not "those were bugs, fix them."  It is that git's
argument grammar is rich enough that a translator either reimplements it
faithfully — flag arity, positional shapes, `--` handling, abbreviation
matching, clustered short options — or silently performs a *different
operation* than the one asked for.  Faithful reimplementation is not a
thing to maintain in a shim.  And a translation that is merely *usually*
right is worse than no translation, because it converts a loud failure
("that command doesn't work here") into a quiet one ("it worked", while
the tree says otherwise).

That is precisely the divergence-of-belief the whole system exists to
prevent, arriving by a new road.  The convenience was not worth it: the
agent already has the archivist's tools, and calling one directly costs
a tool call and returns the tool's real answer instead of a rendering.

**A refusal cannot silently do the wrong thing.**  That is the entire
argument.

## The pass set

`reads` in `classify.go` is a closed allow-list of commands that cannot
alter the repository under *any* arguments.  Anything absent is refused
without further examination, so a git version that adds a command — or
a command whose writing form we overlooked — fails closed.

Some commands are worth having for their read forms alone and carry
write forms that must not ride in with them.  Each decides for itself,
defaulting to refuse: `branch` (listing only), `config` (`--get*`,
`--list`), `remote` (`-v`, `show`, `get-url`), `stash` (`list`, `show`),
`reflog` (`show`), `symbolic-ref` (one operand), `notes`, `worktree`,
`submodule`, `bundle` (all listing/verify only).

Two narrower gates apply to commands that would otherwise pass:

- **`writeFlags`** — `--output=`, `-O`, `--open-files-in-pager`,
  `--ext-diff` turn a read into a file write or a program launch.
  Refused wherever they appear.
- **`-c` config keys** — `core.pager`, `diff.external`, `alias.*`, and
  friends make `git log` arbitrary execution wearing git's name.  The
  deny-list is in `execConfig`.  The agent already has a shell, so this
  prevents **concealment**, not execution: a command that runs should
  look like what it is in the transcript.

## Global options are parsed strictly

`git --namespace log commit -am x` used to classify as `log`, pass as a
read, and perform a real commit that nothing logged — because the
parser skipped unrecognized leading flags as though they were switches,
so a value-taking option's *value* landed in the subcommand slot.

Now every unrecognized leading token is a refusal.  A leading token
whose arity we do not know is a token that may be hiding the
subcommand, so the only safe reading is to stop.  `-C` and `-c` are
understood (with the config deny-list above); the valueless switches
are listed in `globalSwitches`; everything else refuses.

## Logging

Refusals and passthrough runs are logged; passes are not.  Reads are the
high-volume, zero-signal case — every `go build` stamps VCS info — and
recording them would bury the refusals, which are the point.  A refusal
says the agent wanted something not on offer: a candidate for a new
archivist tool, a new read to allow, or a line in the environment
prompt.  Together they are a curriculum for what to build next.

`$HOME/.cloister/git-proxy.log`, on the per-project volume, which
outlives the grange.  Arguments are `%q`-quoted so a newline inside a
commit message cannot forge a record — this log is meant to be read by
a parser eventually.

## The escape hatch is a file, not an environment variable

`/etc/cloister/git-passthrough` disables the proxy entirely — the
operator's out for the first build that trips on a command we did not
anticipate.

It is a file **the agent cannot create**.  An environment variable —
the first design — was settable by the agent itself, documented in the
very repository the agent is often granted, and inherited by every
subprocess: one `export` in a build script and supervision was off for
the session.

The mechanics matter, and the first attempt got them wrong.  The cell
runs `read_only: true`, which is read-only for **root as well**, so
`docker exec -u 0 … touch /etc/cloister/git-passthrough` fails and the
hatch is inert.  `docker/cell.yaml` therefore gives `/etc/cloister` its
own tmpfs at `mode=0755`: root owns the mount, so the operator can
write it and the agent (uid 1000) cannot.  A default tmpfs is mode
1777, which would hand the agent the power to switch off its own
supervision — precisely what moving off the environment variable was
meant to prevent.

To use it:

```sh
docker exec -u 0 <project>-agent touch /etc/cloister/git-passthrough
```

It is tmpfs, so it does not survive a container restart — which is the
right default for a temporary loosening.

When it is in force, the proxy says so on stderr and logs it.  This is
the one path where the proxy stands aside, so it is the last place that
should be silent.

## It must never invoke itself

The proxy is installed **as** `git`.  Anything that resolves the real
binary back to this program forks without bound — and this is not a
hypothetical: a development rig pointed it at a PATH-relative `git`,
and the machine was carrying eleven thousand processes before anyone
noticed.

Three guards, because they fail in different ways:

- **`realGit` must be an absolute path.**  A bare name would be
  resolved through PATH, which finds the proxy.
- **`realGit` must not be this program**, compared with `os.SameFile`
  rather than by path — a hardlink or a copy under another name is
  still the same binary, and path comparison would miss it.
- **A depth counter in the environment** (`CLOISTER_GIT_PROXY_DEPTH`),
  because the first two only stop the direct loop.  The indirect ones
  are invisible to the proxy: git's own script subcommands shell out to
  `git` by name, and so can a pager, an alias, or a repository hook.
  Legitimate nesting is shallow — `git-submodule` calling `rev-parse`
  is depth 2 — so the cap of 8 separates it cleanly from a runaway,
  which reaches the cap in milliseconds.

The depth check runs **before the escape hatch**, since passthrough is
the mode that execs most eagerly, and an unreadable counter is treated
as the cap: a garbled value means something is manipulating the
environment, and stopping is the safe reading.

## Not a boundary

The real binary moves to `/usr/lib/cloister/libexec/gx`, a path only the
proxy names.  This is **obscurity, not enforcement**, and one detail
makes that concrete: Debian's `/usr/lib/git-core/git` is a *hardlink to
the same inode*, and it has to stay — git needs `GIT_EXEC_PATH` to find
`git-remote-https` and the other helpers.  So the real binary remains
reachable at a well-known path by anything that looks.

The same goes for the environment: the child inherits `GIT_DIR`,
`GIT_PAGER`, `GIT_EXTERNAL_DIFF` and friends, several of which turn a
read into execution or retarget which repository it reads.  Sanitizing
them would break legitimate build tooling for no gain against a model
that has bash — the honest description is that the proxy shapes what is
*reflexive*, not what is *possible*.

The load-bearing boundaries are elsewhere and unchanged: the forge
ruleset, the endpoint allowlist, and the bot credential living only in
the archivist.

The image build asserts that `git` answers `--cloister-proxy-version`,
which is the only way to tell the proxy from the real binary at the
install site — `git version` passes straight through and so proves
nothing.  The check runs in the **last** layer, because a later
`apt-get install --reinstall git` would otherwise satisfy any earlier
one.

## Three gits, on purpose

`TestPorcelainCoverage` asks the real `git` what commands it has and
fails when one reaches the catch-all unclassified.  The gits differ,
and that is the point rather than a problem:

| where | git | role |
|---|---|---|
| CI | whatever is current | the early-warning system |
| a developer's machine | whatever they have | incidental |
| the image | bookworm's 2.39 | what actually runs |

CI runs **ahead** of the image, so a new command arrives as a red test
months before a base-image bump would ship it.  That is how `backfill`
(2.49) and `history` (2.54) were classified — CI saw them; neither
exists in the image's git yet.

Because of the skew, the test fails only on commands **this** git has
that nobody has classified.  A pinned entry the local git lacks is
logged, not failed — otherwise the test would only be runnable on one
exact git version.

## What breaks first, and how you find out

Not a new git command — an *existing read that was never listed*, and
the symptom is a failed build.  The refusal names the command and the
log records it, so the fix is usually one line in `reads` plus a case in
`classify_test.go`.  Until that lands, the escape hatch unblocks the
operator in seconds.

Maintenance note: the tool names in refusal text are free-form strings.
Renaming or removing an archivist MCP tool leaves this file pointing at
a tool that no longer exists, discovered by a reader rather than a
compiler.  `TestVerbsAreNamedAsArchivistToolsNotShellCommands` pins the
*form* of those mentions but not their existence.
