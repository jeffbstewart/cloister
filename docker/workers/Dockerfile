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

# cloister-workers — the slim, toolchain-free multi-call worker image: the
# static cloister-worker binary and its role links on a scratch base
# (docs/toolchains.md).  Toolchains live in per-ecosystem builder images
# (docker/toolchains/); this image deliberately ships NO
# /etc/cloister-worker/toolchain id, so running the builder role on it
# fails loudly at startup instead of at first build.
#
# The build pipeline (images.yml) cross-compiles the static binary into
# this directory before `docker build`; the binary itself is never
# committed (.gitignore).

# Prep stage: the artifacts scratch cannot create itself.
#   - CA roots: the scholar verifies kagi.com's certificate against the
#     system pool; without a bundle every research call fails x509.
#   - passwd/group: a name for uid 1000 so user lookups resolve.
#   - role symlinks (not hard links: COPY between stages is guaranteed to
#     preserve symlinks; expanded hard links would ship one binary copy
#     per role).  agent-builder is the pre-rename compat link.
#   - 1000-owned mount points: Docker copies a mountpoint's image-side
#     ownership onto a fresh named volume, so a root-owned /state would be
#     unwritable by the non-root state service.
FROM alpine:3.22 AS prep
RUN apk add --no-cache ca-certificates
COPY cloister-worker /out/usr/local/bin/cloister-worker
RUN set -eux; \
    cd /out/usr/local/bin; \
    for role in builder scribe scholar librarian state-service agency agent-builder; do \
      ln -s cloister-worker "$role"; \
    done; \
    mkdir -p /out/etc/ssl/certs /out/tmp /out/spool /out/state /out/scribe-state /out/burn /out/workspace /out/home/worker; \
    cp /etc/ssl/certs/ca-certificates.crt /out/etc/ssl/certs/; \
    echo "worker:x:1000:1000::/home/worker:/sbin/nologin" > /out/etc/passwd; \
    echo "worker:x:1000:" > /out/etc/group; \
    chown -R 1000:1000 /out/spool /out/state /out/scribe-state /out/burn /out/home/worker

FROM scratch
COPY --from=prep /out/ /
USER 1000:1000
# The generic entrypoint: compose overrides it per service with a role
# link (entrypoint: ["/usr/local/bin/scribe"]); under this default, a
# leading -worker-mode selects the role.
ENTRYPOINT ["/usr/local/bin/cloister-worker"]
