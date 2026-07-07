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

# Jailed qwen-code agent image.
# The base image ships qwen-code preinstalled; pinning the base tag pins the
# CLI version. QWEN_VERSION is supplied by the build pipeline (images.yml) so
# the image tag always matches the qwen-code version inside it.
ARG QWEN_VERSION=0.19.4
FROM ghcr.io/qwenlm/qwen-code:${QWEN_VERSION}

# Base is node:22-slim, which ships a non-root `node` user (UID 1000).
# Pre-create .qwen owned by node so the named volume mounted there inherits
# that ownership on first use (no chown dance at runtime).
RUN mkdir -p /home/node/.qwen && chown -R node:node /home/node

# Trim bundled skills: remove batch / extension-creator / loop — the skill-level
# form of capabilities the jail denies (sub-agent orchestration, capability-
# scaffolding extensions, self-scheduling loops). Kept: qc-helper, review,
# simplify, stuck, dataviz, new-app. qwen-code has no shipped settings-level skill
# allowlist (QwenLM/qwen-code#2216 is unmerged; skills are controlled by presence
# on disk), so enforce it here. We locate the skills directory by the KEPT
# `qc-helper` skill (guaranteed present, unambiguous name) instead of assuming a
# path, then delete the unwanted siblings. Self-verifying: the build FAILS if
# qc-helper isn't found, so a base-image layout change can't silently no-op.
RUN set -eux; \
    found=0; \
    for marker in $(find / -type d -name qc-helper 2>/dev/null); do \
      found=1; \
      dir="$(dirname "$marker")"; \
      for skill in batch extension-creator loop; do \
        rm -rf "$dir/$skill"; \
      done; \
      echo "kept in $dir ->"; ls "$dir"; \
    done; \
    test "$found" = 1

# Register the builder MCP server into qwen's settings on every start, so no
# per-cell manual step is needed. Done via an entrypoint (not a baked file):
# the qwen_home volume mounts over /home/node/.qwen and would shadow any file
# baked at that path except on a fresh volume. The script merges only the
# managed `builder` entry, preserving other settings/servers/history.
COPY --chown=node:node qwen-mcp-init.mjs /usr/local/bin/qwen-mcp-init.mjs

USER node
ENV HOME=/home/node

# Inline exec-form entrypoint (no shell script → no CRLF-shebang hazard from a
# Windows checkout). $0 is "sh"; the container command arrives as "$@", so the
# config runs first (best-effort via `|| true`) then we exec the real command.
ENTRYPOINT ["/bin/sh", "-c", "node /usr/local/bin/qwen-mcp-init.mjs || true; exec \"$@\"", "sh"]
CMD ["qwen"]
