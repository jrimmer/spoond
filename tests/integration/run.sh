#!/bin/bash
# run.sh — forkd integration test suite orchestrator.
#
# Runs the full integration suite against a live forkd stack (vm2).
# By default tests run locally against 127.0.0.1:8890; pass SSHHOST to
# stage and run on a remote host (e.g. SSHHOST=root@10.1.0.11).
#
# Usage:
#   tests/integration/run.sh              # run locally on vm2
#   SSHHOST=root@10.1.0.11 tests/integration/run.sh   # from Hermes host
set -u
cd "$(dirname "$0")"
DIR="$(pwd)"
SSHHOST="${SSHHOST:-}"
BE_API="${BE_API:-https://127.0.0.1:8890}"

# Build the wsclient test binary.
export PATH="$PATH:/usr/local/go/bin"
echo "== building wsclient =="
(cd "$DIR/wsclient" && go build -o "$DIR/wsclient/wsclient" .) \
  || { echo "wsclient build failed"; exit 1; }
echo "wsclient built"

# If SSHHOST is set, stage everything there and run remotely.
if [ -n "$SSHHOST" ]; then
  echo "== staging to $SSHHOST =="
  ssh -o BatchMode=yes -o ConnectTimeout=6 "$SSHHOST" "mkdir -p /tmp/forkd-itest"
  scp -q -r "$DIR/." "$SSHHOST:/tmp/forkd-itest/"
  # gateway test needs the backend token in its env file; run.sh picks it up from /etc
  # The cap runs REMOTELY (vm2 has GNU timeout; the local machine may
  # be macOS/zsh where `timeout` doesn't exist). If the remote run
  # hangs, timeout kills it and ssh returns.
  ssh -o BatchMode=yes -o ConnectTimeout=6 "$SSHHOST" \
    "cd /tmp/forkd-itest && timeout 600 env BE_API='$BE_API' bash run.sh"
  RC=$?
  exit $RC
fi

# Local run (on vm2 or wherever the backend is reachable).
source ./lib.sh
echo "== forkd integration suite =="
echo "backend: $BE_API  host: $(hostname)"
echo

# Truncate the shared results file so the summary reflects THIS run only.
: > "$RESULTS_FILE"

# Tolerate a cold pool: ensure at least one dev-base lease can be granted.
echo "== preflight: pool warm =="
L=$(new_lease dev-base 60 false)
if [ -n "$L" ]; then ok "pool grants dev-base"; del_lease "$L"; else bad "pool grants dev-base (cold pool?)"; fi
echo

run_file() { # run_file <name>
  echo "############################################################"
  echo "# $1"
  echo "############################################################"
  bash "$DIR/$1"
  echo
}

run_file test_lease_api.sh
run_file test_images.sh
run_file test_stream.sh
run_file test_gateway.sh
run_file test_ctl.sh
run_file test_ctl_new.sh
run_file test_netpolicy.sh
run_file test_proxy.sh

summary
