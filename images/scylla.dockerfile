# scylla — ScyllaDB as a forkd SERVICE image (spoond #70).
#
# Capability, not project: a single-node, developer-mode CQL database that a
# CI job (or any sandbox) leases per run and reaches through `expose_ports`.
# The pinned build matches the one cytale's dev and CI run against.
#
# The image carries a boot hook (/etc/forkd/init.d/, run by forkd-init.sh
# before the agent) that starts ScyllaDB and returns once CQL answers — so the
# bake snapshots it SERVING and every fork restores warm, in milliseconds,
# instead of paying a ~10s cold boot per job.
#
# The RHEL-based upstream image already has python3 (the guest agent's
# interpreter), so the bake passes no --extra (apt would fail here anyway).
FROM scylladb/scylla:2026.2.6
# --chmod, not a RUN chmod: the upstream image's USER is `scylla`, which may
# not chmod a root-owned file.
COPY --chmod=755 scylla-init-hook.sh /etc/forkd/init.d/50-scylla
