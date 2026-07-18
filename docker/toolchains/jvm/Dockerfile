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

# cloister-builder-jvm — the JVM ecosystem builder toolchain: JDK 25 plus
# Gradle-wrapper support, with the cloister-worker binary serving the
# builder role (docs/toolchains.md).  ONLY the cell's builder service runs
# a toolchain image; every other role runs the slim cloister-workers
# image.  The build pipeline (images.yml) cross-compiles the static binary
# into this directory before `docker build`; never committed (.gitignore).
FROM amazoncorretto:25

# UTF-8 locale. Kotlin allows backtick-quoted test names with any Unicode, so the
# compiler emits .class files whose NAMES carry those bytes (e.g. an em-dash in a
# test method name). The JVM decodes filenames via sun.jnu.encoding, which
# follows the OS locale; under the default C/POSIX (ASCII) locale, walking or
# deleting such a file throws "Malformed input or input contains unmappable
# characters" — which breaks `clean` and any task that scans build/. JDK 25's
# UTF-8 file.encoding default does NOT cover filenames. A UTF-8 locale does.
ENV LANG=C.UTF-8 LC_ALL=C.UTF-8

# The Gradle 9 wrapper (gradlew) hard-requires `xargs` to parse JVM options
# and aborts with "xargs is not available" without it. AL2023 (corretto's
# base) ships no findutils, so install it. Clean the dnf cache to keep the
# layer lean; this is the only package the toolchain adds beyond the JDK.
RUN dnf install -y findutils \
    && dnf clean all \
    && rm -rf /var/cache/dnf

COPY cloister-worker /usr/local/bin/cloister-worker

# The builder role link (this image runs exactly one role) plus the
# pre-rename agent-builder compat link old compose HEALTHCHECKs name.
RUN ln /usr/local/bin/cloister-worker /usr/local/bin/builder \
    && ln /usr/local/bin/cloister-worker /usr/local/bin/agent-builder

# amazoncorretto's AL2023 base has no shadow-utils (useradd), and pulling
# it in just to make one static user is more package surface than the jail
# needs — write the passwd/group entries directly instead.  /spool is a
# tmpfs mount point that must pre-exist; the toolchain id is the value the
# project manifest's `toolchain:` field must match (fail-closed handshake
# in internal/manifest).
RUN mkdir -p /etc/cloister-worker /home/builder /spool \
    && echo "jdk25-gradle" > /etc/cloister-worker/toolchain \
    && echo "builder:x:1000:1000::/home/builder:/sbin/nologin" >> /etc/passwd \
    && echo "builder:x:1000:" >> /etc/group \
    && chown 1000:1000 /home/builder /spool

# Platform dependency-warmer init script used by the airlock (world-readable;
# not agent-writable). Resolves all configs so offline build/test/coverage work.
COPY warm-deps.gradle /etc/cloister-worker/warm-deps.gradle

USER builder
EXPOSE 9200
# The generic entrypoint: compose overrides it with the builder role link;
# under this default, a leading -worker-mode selects the role.
ENTRYPOINT ["/usr/local/bin/cloister-worker"]
