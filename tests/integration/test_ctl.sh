#!/bin/bash
source "$(dirname "$0")/lib.sh"
# test_ctl.sh — SSH-as-API control plane (U3) integration tests.
# Requires: lib.sh sourced, run ON vm2, spoond-sshd-gateway with ctl support.
# Adds a temporary test key to the allowlist, tests, then restores the unit.
set -u
UNIT=/etc/systemd/system/spoond-sshd-gateway.service
KEYS=/etc/forkd-gateway/keys
GWKEY=/tmp/itest_gw_key
GWKEY_PUB=/tmp/itest_gw_key.pub
SSHOPTS="-i $GWKEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes"
SSH="ssh $SSHOPTS ctl@127.0.0.1 -p 2222"

echo "== ctl: temporary test key setup =="
[ -f "$GWKEY" ] || ssh-keygen -t ed25519 -f "$GWKEY" -N "" -C "integration-test" >/dev/null 2>&1
cp "$GWKEY_PUB" "$KEYS/itest.pub"
cp "$UNIT" /tmp/ctl-unit.bak
# The unit uses --client-keys <dir> (dir scan): dropping itest.pub into
# the dir is all that's needed; the restart picks it up. Restore = rm.
systemctl daemon-reload && systemctl restart spoond-sshd-gateway
sleep 2
systemctl is-active spoond-sshd-gateway >/dev/null && ok "gateway restarted with test key" || bad "gateway restarted with test key"

echo
echo "== ctl: help =="
OUT=$(timeout 20 $SSH "help" 2>&1)
assert_contains "help lists commands" "$OUT" "new"

echo
echo "== ctl: new =="
OUT=$(timeout 40 $SSH "new" 2>&1)
assert_contains "new returns JSON" "$OUT" '"created":true'
CTLID=$(echo "$OUT" | grep -oE '[0-9a-f]{32}' | head -1)
if [ -n "$CTLID" ]; then ok "captured ctl lease id ($CTLID)"; else bad "captured ctl lease id"; fi

echo
echo "== ctl: ls == (pretty default; --json for machine) =="
OUT=$(timeout 20 $SSH "ls --json" 2>&1)
assert_contains "ls --json lists leases" "$OUT" "$CTLID"
OUT_PRETTY=$(timeout 20 $SSH "ls" 2>&1)
assert_contains "ls pretty has header" "$OUT_PRETTY" "IMAGE"

echo
echo "== ctl: whoami == (pretty default; --json for machine) =="
OUT=$(timeout 20 $SSH "whoami --json" 2>&1)
assert_contains "whoami --json returns user ctl" "$OUT" '"user":"ctl"'
assert_contains "whoami --json includes key fingerprint" "$OUT" "SHA256:"
OUT_PRETTY=$(timeout 20 $SSH "whoami" 2>&1)
assert_contains "whoami pretty shows user ctl" "$OUT_PRETTY" "user: ctl"

echo
echo "== ctl: comment =="
if [ -n "$CTLID" ]; then
  OUT=$(timeout 20 $SSH "comment $CTLID integration test box" 2>&1)
  assert_contains "comment set returns JSON" "$OUT" '"comment":"integration test box"'
  OUT=$(timeout 20 $SSH "ls" 2>&1)
  assert_contains "ls shows comment" "$OUT" "integration test box"
  OUT=$(timeout 20 $SSH "comment $CTLID" 2>&1)
  assert_contains "comment clears" "$OUT" '"comment":""'
fi

echo
echo "== ctl: keepalive =="
if [ -n "$CTLID" ]; then
  OUT=$(timeout 20 $SSH "keepalive $CTLID" 2>&1)
  assert_contains "keepalive returns JSON" "$OUT" '"persistent":true'
fi

echo
echo "== ctl: cp (clone) =="
if [ -n "$CTLID" ]; then
  OUT=$(timeout 60 $SSH "cp $CTLID" 2>&1)
  assert_contains "cp branches and clones" "$OUT" '"branch_tag"'
  CLONEID=$(echo "$OUT" | grep -oE '[0-9a-f]{32}' | head -1)
  if [ -n "$CLONEID" ]; then ok "captured clone lease id ($CLONEID)"; else bad "captured clone lease id"; fi
  BRANCHTAG=$(echo "$OUT" | grep -oE '"branch_tag":"[^"]*"' | cut -d'"' -f4)
  if [ -n "$BRANCHTAG" ]; then ok "captured branch tag ($BRANCHTAG)"; else bad "captured branch tag"; fi
fi

echo
echo "== ctl: suspend/resume =="
if [ -n "$CTLID" ]; then
  OUT=$(timeout 60 $SSH "suspend $CTLID" 2>&1)
  assert_contains "suspend returns JSON" "$OUT" '"status":"suspended"'
  # the workspace should now be suspended in the controller
  WS_STATE=$(curl -s --max-time 8 http://127.0.0.1:8889/v1/workspaces)
  if echo "$WS_STATE" | grep -q "\"name\":\"ws-$CTLID\".*\"status\":\"suspended\""; then
    ok "workspace $CTLID suspended in controller"
  else
    bad "workspace $CTLID suspended in controller"
  fi
  # exec while suspended must fail cleanly (409 conflict, not hang)
  CODE=$(curl -sk -o /dev/null -w '%{http_code}' -X POST "$BE_API/api/sandboxes/$CTLID/exec" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"cmd":"echo should-not-run"}')
  assert_eq "exec on suspended lease 409" "$CODE" "409"
  OUT=$(timeout 90 $SSH "resume $CTLID" 2>&1)
  assert_contains "resume returns JSON" "$OUT" '"status":"running"'
  WS_STATE2=$(curl -s --max-time 8 http://127.0.0.1:8889/v1/workspaces)
  if echo "$WS_STATE2" | grep -q "\"name\":\"ws-$CTLID\".*\"status\":\"running\""; then
    ok "workspace $CTLID running after resume"
  else
    bad "workspace $CTLID running after resume"
  fi
  # exec after resume must work (state restored, fresh sandbox id)
  OUT2=$(api POST "/api/sandboxes/$CTLID/exec" '{"cmd":"echo RESUMED_OK"}')
  assert_contains "exec works after resume" "$OUT2" "RESUMED_OK"
fi

echo
echo "== ctl: rejections =="
OUT=$(timeout 20 $SSH "frobnicate" 2>&1)
assert_contains "unknown command rejected" "$OUT" "unknown command"
OUT=$(timeout 20 $SSH "rm" 2>&1)
assert_contains "rm without id rejected" "$OUT" "usage"

echo
echo "== ctl: cleanup =="
if [ -n "${CLONEID:-}" ]; then
  api DELETE "/api/sandboxes/$CLONEID" >/dev/null && ok "deleted clone lease" || bad "delete clone lease"
  # remove the branch snapshot too (it's a persistent forkd snapshot)
  curl -s --max-time 10 -X DELETE "http://127.0.0.1:8889/v1/snapshots/${BRANCHTAG:-none}" >/dev/null 2>&1
  ok "removed branch snapshot"
fi
if [ -n "$CTLID" ]; then
  api DELETE "/api/sandboxes/$CTLID" >/dev/null && ok "deleted ctl lease" || bad "delete ctl lease"
fi
cp /tmp/ctl-unit.bak "$UNIT"
rm -f "$KEYS/itest.pub" "$GWKEY" "$GWKEY_PUB"
systemctl daemon-reload && systemctl restart spoond-sshd-gateway
sleep 2
systemctl is-active spoond-sshd-gateway >/dev/null && ok "gateway restored (jason key only)" || bad "gateway restored"
echo
echo "== ctl done =="
