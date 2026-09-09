#!/bin/bash
# Bake the elixir-release snapshot: Elixir 1.18/OTP 27 + Rust stable +
# Node 22/pnpm + kaniko. Runs ON THE FORKD HOST (vm2); the dockerfile is
# images/elixir-release.dockerfile from the spoond repo.
#
# Retry with fixed args, js-base style: from-image registers the tag and
# a re-run overwrites it.
set -e -o pipefail
cd /var/cache/forkd
export FORKD_SCRIPTS_DIR=/usr/local/share/forkd-scripts

echo "=== 1. build toolchain image (docker) ==="
# --security-opt seccomp=unconfined: the host's docker default seccomp
# profile denies thread creation (getaddrinfo) and AF_UNIX in build
# containers. --network=host: build-sandbox DNS is flaky otherwise.
DOCKER_BUILDKIT=1 docker build --network=host --security-opt seccomp=unconfined \
  -f elixir-release.dockerfile -t elixir-release-tools:local /var/cache/forkd

echo "=== 2. from-image -> ext4 -> boot -> warm -> snapshot ==="
# Cache-bust: from-image keys its ext4 artifact cache on the image TAG, so
# a rebuilt image under the same tag silently reuses a STALE conversion
# (this cost us a whole round of "where did rust go"). rm the artifact
# unless KEEP_CACHE=1.
if [ "${KEEP_CACHE:-0}" != "1" ]; then rm -f /var/cache/forkd/elixir-release-tools-local.ext4; fi
# Remove the existing snapshot FIRST: re-registering over a live tag keeps
# the daemon's open rootfs fd (old inode — rm+recreate of the ext4 at the
# same path is invisible to already-registered snapshots), so new bakes
# would silently serve stale content.
forkd rmi elixir-release 2>&1 | tail -2 || true
forkd from-image elixir-release-tools:local --tag elixir-release \
  --extra python3 --size-mib 12288 --mem-size-mib 4096

echo "=== 3. registered ==="
forkd images 2>&1 | grep -E 'elixir-release|TAG'

echo "=== 4. spawn + verify tools ==="
SB=$(curl -s -X POST http://127.0.0.1:8889/v1/sandboxes -H 'Content-Type: application/json' \
  -d '{"snapshot_tag":"elixir-release","n":1,"per_child_netns":true}' \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d[0]['id'] if isinstance(d,list) and d else d.get('id',''))")
echo "sandbox: $SB"
if [ -z "$SB" ]; then echo "SPAWN FAILED"; exit 1; fi

ok=0
for i in $(seq 1 12); do
  sleep 5
  R=$(curl -s -X POST "http://127.0.0.1:8889/v1/sandboxes/$SB/ping" -H 'Content-Type: application/json' -d '{}')
  if echo "$R" | grep -q 'pong'; then echo "agent up after $((i*5))s"; ok=1; break; fi
done
[ "$ok" = "1" ] || { echo "AGENT NEVER CAME UP"; curl -s -X DELETE "http://127.0.0.1:8889/v1/sandboxes/$SB" >/dev/null; exit 1; }

cat > /tmp/elixir-release-verify.sh <<'VERIFY'
set -e
echo "--- toolchains ---"
elixir --version | head -2
rustc --version && cargo --version
node --version && git --version && python3 --version
echo "--- symlink + copy exec (ext4-conversion corruption sentinels) ---"
ln -sf /usr/local/bin/node /tmp/node-link && /tmp/node-link --version && echo SYMLINK_EXEC_OK
cp /usr/local/bin/node /tmp/node-copy && /tmp/node-copy --version && echo COPY_EXEC_OK
corepack --version && echo COREPACK_SHIM_OK
which pkg-config && echo WHICH_OK
id && getent passwd root >/dev/null && echo PASSWD_OK
pkg-config --libs openssl && echo PKGCONFIG_OPENSSL_OK
pkg-config --version && PKG_CONFIG_PATH=/usr/lib/x86_64-linux-gnu/pkgconfig pkg-config --exists openssl && echo PKGCONFIG_OK
echo "--- space ---"
df -h / | tail -1
echo ELIXIR_RELEASE_OK
VERIFY

python3 - <<'PYEOF' > /tmp/elixir-release-payload.json
import json
print(json.dumps({"args": ["bash", "-c", open("/tmp/elixir-release-verify.sh").read()]}))
PYEOF

curl -s -X POST "http://127.0.0.1:8889/v1/sandboxes/$SB/exec" -H 'Content-Type: application/json' \
  -d @/tmp/elixir-release-payload.json \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('stdout','')); print(d.get('stderr',''), file=sys.stderr)"

curl -s -X DELETE "http://127.0.0.1:8889/v1/sandboxes/$SB" >/dev/null
echo "BAKE DONE"
