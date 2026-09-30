# scylla — ScyllaDB as a SERVICE image (spoond #70). A job leases it with
# expose_ports [9042]. Debian 12 + ScyllaDB's apt repository (the upstream
# scylladb/scylla image is RHEL UBI, which E2B's template builder rejects).
FROM debian:12
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg python3 \
 && install -d -m 0755 /etc/apt/keyrings \
 && curl -fsSL 'https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x6C6ECC84F42AF147BD2A65AEC503C686B007F39E' \
      | gpg --homedir /tmp --no-default-keyring --keyring /etc/apt/keyrings/scylladb.gpg --dearmor --import \
 && gpg --no-default-keyring --keyring /etc/apt/keyrings/scylladb.gpg --list-keys --with-colons \
      | grep -q '^fpr:::::::::6C6ECC84F42AF147BD2A65AEC503C686B007F39E:' \
 && curl -fsSL -o /etc/apt/sources.list.d/scylla.list https://downloads.scylladb.com/deb/debian/scylla-2026.2.list \
 && apt-get update -qq \
 && apt-get install -y --no-install-recommends scylla=2026.2.6-0.20260824.c06236b53803-1 \
 && rm -rf /var/lib/apt/lists/*
COPY --chmod=755 scylla-init-hook.sh /etc/spoond/init.d/50-scylla
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
RUN mkdir -p /etc/spoond/init.d
