# Copyright 2026 Jeffrey B. Stewart
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# cloister-workbench — the fat, identity-free agent workbench
# (docs/grange.md, milestone M4): every toolchain the cells serve — Go,
# Rust, and the JVM (Java, with Kotlin riding the Gradle wrapper) —
# under the coding-agent CLIs, in ONE image, layered stable-to-volatile
# so a CLI bump rebuilds only the thin top layers.  Deliberately ABSENT:
# Python (excluded by decision) and C++ (cross-platform complexity,
# deferred until unavoidable) — the gcc below is Rust's native linker,
# not C/C++ support.  No identity, no credential, no worker binary: the
# archivist owns VCS and the forge; the agent runs these tools freely
# against the grange mounted rw, and its builds are untrusted scratch
# work (CI is the build of record).
#
# Toolchains arrive as multi-stage COPIES from pinned donor images, each
# glibc-compatible with the bookworm base (bookworm-family donors, plus
# corretto's AL2023 whose glibc 2.34 predates bookworm's 2.36), so the
# binaries run without repackaging and each donor tag is the version pin.
ARG GO_IMAGE=golang:1.25-bookworm
ARG RUST_IMAGE=rust:1-slim-bookworm
ARG JDK_IMAGE=amazoncorretto:25
ARG NODE_IMAGE=node:22-bookworm-slim

FROM ${GO_IMAGE} AS go-donor
FROM ${RUST_IMAGE} AS rust-donor
FROM ${JDK_IMAGE} AS jdk-donor
FROM ${NODE_IMAGE} AS node-donor

FROM debian:bookworm-slim

