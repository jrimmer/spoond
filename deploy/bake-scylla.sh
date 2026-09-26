#!/bin/bash
# Bake the `scylla` service snapshot (spoond #70): ScyllaDB 2026.2.6, single
# node, developer mode, started by an /etc/forkd/init.d hook so the snapshot
# holds it SERVING — every fork restores with CQL already up.
#
# Runs ON THE FORKD HOST (sandbox). Needs, next to this script's copies in
# /var/cache/forkd: images/scylla.dockerfile + images/scylla-init-hook.sh
# from this repo, and deploy/rootfs-init/forkd-init.sh installed at
# /usr/local/share/rootfs-init/ (the hook runner + cgroup2 mount; without
# them the guest boots, never starts ScyllaDB, and the verify below fails).
#
# Same shape and gotchas as bake-elixir-release.sh: rm the cached ext4 (the
# conversion cache keys on the image TAG) and rmi the tag first (a
# re-registered live tag keeps the daemon's old rootfs fd).
set -e -o pipefail
cd /var/cache/forkd
export FORKD_SCRIPTS_DIR=/usr/local/share/forkd-scripts

grep -q "/etc/forkd/init.d" /usr/local/share/rootfs-init/forkd-init.sh || {
  echo "installed forkd-init.sh has no init.d hook runner — install deploy/rootfs-init/forkd-init.sh first"; exit 1; }

echo "=== 1. build service image (docker) ==="
DOCKER_BUILDKIT=1 docker build --network=host --security-opt seccomp=unconfined \
  -f scylla.dockerfile -t scylla-service:local /var/cache/forkd

echo "=== 2. from-image -> ext4 -> boot (hook starts scylla) -> snapshot ==="
if [ "${KEEP_CACHE:-0}" != "1" ]; then rm -f /var/cache/forkd/scylla-service-local-*.ext4*; fi
forkd rmi scylla 2>&1 | tail -2 || true
# 3 GiB guest: seastar --memory=1200M plus its OS reserve and the agent.
# boot-wait covers the hook's cold start (schema tables, ~10-20s) with margin.
forkd from-image scylla-service:local --tag scylla \
  --size-mib 8192 --mem-size-mib 3072 --boot-wait-secs 90

echo "=== 3. registered ==="
forkd images 2>&1 | grep -E '^ *(scylla|TAG) '

echo "=== 4. spawn + verify CQL is up on restore (no start step) ==="
SB=$(curl -s -X POST http://127.0.0.1:8889/v1/sandboxes -H 'Content-Type: application/json' \
  -d '{"snapshot_tag":"scylla","n":1,"per_child_netns":true}' \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d[0]['id'] if isinstance(d,list) and d else d.get('id',''))")
echo "sandbox: $SB"
[ -n "$SB" ] || { echo "SPAWN FAILED"; exit 1; }
cleanup() { curl -s -X DELETE "http://127.0.0.1:8889/v1/sandboxes/$SB" >/dev/null; }
trap cleanup EXIT

python3 - <<'PYEOF' > /tmp/scylla-verify.json
import json
print(json.dumps({"args": ["bash", "-c",
  "t0=$(date +%s%N); for i in $(seq 1 60); do "
  "out=$(cqlsh 127.0.0.1 -e 'SELECT release_version FROM system.local' 2>&1) && break; sleep 0.5; done; "
  "echo \"$out\" | grep -E '^ +[0-9]'; echo ready_ms=$(( ($(date +%s%N)-t0)/1000000 )); "
  "grep -c . /tmp/forkd-init-hooks.log; tail -3 /tmp/forkd-init-hooks.log"]}))
PYEOF
for i in $(seq 1 24); do
  curl -s -X POST "http://127.0.0.1:8889/v1/sandboxes/$SB/ping" -H 'Content-Type: application/json' -d '{}' | grep -q pong && break
  sleep 5
done
OUT=$(curl -s -X POST "http://127.0.0.1:8889/v1/sandboxes/$SB/exec" -H 'Content-Type: application/json' \
  -d @/tmp/scylla-verify.json | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('stdout','')); print(d.get('stderr',''))")
echo "$OUT"
echo "$OUT" | grep -q -E '^ +[0-9]+\.[0-9]' || { echo "CQL NOT UP ON RESTORE"; exit 1; }
echo "BAKE DONE"
