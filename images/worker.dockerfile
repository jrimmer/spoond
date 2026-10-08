# worker — the hive's worker layer: turns any catalog image into
# <base>-worker, an image a bee runs in. A bee takes tasks over Agent
# Mail, implements and verifies them with Pi, pushes the task branch and
# reports (images/worker-start.sh). Built with spoond images build from
# a manifest entry with `from: <base>` (worker.manifest.yaml), which
# passes the base's digest as BASE and its runtime env as WARM_ENV.
#
# Nothing project- or model-specific is baked in except the optional
# warm step; per-bee credentials and model choices arrive in the exec
# environment at start.
#
# Build context (assembled by the builder):
#   pi-linux-x64.tar.gz   Pi release, checksum-verified on the build host
#   agent-hub/bin/amail   Agent Mail CLI
#   worker-start.sh       the bee loop
#   worker-git.sh         the rebase and migration rules the loop sources
#   project/              snapshot of the project's default branch for
#                         the warm step (may be empty)
ARG BASE
FROM ${BASE}
# resolveBase always passes SPOOND_GUEST_DNS_ADDR. The worker does not
# re-bake the resolver: the FROM base already wrote /etc/spoond/guest-dns
# from it. Declaring the ARG here keeps the build-arg from warning as an
# unused variable, but it is intentionally not consumed in this layer.
ARG SPOOND_GUEST_DNS_ADDR=

# The layer installs with apt: every catalog base is Debian or Ubuntu.
ENV DEBIAN_FRONTEND=noninteractive
RUN command -v apt-get >/dev/null || { echo "worker layer: base has no apt-get" >&2; exit 1; } \
 && apt-get update -qq \
 && apt-get install -y --no-install-recommends \
      git ca-certificates curl python3 jq openssh-client shellcheck \
 && rm -rf /var/lib/apt/lists/*

# Pi (standalone build; its assets must stay next to the binary).
COPY pi-linux-x64.tar.gz /tmp/pi.tgz
RUN mkdir -p /opt/pi \
 && tar xzf /tmp/pi.tgz -C /opt/pi --strip-components=1 \
 && rm /tmp/pi.tgz \
 && ln -sf /opt/pi/pi /usr/local/bin/pi

COPY --chmod=755 agent-hub/bin/amail /usr/local/bin/amail
COPY --chmod=755 worker-start.sh /usr/local/bin/worker-start
COPY --chmod=755 worker-git.sh /usr/local/bin/worker-git.sh

# Warm: bees have no public egress, so the project's dependencies are
# fetched here, once, by the project's warm command (hive.yaml `warm:`),
# run in a snapshot of its default branch with the lease's runtime env.
# The snapshot is removed afterwards: only caches outside the checkout
# (Go's module cache, Hex's package cache, ...) survive into the image.
ARG WARM=""
ARG WARM_ENV=""
COPY project/ /tmp/project/
RUN if [ -n "$WARM" ]; then eval "$WARM_ENV"; cd /tmp/project && sh -ec "$WARM"; fi \
 && rm -rf /tmp/project

# Fail the build loudly if anything a bee needs is missing.
RUN for b in git pi python3 amail worker-start ssh jq curl shellcheck; do \
      command -v "$b" >/dev/null || { echo "MISSING TOOL: $b" >&2; exit 1; }; \
    done \
 && test -r /usr/local/bin/worker-git.sh \
 && pi --version \
 && test -x /usr/local/bin/spoond-guest-init
