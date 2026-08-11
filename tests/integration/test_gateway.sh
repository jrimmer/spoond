#!/bin/bash
source "$(dirname "$0")/lib.sh"
# test_gateway.sh — SSH gateway integration tests.
# Requires: lib.sh sourced, run ON vm2 (needs local systemd + gateway keys dir).
# Adds a temporary test key to the allowlist, tests, then restores the unit.
set -u
UNIT=/etc/systemd/system/spoond-sshd-gateway.service
KEYS=/etc/forkd-gateway/keys
GWKEY=/tmp/itest_gw_key
GWKEY_PUB=/tmp/itest_gw_key.pub
SSHOPTS="-i $GWKEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes"

echo "== gateway: temporary test key setup =="
if [ ! -f /etc/forkd-gateway/keys ]; then
  # sanity: keys dir exists
  ls -d "$KEYS" >/dev/null 2>&1 || { echo "  ❌ gateway keys dir missing"; exit 1; }
fi
[ -f "$GWKEY" ] || ssh-keygen -t ed25519 -f "$GWKEY" -N "" -C "integration-test" >/dev/null 2>&1
cp "$GWKEY_PUB" "$KEYS/itest.pub"
cp "$UNIT" /tmp/gateway-unit.bak
# The unit uses --client-keys <dir> (dir scan): dropping itest.pub into
# the dir is all that's needed; the restart picks it up. Restore = rm.
systemctl daemon-reload && systemctl restart spoond-sshd-gateway
sleep 2
systemctl is-active spoond-sshd-gateway >/dev/null && ok "gateway restarted with test key" || bad "gateway restarted with test key"

echo
echo "== gateway: auto-create (ssh new@) =="
OUT=$(timeout 40 ssh $SSHOPTS "new@127.0.0.1" -p 2222 "echo GW_CREATE_OK; hostname; exit" 2>&1)
assert_contains "new@ creates sandbox (MOTD)" "$OUT" "spoond: created sandbox"
assert_contains "new@ drops into guest" "$OUT" "GW_CREATE_OK"
assert_contains "new@ guest hostname is 10.42" "$OUT" "10.42"
NEWID=$(echo "$OUT" | grep -oP '(?<=sandbox )[a-f0-9]{32}' | head -1)
if [ -n "$NEWID" ]; then ok "captured new lease id ($NEWID)"; else bad "captured new lease id"; fi

echo
echo "== gateway: attach existing (ssh <id>@) =="
if [ -n "$NEWID" ]; then
  OUT2=$(timeout 40 ssh $SSHOPTS "$NEWID@127.0.0.1" -p 2222 "echo GW_REATTACH_OK; hostname; exit" 2>&1)
  assert_contains "reattach reaches same sandbox" "$OUT2" "GW_REATTACH_OK"
  assert_contains "reattach same hostname" "$OUT2" "10.42"
fi

echo
echo "== gateway: interactive pty (exec path with -tt) =="
OUT3=$(timeout 40 ssh -tt $SSHOPTS "new@127.0.0.1" -p 2222 "echo GW_PTY_OK; exit" 2>&1)
assert_contains "pty exec works" "$OUT3" "GW_PTY_OK"
# the pty run auto-created a SECOND persistent lease (workspace-backed);
# capture it so cleanup can dispose it too.
PTYID=$(echo "$OUT3" | grep -oP '(?<=sandbox )[a-f0-9]{32}' | head -1)
if [ -n "$PTYID" ]; then ok "captured pty lease id ($PTYID)"; else bad "captured pty lease id"; fi

echo
echo "== gateway: rejections =="
OUT4=$(timeout 20 ssh $SSHOPTS "new-go@127.0.0.1" -p 2222 "echo never" 2>&1)
assert_contains "CI image rejected" "$OUT4" "CI image with no sshd"
OUT5=$(timeout 20 ssh $SSHOPTS "new-bogus@127.0.0.1" -p 2222 "echo never" 2>&1)
assert_contains "unknown image rejected" "$OUT5" "unknown image"
OUT6=$(timeout 20 ssh $SSHOPTS "deadbeefdeadbeefdeadbeefdeadbeef@127.0.0.1" -p 2222 "echo never" 2>&1)
assert_contains "unknown lease rejected" "$OUT6" "cannot reach"

echo
echo "== gateway: cleanup =="
# destroy the auto-created sandboxes (new@ + pty new@)
if [ -n "$NEWID" ]; then
  api DELETE "/api/sandboxes/$NEWID" >/dev/null && ok "deleted auto-created sandbox $NEWID" || bad "delete auto-created sandbox"
fi
if [ -n "${PTYID:-}" ]; then
  api DELETE "/api/sandboxes/$PTYID" >/dev/null && ok "deleted pty sandbox $PTYID" || bad "delete pty sandbox"
fi
# restore unit without test key
cp /tmp/gateway-unit.bak "$UNIT"
rm -f "$KEYS/itest.pub" "$GWKEY" "$GWKEY_PUB"
systemctl daemon-reload && systemctl restart spoond-sshd-gateway
sleep 2
systemctl is-active spoond-sshd-gateway >/dev/null && ok "gateway restored (jason key only)" || bad "gateway restored"
echo
echo "== gateway done =="
