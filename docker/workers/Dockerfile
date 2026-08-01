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

# cloister-workers — the slim, toolchain-free multi-call worker image:
# the static cloister-worker binary and its role links.  Toolchains live
# in the agent's workbench image (docker/workbench) and nowhere else; the
# mediator roles (builder, scribe, librarian) retired with the grange
# cutover (docs/grange.md M3/M4).
#
# Alpine base, not scratch: the archivist's provision/publish exec the
# REAL git binary, and git's https transport (git-remote-https) rides the
# curl/TLS stack — impractical to carry statically.  Accepted cost: a
# busybox shell now exists in every worker container.  The jail does not
# lean on its absence — read-only rootfs, cap_drop ALL,
# no-new-privileges, non-root, internal-only networks, dead DNS — and
# git itself is driven only through the archivist's hardened runner
# (hooks off, config allowlist-guarded, env-scrubbed; internal/archive).
#
# The build pipeline (images.yml) cross-compiles the static binary into
# this directory before `docker build`; the binary itself is never
# committed (.gitignore).
FROM alpine:3.22

# git for the archivist; ca-certificates for the scholar's kagi.com TLS
# verification and git's own https.
RUN apk add --no-cache git ca-certificates

COPY cloister-worker /usr/local/bin/cloister-worker

# Role links (agent-builder is the pre-rename compat link), the worker
# user (uid 1000, matching the image-initialized volumes), and
# 1000-owned mount points: Docker copies a mountpoint's image-side
# ownership onto a fresh named volume, so a root-owned /state would be
# unwritable by the non-root state service.
RUN set -eux; \
    cd /usr/local/bin; \
    for role in scholar archivist state-service agency agent-builder; do \
      ln -s cloister-worker "$role"; \
    done; \
    adduser -D -u 1000 -s /sbin/nologin worker; \
    mkdir -p /state /burn /grange /status /agency-status; \
    chown 1000:1000 /state /burn /grange /status /agency-status /home/worker

USER 1000:1000
# The generic entrypoint: compose overrides it per service with a role
# link (entrypoint: ["/usr/local/bin/archivist"]); under this default, a
# leading -worker-mode selects the role.
ENTRYPOINT ["/usr/local/bin/cloister-worker"]
