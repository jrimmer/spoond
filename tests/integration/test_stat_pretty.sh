#!/bin/bash
# test_stat_pretty.sh — tickets #25 + #27: ctl stat verb returns shaped
# metrics; ctl ls defaults to a pretty table; ls --json stays raw JSON.
#
# Uses the same temporary-key harness as test_ctl.sh: generates an
# ed25519 key, registers itepub in the gateway allowlist, runs the
# assertions, then restores the unit + removes the key (a dangling
# key reference crash-loops the gateway — restore ALWAYS runs).
set -u
# shellcheck source=/dev/null
. "$(dirname "$0")/lib.sh"

UNIT=/etc/systemd/system/spoond-sshd-gateway.service
KEYS=/etc/forkd-gateway/keys
GWKEY=/tmp/itest_stat_key
GWKEY_PUB=/tmp/itest_stat_key.pub
SSHOPTS="-i $GWKEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes"
SSH="ssh $SSHOPTS ctl@127.0.0.1 -p 2222"

restore_gw() { # restore unit + key; must always run
  cp -f /tmp/stat-unit.bak "$UNIT" 2>/dev/null || true
  rm -f "$KEYS/itest_stat.pub"
  systemctl daemon-reload && systemctl restart spoond-sshd-gateway
  sleep 2
}
trap restore_gw EXIT

echo "== stat/pretty: temporary test key setup =="
[ -f "$GWKEY" ] || ssh-keygen -t ed25519 -f "$GWKEY" -N "" -C "integration-test" >/dev/null 2>&1
cp "$GWKEY_PUB" "$KEYS/itest_stat.pub"
cp "$UNIT" /tmp/stat-unit.bak
sed -i 's|,/etc/forkd-gateway/keys/itest_stat.pub||; s|--client-keys \([^ ]*\)|--client-keys \1,/etc/forkd-gateway/keys/itest_stat.pub|' "$UNIT"
systemctl daemon-reload && systemctl restart spoond-sshd-gateway
sleep 2
systemctl is-active spoond-sshd-gateway >/dev/null && ok "gateway restarted with test key" || bad "gateway restarted with test key"

echo
echo "== stat/pretty: lease for testing =="
L=$(new_lease dev-base 300 false)
if [ -z "$L" ]; then
  bad "stat/pretty: lease for testing"
  exit 0
fi
ok "lease $L"

echo
echo "== stat/pretty: backend API path =="
STAT=$(api GET "/api/sandboxes/$L/stat")
assert_contains "stat returns cpu" "$STAT" '"cpu"'
assert_contains "stat returns mem" "$STAT" '"used_mib"'
assert_contains "stat returns net" "$STAT" '"rx_bytes"'

echo
echo "== stat/pretty: ctl stat (pretty default) =="
CTL_STAT=$(timeout 20 $SSH "stat $L" 2>&1)
assert_contains "ctl stat pretty shows cpu line" "$CTL_STAT" "cpu : load1"
assert_contains "ctl stat pretty shows mem line" "$CTL_STAT" "MiB used"
assert_contains "ctl stat pretty shows net line" "$CTL_STAT" "net : rx"

echo
echo "== stat/pretty: ctl stat --json (raw) =="
CTL_STAT_JSON=$(timeout 20 $SSH "stat $L --json" 2>&1)
assert_contains "ctl stat --json raw" "$CTL_STAT_JSON" '"load1"'

echo
echo "== stat/pretty: ctl ls (pretty default) =="
LS=$(timeout 20 $SSH "ls" 2>&1)
assert_contains "ctl ls pretty has header" "$LS" "IMAGE"
assert_contains "ctl ls pretty has truncated id" "$LS" "…"

echo
echo "== stat/pretty: ctl ls --json (raw) =="
LS_JSON=$(timeout 20 $SSH "ls --json" 2>&1)
assert_contains "ctl ls --json is sandboxes array" "$LS_JSON" '"sandboxes"'

echo
echo "== stat/pretty: ctl whoami (pretty default) =="
WHOAMI=$(timeout 20 $SSH "whoami" 2>&1)
assert_contains "ctl whoami pretty" "$WHOAMI" "user: ctl"

echo
echo "== stat/pretty: ctl whoami --json (raw) =="
WHOAMI_JSON=$(timeout 20 $SSH "whoami --json" 2>&1)
assert_contains "ctl whoami --json raw" "$WHOAMI_JSON" '"user":"ctl"'
