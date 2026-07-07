# builder-jdk25-gradle — JDK 25 toolchain + the agent-builder MCP sidecar.
# The build pipeline (images.yml) cross-compiles the static agent-builder
# binary into this directory before `docker build`; the binary itself is
# never committed (.gitignore).
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

COPY agent-builder /usr/local/bin/agent-builder

# amazoncorretto's AL2023 base has no shadow-utils (useradd), and pulling
# it in just to make one static user is more package surface than the jail
# needs — write the passwd/group entries directly instead. /spool, /state,
# /gradle-home, /scribe-state, and /burn are tmpfs/volume mount points that MUST
# pre-exist in the image, created 1000-owned: Docker copies a mountpoint's
# image-side ownership onto a fresh volume, so a path missing here yields a
# root-owned volume the non-root user cannot write (gradlew then fails to create
# its dist/lock dirs under /gradle-home; the scribe fails to stage under
# /scribe-state; the scholar fails to open its burn ledger under /burn).
RUN mkdir -p /etc/agent-builder /home/builder /spool /state /gradle-home /scribe-state /burn \
    && echo "jdk25-gradle" > /etc/agent-builder/toolchain \
    && echo "builder:x:1000:1000::/home/builder:/sbin/nologin" >> /etc/passwd \
    && echo "builder:x:1000:" >> /etc/group \
    && chown 1000:1000 /home/builder /spool /state /gradle-home /scribe-state /burn

# Platform dependency-warmer init script used by the airlock (world-readable;
# not agent-writable). Resolves all configs so offline build/test/coverage work.
COPY warm-deps.gradle /etc/agent-builder/warm-deps.gradle

USER builder
EXPOSE 9200
ENTRYPOINT ["/usr/local/bin/agent-builder"]
