# scylla — ScyllaDB as a SERVICE image (spoond #70). A job leases it with
# expose_ports [9042]. Debian 12 + ScyllaDB's apt repository (the upstream
# scylladb/scylla image is RHEL UBI, which E2B's template builder rejects).
FROM debian:12
ENV DEBIAN_FRONTEND=noninteractive
# The scylla metapackage requires its sub-packages at exactly its own
# version, but apt resolves them to the newest in the repository, so an
# apt preferences pin holds every scylla* package at one version (a
# 2026.2.8 release broke the build on 2026-10-04). Change the version in
# the pin to upgrade.
COPY scylla-signing-key.gpg scylla-2026.2.list /tmp/
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends ca-certificates python3 \
 && install -d -m 0755 /etc/apt/keyrings \
 && install -m 0644 /tmp/scylla-signing-key.gpg /etc/apt/keyrings/scylladb.gpg \
 && install -m 0644 /tmp/scylla-2026.2.list /etc/apt/sources.list.d/scylla.list \
 && apt-get update -qq \
 && apt-get install -y --no-install-recommends procps \
 && dpkg-divert --local --rename --add /sbin/sysctl \
 && printf '#!/bin/sh\nexit 0\n' > /sbin/sysctl \
 && chmod 0755 /sbin/sysctl \
 && printf 'Package: scylla*\nPin: version 2026.2.7-0.20260902.94dae629230b-1\nPin-Priority: 1001\n' > /etc/apt/preferences.d/scylla \
 && apt-get install -y --no-install-recommends scylla \
 && rm -f /sbin/sysctl \
 && dpkg-divert --local --rename --remove /sbin/sysctl \
 && rm -rf /var/lib/apt/lists/*
COPY --chmod=755 scylla-init-hook.sh /etc/spoond/init.d/50-scylla
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
ARG SPOOND_GUEST_DNS_ADDR=
RUN mkdir -p /etc/spoond/init.d \
 && printf '%s\n' "$SPOOND_GUEST_DNS_ADDR" > /etc/spoond/guest-dns
