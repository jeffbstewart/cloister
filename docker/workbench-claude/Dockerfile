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

# cloister-workbench-claude — the workbench, plus Claude Code and a trust
# store that holds exactly one certificate (docs/JAILED_CLAUDE.md).
#
# THE RECIPE IS PUBLIC; THE IMAGE IS NOT.  Those are different artifacts and
# the distinction is clean.  claude-code's LICENSE.md reads, in full, "©
# Anthropic PBC.  All rights reserved.  Use is subject to Anthropic's
# Commercial Terms of Service" — all rights reserved means no redistribution
# right, so pushing a built image containing the CLI to a public package is
# out.  A Dockerfile is not a copy: the layer below names a publicly
# installable package that whoever runs the build fetches themselves under
# their own license.  Anthropic publishes its own containerization recipe,
# init-firewall.sh and all, in that same public repository; this is the same
# category of artifact.  So: this file is public, the built image goes to a
# PRIVATE GHCR package (.github/workflows/images.yml).
#
# It is a thin layer on the stock workbench rather than a fork of it.  That
# is the hedge doing its job — qwen and Claude get the SAME toolchains, the
# same git proxy, the same grange, and above all the same environment prompt,
# so the qwen path cannot rot while nobody is looking.  Everything this adds
# is Claude-specific plumbing that qwen does not need.

ARG WORKBENCH_IMAGE=ghcr.io/jeffbstewart/cloister-workbench:latest
FROM ${WORKBENCH_IMAGE}

USER root

# --- the CLI ---------------------------------------------------------------
# Version pins ride the build pipeline (images.yml); the default exists so a
# bare local build produces something sensible.  DISABLE_AUTOUPDATER in the
# cell keeps this pin honest at runtime — there is no route to a registry
# from a cell anyway, so an autoupdater could only fail slowly.
ARG CLAUDE_CLI_VERSION=2.1.222
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CLI_VERSION} \
    && npm cache clean --force

# --- configuration the agent cannot edit -----------------------------------
# Baked, not mounted.  Both files sit on the read-only rootfs owned by root,
# which is the same containment property a read-only bind would give — with
# one fewer host path to keep in step, and versioned with the image that
# reads them.  The agent runs as uid 1000 and cannot touch either.
#
# Heredocs rather than COPYed .json files, for the same two reasons the tmux
# config and the motd are heredocs: build-time generation writes LF bytes no
# matter how a Windows checkout mangled line endings, and JSON has no comment
# syntax — so the only place these choices can be explained is here.
#
# MCP registration mirrors the qwen setup EXACTLY: same two servers, same
# URLs, plain HTTP on internal networks and therefore untouched by any of the
# TLS work around them.  Holding that surface constant is the entire point of
# the diagnostic — a difference here would confound the thing being measured.
COPY <<'EOF' /etc/cloister/claude-mcp.json
{
  "mcpServers": {
    "archivist": { "type": "http", "url": "http://archivist:9600/mcp" },
    "scholar":   { "type": "http", "url": "http://scholar:9500/mcp" }
  }
}
EOF

# The settings file.  Read this list knowing that Claude Code SILENTLY
# IGNORES settings keys it does not recognize — measured, by feeding it
# {"totallyBogusKeyXyz":1} and watching it succeed without a murmur.  So
# "the CLI accepted this file" is evidence of nothing at all, and every key
# below is either load-bearing-and-verified or documentation.  Nothing here
# is treated as a control.
#
#   permissions.deny   DOCUMENTATION OF INTENT, not enforcement.  Measured
#                      inert under bypassPermissions — the mode M3 runs in —
#                      so it states what the agent is supposed to do and
#                      enforces nothing.  The enforcement is the proxy's
#                      path allowlist and its server-side-tool refusal.  It
#                      does bite in attended sessions without the bypass
#                      flag, and it costs nothing, so it stays.
#   cleanupPeriodDays  set deliberately rather than left at the default 30.
#                      Transcripts are the most detailed record of a session
#                      that exists outside the audit trail, and they live in
#                      a directory this cell makes ephemeral anyway.
COPY <<'EOF' /etc/cloister/claude-settings.json
{
  "permissions": {
    "deny": ["WebSearch", "WebFetch"]
  },
  "cleanupPeriodDays": 7
}
EOF

# The fail-closed boot check for THIS variant's two claims: that the trust
# store really holds one certificate, and that the door answers with a
# certificate our CA signed.  The base image's jail-probe still runs first
# and is unchanged — its probes stay valid here, because api.anthropic.com
# resolves through a network ALIAS answered authoritatively by the embedded
# resolver, so cloudflare.com and google.com are as dead as they ever were.
COPY claude-door-probe.mjs /usr/local/bin/claude-door-probe.mjs

