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
# --security-opt seccomp=unconfined: vm2 is a Proxmox LXC; the default build
# sandbox denies thread creation (getaddrinfo) and AF_UNIX (Erlang spawn_init).
# --network=host: DNS resolution is flaky in the nested build sandbox.
DOCKER_BUILDKIT=1 docker build --network=host --security-opt seccomp=unconfined \
  -f elixir-release.dockerfile -t elixir-release-tools:local /var/cache/forkd

echo "=== 2. from-image -> ext4 -> boot -> warm -> snapshot ==="
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
elixir --version | head -1
erl -noshell -eval 'io:format("otp: ~s~n", [erlang:system_info(otp_release)]), halt().'
rustc --version && cargo --version
node --version && corepack pnpm --version
executor --help 2>&1 | head -1
git --version && python3 --version
echo ELIXIR_RELEASE_OK
VERIFY

python3 - <<'PYEOF' > /tmp/elixir-release-payload.json
import json
print(json.dumps({"args": ["bash", "-lc", open("/tmp/elixir-release-verify.sh").read()]}))
PYEOF

curl -s -X POST "http://127.0.0.1:8889/v1/sandboxes/$SB/exec" -H 'Content-Type: application/json' \
  -d @/tmp/elixir-release-payload.json \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('stdout','')); print(d.get('stderr',''), file=sys.stderr)"

curl -s -X DELETE "http://127.0.0.1:8889/v1/sandboxes/$SB" >/dev/null
echo "BAKE DONE"