# git is the agent's own (grange.md "Local git is the agent's": hooks and
# config inside the agent's container execute as the agent, already the
# agent's privilege; the credential and every remote verb stay
# archivist-only).  gcc + libc headers are the native linker every Rust
# build needs — not C++ support.  zip/unzip serve Gradle distributions.
#
# Build systems, by decision: Gradle rides each repo's WRAPPER (gradlew
# pins the version; no system Gradle to skew against it); Go and Cargo
# are their toolchains' own; make is here for shell-glue repos.  Absent
# until a repo demands them: Maven (mvnw would ride the wrapper pattern),
# Bazel (its external-repo fetching fights the offline cell — vendored
# caches are real work, priced when needed), CMake/ninja (out with C++).
# npm builds RUN (node is the CLI runtime) but offline npm-cache warming
# is unwired — close that gap before granting a node project a grange.
# The unix toolbox rides along with the toolchains.  bookworm-slim's
# required set already carries bash, coreutils, sed, grep, findutils
# (find/xargs), tar, gzip, and diffutils; the installs below fill the
# gaps a working shell (and a working agent) actually hits: gawk (slim
# ships at best mawk), procps (ps — tmux triage), less, file, patch,
# xz, and the two tools coding agents reach for constantly, ripgrep
# and jq.  curl is the operator's diagnostic probe (healthz endpoints,
# the agency door) — inside the cell it can only reach what the
# topology already grants, and during the airlock's egress window no
# agent session is running (the airlock's own gate).
# protoc is the protobuf compiler alone — the per-language
# codegen plugins (protoc-gen-go, grpc) are repo-level tools an agent
# builds offline from the warmed module cache, not image freight.
# emacs-nox by operator decree (no X stack rides in with -nox).
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git gcc libc6-dev make zip unzip tmux \
      gawk procps less file patch tree xz-utils ripgrep jq curl \
      protobuf-compiler emacs-nox \
    && rm -rf /var/lib/apt/lists/*

# UTF-8 locale: Kotlin emits .class files whose NAMES carry backtick
# test-name Unicode; under the C/POSIX locale the JVM cannot walk or
# delete them (breaks `clean`).  Same lesson the builder-jvm image
# learned; the JDK's UTF-8 file.encoding does not cover filenames.
ENV LANG=C.UTF-8 LC_ALL=C.UTF-8

# --- toolchains, most stable first ---

# JVM: Java 25, and Kotlin via each project's Gradle wrapper (the
# toolkits are wrapper-built; no standalone kotlinc to version-skew
# against the plugin the wrapper resolves).
COPY --from=jdk-donor /usr/lib/jvm/java-25-amazon-corretto /usr/lib/jvm/java-25-amazon-corretto
ENV JAVA_HOME=/usr/lib/jvm/java-25-amazon-corretto

# Go.  GOTOOLCHAIN=local: never fetch a different toolchain mid-build
# (the cell is offline); GOPROXY=off: a module missing from the warmed
# cache fails immediately and clearly instead of dialing a proxy no
# route serves.
COPY --from=go-donor /usr/local/go /usr/local/go
ENV GOTOOLCHAIN=local GOPROXY=off

# Rust.  The toolchain is system-wide and effectively read-only
# (RUSTUP_HOME pins it); the agent's own cargo state — registry cache,
# the warmed dependency store — rides $HOME.  CARGO_NET_OFFLINE makes
# cache misses a clear refusal, like GOPROXY=off.
COPY --from=rust-donor /usr/local/rustup /usr/local/rustup
COPY --from=rust-donor /usr/local/cargo /usr/local/cargo
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/home/agent/.cargo \
    CARGO_NET_OFFLINE=true

ENV PATH=${JAVA_HOME}/bin:/usr/local/go/bin:/usr/local/cargo/bin:${PATH}

# Node — the runtime under both agent CLIs, not a served toolchain.
# Selective copies so the donor's /usr/local never shadows the
# toolchain trees above; npm/npx are relative symlinks into
# lib/node_modules and survive the copy.
COPY --from=node-donor /usr/local/bin /usr/local/bin
COPY --from=node-donor /usr/local/lib/node_modules /usr/local/lib/node_modules

# The warming handshake: the airlock's JVM dependency-warmer init script
# (world-readable, not agent-writable) and the toolchain id the warming
# marker is keyed on.  The id scheme — and the airlock's post-builder
# rework — are settled in the cutover; "workbench1" is the placeholder
# the first warmed marker will carry.
COPY warm-deps.gradle /usr/local/share/cloister/warm-deps.gradle
RUN mkdir -p /etc/cloister-worker && echo "workbench1" > /etc/cloister-worker/toolchain

# --- the volatile top: the agent CLIs ---

# Version pins ride the build pipeline (images.yml).  The Dockerfile
# defaults exist so a bare local `docker build` produces something
# sensible, but the pipeline's values are the pins of record.
# Claude Code is deliberately ABSENT: it is proprietary (Anthropic's
# commercial terms, not open source), and this image's GHCR package is
# public — baking it in would be public redistribution.  It returns as
# its own PRIVATE image variant when that work is picked up; qwen-code
# and the toolchains are all Apache-2.0/permissive and publish freely.
ARG QWEN_CLI_VERSION=0.19.4
RUN npm install -g \
      @qwen-code/qwen-code@${QWEN_CLI_VERSION} \
    && npm cache clean --force

# Trim qwen's bundled skills: batch / extension-creator / loop are the
# skill-level form of capabilities the jail denies (sub-agent
# orchestration, capability-scaffolding extensions, self-scheduling
# loops).  qwen-code has no settings-level skill allowlist (skills are
# controlled by presence on disk), so enforce it here — located by the
# KEPT qc-helper skill and self-verifying: the build FAILS if qc-helper
# isn't found, so a package-layout change can't silently no-op.
RUN set -eux; \
    found=0; \
    for marker in $(find /usr/local/lib/node_modules -type d -name qc-helper 2>/dev/null); do \
      found=1; \
      dir="$(dirname "$marker")"; \
      for skill in batch extension-creator loop; do \
        rm -rf "$dir/$skill"; \
      done; \
      echo "kept in $dir ->"; ls "$dir"; \
    done; \
    test "$found" = 1

# The stock environment prompt: how the grange, the archivist, and the
# structurally-offline cell work — one canonical file, materialized by
# the entrypoint into each CLI's HOME config location (~/.qwen/QWEN.md
# today; future agent variants add theirs) at every start, never into
# the working tree: the cwd is the CHECKOUT now, and a context file
# dropped there would pollute the tree the agent commits from.  Home-dir
# volumes would shadow a baked copy, hence materialize-at-start,
# refreshed each start so an image upgrade updates the prompt.
COPY AGENTS.md /usr/local/share/cloister/AGENTS.md

# The MCP registration: archivist (version control + the PR flow) and
# scholar (grounded research) merged into qwen's settings at every
# start — and the retired mediators' stale entries scrubbed.  Runs from
# the entrypoint because the qwen_home volume shadows any baked file.
COPY qwen-mcp-init.mjs /usr/local/bin/qwen-mcp-init.mjs

# The fail-closed boot self-check (the scholar's pattern): the network
# jail is effective, or the container refuses to start.
COPY jail-probe.mjs /usr/local/bin/jail-probe.mjs

# The identity-free non-root user, uid 1000 to match the image-owned
# grange volume.  The grange mount itself lives in compose
# (docker/ai-workers.yaml); this image stays pure capability.
# A REAL login shell, unlike the slim workers image this was copied
# from: there, nologin is right (a scratch worker never needs one);
# here it broke everything.  tmux takes `default-shell` from $SHELL or,
# failing that, the passwd entry — and `docker exec` sets no SHELL — so
# every tmux command ran as `nologin -c '...'`, which prints "This
# account is currently not available", exits, and takes the pane and
# session with it before the message can be read.  The agent having a
# shell is the entire point of the workbench (docs/grange.md M3).
ENV SHELL=/bin/bash
RUN useradd --uid 1000 --create-home --shell /bin/bash agent \
    && mkdir -p /home/agent/.qwen \
    && chown -R 1000:1000 /home/agent
# The operator's session door: agents run under tmux so their lifetime
# is decoupled from the attachment — a dropped Portainer tab or SSH hop
# no longer kills the agent mid-task.  `workbench` attaches to the live
# session when one exists, else asks which agent to start.  Heredocs
# (BuildKit), not COPY: build-time generation writes LF bytes no matter
# how a Windows checkout mangled line endings — the same CRLF-shebang
# hazard the agent image dodged by having no scripts at all.
COPY <<'EOF' /etc/tmux.conf
# The operator's prefix is Ctrl-Z (not tmux's default C-b).  Ctrl-Z
# collides with shell job control by design of the preference: press
# the prefix TWICE to pass a literal suspend through to the program.
unbind C-b
set -g prefix C-z
bind C-z send-prefix
# Explicit, not inherited: tmux otherwise takes the shell from $SHELL or
# the passwd entry, and a nologin there kills every pane instantly (with
# the error scrolling past as the session dies).  Stating it here means
# the session survives a future change to the image's user.
set -g default-shell /bin/bash
# `screen-256color` has terminfo in ncurses-base, which slim images
# carry; `tmux-256color` needs ncurses-term, which they do not.
set -g default-terminal "screen-256color"
# Agent sessions are long and scrollback is the operator's audit trail
# of the conversation; keep plenty.
set -g history-limit 50000
EOF

# The session manager (cmd/workbench), which replaced the shell script
# that used to live here.  It owns the workspace's LIFETIME — provision,
# run the agent, dispose — because those verbs left the agent's MCP
# surface (docs/archivist.md, "Two surfaces"): a workspace swapped under
# a live session leaves the model reasoning about a repository that is no
# longer there.  Static (CGO_ENABLED=0), cross-compiled into this build
# context by images.yml, so there is no toolchain or CRLF-shebang
# exposure in the image itself.
COPY cloister-workbench /usr/local/bin/workbench
RUN chmod 0755 /usr/local/bin/workbench

# The git proxy (docs/git-proxy.md): reads run the real binary
# unchanged, the closed core of ref-moving commands is translated into
# archivist verbs and ANNOUNCED on stderr, everything else is refused
# with a reason.  Visible translation, never illusion — an agent that
# thought `commit --amend` had rewritten history would reason from that
# for the rest of the session.
#
# The real binary moves to an unadvertised path only the proxy names.
# That is obscurity, not enforcement: /usr/lib/git-core/git is a
# hardlink to the same inode and must stay (git needs GIT_EXEC_PATH to
# find git-remote-https and friends), so a determined process still
# finds it.  What it buys is that the wrong move is no longer the
# reflexive one.
COPY cloister-git-proxy /usr/local/bin/cloister-git-proxy
RUN mkdir -p /usr/lib/cloister/libexec \
    && mv /usr/bin/git /usr/lib/cloister/libexec/gx \
    && mv /usr/local/bin/cloister-git-proxy /usr/bin/git \
    && chmod 0755 /usr/bin/git /usr/lib/cloister/libexec/gx \
    # Fail the build if the proxy is not what `git` resolves to, or if
    # the real binary did not survive the move.  A silently-unproxied
    # cell would look identical from outside.
    && [ "$(command -v git)" = /usr/bin/git ] \
    && /usr/lib/cloister/libexec/gx --version > /dev/null

# The how-do-I-drive-this banner.  /etc/motd alone only greets PAM
# LOGIN shells, and a `docker exec -it ... bash` is not one — so the
# same banner is also shown by /etc/bash.bashrc, which Debian's bash
# reads for every interactive shell (guarded to once per shell, TTY
# only, and skipped inside tmux — the operator already found the door).
COPY <<'EOF' /etc/motd

cloister workbench — jailed agent cell
  workbench    the session manager: pick a repository, provision it,
               run the agent, dispose when the task is done.  Attaches
               instead when a session is already live.  Sessions survive
               disconnects: close this shell freely, the agent keeps
               working.  tmux prefix is Ctrl-Z (press it twice to send a
               literal Ctrl-Z through).
  workspace    /grange/tree — absent until `workbench` provisions it.
               The agent CANNOT provision or dispose: the workspace's
               lifetime is yours, not the model's.  Its own rules are in
               ~/.qwen/QWEN.md.
  offline      no internet by design; package registries refuse.
               Missing dependencies go through the operator airlock.

EOF
RUN cat <<'EOF' >> /etc/bash.bashrc

# cloister: the workbench banner, once per interactive shell.
if [ -n "$PS1" ] && [ -t 1 ] && [ -z "${TMUX:-}" ] && [ -z "${CLOISTER_BANNER:-}" ]; then
    export CLOISTER_BANNER=1
    cat /etc/motd
fi
EOF

USER agent
ENV HOME=/home/agent
WORKDIR /home/agent

# Inline exec-form entrypoint (no shell script -> no CRLF-shebang hazard
# from a Windows checkout).  The jail probe runs FIRST and is
# fail-CLOSED — a reachable internet or live external DNS refuses the
# boot outright; everything after it (prompt copy, MCP registration) is
# best-effort, because a config hiccup must not stop the CLI.
ENTRYPOINT ["/bin/sh", "-c", "node /usr/local/bin/jail-probe.mjs || exit 1; mkdir -p \"$HOME/.qwen\" 2>/dev/null || true; cp /usr/local/share/cloister/AGENTS.md \"$HOME/.qwen/QWEN.md\" 2>/dev/null || true; node /usr/local/bin/qwen-mcp-init.mjs || true; exec \"$@\"", "sh"]
# The container idles; work starts when the operator runs `workbench` in
# an exec shell — it provisions the workspace and launches the agent into
# a tmux session that outlives the shell that started it.
CMD ["sleep", "infinity"]