# --- the launcher ----------------------------------------------------------
# The Claude arm of what qwen-cloister does, and deliberately its twin: same
# prompt file, same fail-closed refusal, same reason.  An agent running
# without its rules looks entirely normal until it does something expensive.
#
# --append-system-prompt-FILE, note.  The flag is absent from `claude --help`
# and works anyway (docs/JAILED_CLAUDE.md, "The flag that --help denies"), so
# the prompt is passed by PATH: no 128 KiB MAX_ARG_STRLEN ceiling, nothing
# multi-kilobyte in `ps`, and no shell quoting to get exactly right forever.
#
# The other three flags are containment, not convenience:
#   --setting-sources user   excludes .claude/settings.json and .claude/rules/
#                            IN THE CHECKOUT, which in this design is a tree
#                            the agent itself wrote.  Measured to work
#                            (probe step 3).  Better than disableAllHooks,
#                            which would also kill the operator's own
#                            InstructionsLoaded hook — an instrument, not a risk.
#   --strict-mcp-config      loads ONLY the servers named on --mcp-config and
#                            ignores every other source, so the agent cannot
#                            register a server of its own.
#   --settings <path>        a root-owned file, rather than the user settings
#                            the agent could rewrite on its home volume.
COPY <<'EOF' /usr/local/bin/claude-cloister
#!/bin/sh
set -eu
prompt=/usr/local/share/cloister/AGENTS.md
if [ ! -s "$prompt" ]; then
  echo "cloister: FATAL the environment prompt ($prompt) is missing or empty." >&2
  echo "cloister: refusing to start the agent without its rules — it would not know" >&2
  echo "cloister: that git is read-only here, that the archivist owns writes, or that" >&2
  echo "cloister: this cell reaches nothing but the inspected door.  Rebuild the image." >&2
  exit 1
fi
exec claude \
  --append-system-prompt-file "$prompt" \
  --settings /etc/cloister/claude-settings.json \
  --setting-sources user \
  --mcp-config /etc/cloister/claude-mcp.json \
  --strict-mcp-config \
  "$@"
EOF
RUN chmod 0755 /usr/local/bin/claude-cloister

# --- the trust store: exactly one certificate ------------------------------
# LAST, and last on purpose.  This is image-WIDE, not Claude-wide:
# /etc/ssl/certs is the system store for Go, git, curl, npm and python too,
# so any layer after this that wants TLS breaks outright.  Builds in a cell
# are offline (GOPROXY=off, CARGO_NET_OFFLINE, cache-only Gradle), which is
# what makes that survivable — expect to revisit it the first time a build
# wants a network.
#
# Not merely ADDING our root: REMOVING every other one.  That is the second
# guarantee behind the topology.  The cell has no route to anything but the
# proxy, AND no issuer is trusted but ours — so an agent that somehow found a
# path to a real internet host fails validation against a perfectly
# legitimate public certificate, loudly, in a log we read.
#
# cloister-ca.crt must be in the build context.  A missing file fails the
# COPY, which is the correct outcome: an image built without the swap would
# trust the public roots and quietly lose the guarantee, and there is no
# useful "default" to fall back to.
COPY cloister-ca.crt /usr/local/share/ca-certificates/cloister-ca.crt
RUN set -eux; \
    rm -rf /usr/share/ca-certificates/*; \
    rm -f /etc/ssl/certs/*.pem /etc/ssl/certs/ca-certificates.crt; \
    : > /etc/ca-certificates.conf; \
    update-ca-certificates --fresh; \
    # Self-verifying, because the whole claim is a COUNT.  A future base-image
    # bump that reinstalls ca-certificates, or a stray apt layer, would
    # otherwise restore the public roots silently — the image would look
    # identical and the second guarantee would simply be gone.
    n="$(grep -c 'BEGIN CERTIFICATE' /etc/ssl/certs/ca-certificates.crt)"; \
    echo "cloister: system trust store holds $n certificate(s)"; \
    test "$n" = 1
# The JVM keeps its own store (cacerts), which this does not touch.  Builds
# are offline so that is mostly moot; it will still bite the first time
# something in a build wants TLS.

# The banner's Claude arm.  Appended rather than replacing the base's,
# because everything it says still applies — this image is the same cell with
# one more CLI in it.  What changes is the delivery check: the stock line
# tells the operator to grep for --append-system-prompt, and on this arm the
# flag carries a PATH instead.  A health check that reports a false negative
# is worse than none, because the operator acts on it.
RUN cat <<'EOF' >> /etc/motd
  claude       this cell also carries Claude Code, and `workbench` launches
               it (WORKBENCH_AGENT=claude-cloister).  Delivery check for
               this arm:
                 ps -eo args | grep claude
               shows --append-system-prompt-file /usr/local/share/cloister/AGENTS.md
               — the same prompt qwen gets, passed by path rather than by
               value.  Adherence is still the question, not delivery: ask
               what `git commit -m "x"` would do here.
  the door     there is no general internet.  api.anthropic.com resolves to
               an inspected proxy in the abbey that allows /v1/messages and
               nothing else, and refuses tools that would run on Anthropic's
               side of it.  Web research still goes through the scholar.
               If inference fails outright, check the abbey: claude-proxy
               and claude-egress, then `docker logs claude-proxy` — every
               refusal is logged there by name.

EOF

USER agent

# The base image's entrypoint plus this variant's probe.  Restated in full
# rather than appended to, because an ENTRYPOINT cannot be extended — and
# stating it here means a change to the base's arm is a visible conflict when
# this file is next touched, instead of a silent divergence.
#
# Both probes are fail-CLOSED and run before anything else.  Everything after
# them is best-effort: a config hiccup must not stop the CLI.  The qwen MCP
# init still runs — this image carries both CLIs, and the hedge is only real
# if the qwen path stays exercised.
ENTRYPOINT ["/bin/sh", "-c", "node /usr/local/bin/jail-probe.mjs || exit 1; node /usr/local/bin/claude-door-probe.mjs || exit 1; mkdir -p \"$HOME/.qwen\" || echo 'cloister: WARNING cannot create $HOME/.qwen' >&2; node /usr/local/bin/qwen-mcp-init.mjs || true; exec \"$@\"", "sh"]
CMD ["sleep", "infinity"]
