#!/bin/bash
# /etc/spoond/init.d/50-scylla — start ScyllaDB at guest boot (spoond #70).
#
# Runs as a spoond-guest-init hook, before the agent: starts the server detached
# and returns once CQL answers, so a bake snapshots it serving.
#
# Flag syntax matters: the scylla wrapper takes `--flag value` or
# `--flag=value`, but seastar's own options (--smp, --memory) must use `=`, and
# --overprovisioned is a bare flag — `--smp 1` breaks seastar's parser with
# "sstring out of range", the same message a missing cgroup2 mount produces.
#
#   developer-mode    no io/XFS tuning checks (a microVM ext4 rootfs)
#   memory=1200M      seastar reserves RAM for the OS: in the 3 GiB guest only
#                     ~1.37 GiB is left, so 1536M fails "insufficient physical
#                     memory" (measured). 1200M is also the dev-recipe value.
#   overprovisioned   no busy-polling: idle pool children stay cheap
#   tablets disabled  schemas using SimpleStrategy are rejected under tablets
#   rpc 0.0.0.0       reachable through the lease's expose_ports DNAT
set -u

mkdir -p /var/lib/scylla/data /var/lib/scylla/commitlog /var/lib/scylla/hints /var/lib/scylla/view_hints

nohup setsid /usr/bin/scylla \
  --options-file /etc/scylla/scylla.yaml \
  --developer-mode=1 --smp=1 --memory=1200M --overprovisioned \
  --tablets-mode-for-new-keyspaces=disabled \
  --listen-address=127.0.0.1 --rpc-address=0.0.0.0 --broadcast-rpc-address=169.254.0.21 \
  --seed-provider-parameters=seeds=127.0.0.1 --api-address=127.0.0.1 \
  >/tmp/scylla.log 2>&1 </dev/null &
echo $! >/tmp/scylla.pid

# Ready = the CQL port accepts. Bounded well inside the guest-init hook timeout.
for _ in $(seq 1 150); do
  if (exec 3<>/dev/tcp/127.0.0.1/9042) 2>/dev/null; then
    echo "scylla: CQL up"
    exit 0
  fi
  kill -0 "$(cat /tmp/scylla.pid)" 2>/dev/null || { echo "scylla: exited during start"; tail -20 /tmp/scylla.log; exit 1; }
  sleep 1
done
echo "scylla: CQL not up after 150s"
tail -20 /tmp/scylla.log
exit 1
