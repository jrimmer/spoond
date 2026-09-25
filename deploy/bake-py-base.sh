#!/bin/bash
# Bake the py-base snapshot: Python 3.12 slim + git (runner checkout needs
# git inside the sandbox) + curl. The runner's DEFAULT_IMAGE: every label
# without an explicit IMAGE_MAP mapping lands here (ubuntu-latest included).
#
# Runs ON THE FORKD HOST (vm2). Modeled on bake-go-base.sh; shares the
# 2026-09-25 lesson: from-image's conversion cache keys on the TAG and
# stores under a HASH-suffixed name — the cache-bust rm must glob it.
set -e -o pipefail
cd /var/cache/forkd
export FORKD_SCRIPTS_DIR=/usr/local/share/forkd-scripts

TAG=py-base
LOCAL_IMAGE=py-base-tools:local
SIZE_MIB=2048
MEM_MIB=2048

echo "=== 0. disk check ==="
df -h /var/cache/forkd | tail -1

echo "=== 1. build toolchain image (docker) ==="
DOCKER_BUILDKIT=1 docker build --network=host --security-opt seccomp=unconfined \
  -f py-base.dockerfile -t "$LOCAL_IMAGE" /var/cache/forkd

echo "=== 2. remove the existing snapshot BEFORE re-registering ==="
# Re-registering over a live tag keeps the daemon's open rootfs fd (old
# inode), so new bakes would silently serve stale content.
forkd rmi "$TAG" 2>&1 | tail -2 || true

echo "=== 3. from-image -> ext4 -> boot -> warm -> snapshot ==="
if [ "${KEEP_CACHE:-0}" != "1" ]; then
  rm -f /var/cache/forkd/py-base-tools-local*.ext4 \
        /var/cache/forkd/py-base-tools-local*.cache-meta.json
fi
forkd from-image "$LOCAL_IMAGE" --tag "$TAG" \
  --extra python3 --size-mib "$SIZE_MIB" --mem-size-mib "$MEM_MIB"

echo "=== 4. registered ==="
forkd images 2>&1 | grep -E "$TAG|TAG"

echo "=== 5. spawn + verify ==="
API=http://127.0.0.1:8889/v1
RESP=$(curl -s -X POST "$API/sandboxes" -H 'Content-Type: application/json' \
  -d "{\"snapshot_tag\":\"$TAG\",\"n\":1,\"per_child_netns\":true}")
SB=$(echo "$RESP" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d[0]['id'] if isinstance(d,list) and d else d.get('id',''))")
echo "sandbox: $SB"
if [ -z "$SB" ]; then echo "SPAWN FAILED: $RESP"; exit 1; fi

ok=0
for i in $(seq 1 12); do
  sleep 5
  if curl -s -X POST "$API/sandboxes/$SB/ping" -H 'Content-Type: application/json' -d '{}' | grep -q pong; then
    echo "agent up after $((i*5))s"; ok=1; break
  fi
done
[ "$ok" = "1" ] || { echo "AGENT NEVER CAME UP"; curl -s -X DELETE "$API/sandboxes/$SB" >/dev/null; exit 1; }

cat > /tmp/py-base-verify.sh <<'VERIFY'
set -e
echo "--- toolchain ---"
python3 --version
pip3 --version
git --version
echo "--- the real gate: pip install from the network + git clone ---"
pip3 install --quiet --no-cache-dir packaging && python3 -c "import packaging; print('PIP_GATE_OK')"
cd /tmp && rm -rf gate && git clone --depth 1 https://github.com/octocat/Hello-World.git gate 2>&1 | tail -1
ls gate/README >/dev/null && echo "GIT_GATE_OK"
echo PY_BASE_VERIFY_OK
VERIFY

python3 - <<'PYEOF' > /tmp/py-base-payload.json
import json
print(json.dumps({"args": ["bash","-c", open("/tmp/py-base-verify.sh").read()]}))
PYEOF

curl -s -X POST "$API/sandboxes/$SB/exec" -H 'Content-Type: application/json' \
  -d @/tmp/py-base-payload.json \
  | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('stdout',''));print(d.get('stderr',''),file=sys.stderr)"

curl -s -X DELETE "$API/sandboxes/$SB" >/dev/null
echo "=== BAKE DONE ==="
