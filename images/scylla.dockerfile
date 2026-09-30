# scylla — ScyllaDB as a SERVICE image (spoond #70). A job leases it with
# expose_ports [9042]. Debian 12 + ScyllaDB's apt repository (the upstream
# scylladb/scylla image is RHEL UBI, which E2B's template builder rejects).
FROM debian:12
ENV DEBIAN_FRONTEND=noninteractive
COPY scylla-signing-key.gpg scylla-2026.2.list /tmp/
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends ca-certificates python3 \
 && install -d -m 0755 /etc/apt/keyrings \
 && install -m 0644 /tmp/scylla-signing-key.gpg /etc/apt/keyrings/scylladb.gpg \
 && install -m 0644 /tmp/scylla-2026.2.list /etc/apt/sources.list.d/scylla.list \
 && apt-get update -qq \
 && apt-get install -y --no-install-recommends scylla=2026.2.6-0.20260824.c06236b53803-1 \
 && rm -rf /var/lib/apt/lists/*
COPY --chmod=755 scylla-init-hook.sh /etc/spoond/init.d/50-scylla
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
RUN mkdir -p /etc/spoond/init.d
