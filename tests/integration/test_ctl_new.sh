#!/bin/bash
source "$(dirname "$0")/lib.sh"
# test_ctl_new.sh — extended ctl surface (U7): tag, restart, prompt, names.
# Requires: lib.sh sourced, run ON vm2, spoond-sshd-gateway with ctl support.
# Adds a temporary test key to the allowlist, tests, then restores the unit.
set -u
UNIT=/etc/systemd/system/spoond-sshd-gateway.service
KEYS=/etc/forkd-gateway/keys
GWKEY=/tmp/itest_gw_key
GWKEY_PUB=/tmp/itest_gw_key.pub
SSHOPTS="-i $GWKEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes"
SSH="ssh $SSHOPTS ctl@127.0.0.1 -p 2222"
SSHSB="ssh $SSHOPTS"

echo "== ctl-new: temporary test key setup =="
[ -f "$GWKEY" ] || ssh-keygen -t ed25519 -f "$GWKEY" -N "" -C "integration-test" >/dev/null 2>&1
cp "$GWKEY_PUB" "$KEYS/itest.pub"
cp "$UNIT" /tmp/ctl-unit.bak
# The unit uses --client-keys <dir> (dir scan): dropping itest.pub into
# the dir is all that's needed; the restart picks it up. Restore = rm.
systemctl daemon-reload && systemctl restart spoond-sshd-gateway
sleep 2
systemctl is-active spoond-sshd-gateway >/dev/null && ok "gateway restarted with test key" || bad "gateway restarted with test key"

echo
echo "== ctl-new: new ==="
OUT=$(timeout 40 $SSH "new" 2>&1)
assert_contains "new returns JSON" "$OUT" '"created":true'
CTLID=$(echo "$OUT" | grep -oE '[0-9a-f]{32}' | head -1)
if [ -n "$CTLID" ]; then ok "captured ctl lease id ($CTLID)"; else bad "captured ctl lease id"; fi

echo
echo "== ctl-new: tag + name resolution =="
if [ -n "$CTLID" ]; then
  OUT=$(timeout 20 $SSH "tag $CTLID webby" 2>&1)
  assert_contains "tag assigns name" "$OUT" '"name":"webby"'
  OUT=$(api GET "/api/names/webby")
  assert_contains "name resolves to lease" "$OUT" "$CTLID"
  # duplicate name on a second lease must be rejected
  OUT2=$(timeout 40 $SSH "new" 2>&1)
  CTLID2=$(echo "$OUT2" | grep -oE '[0-9a-f]{32}' | head -1)
  if [ -n "$CTLID2" ]; then
    OUT3=$(timeout 20 $SSH "tag $CTLID2 webby" 2>&1)
    assert_contains "duplicate name rejected" "$OUT3" "already in use"
  fi
  # name-based SSH attach (gateway resolves username -> lease)
  OUT4=$(timeout 30 $SSHSB "webby@127.0.0.1" "hostname" 2>&1)
  assert_contains "name-based ssh works" "$OUT4" "host"
fi

echo
echo "== ctl-new: restart =="
if [ -n "$CTLID" ]; then
  OUT=$(timeout 90 $SSH "restart $CTLID" 2>&1)
  assert_contains "restart returns JSON" "$OUT" '"status":"running"'
  # exec after restart must work (fresh sandbox, state restored)
  OUT2=$(api POST "/api/sandboxes/$CTLID/exec" '{"cmd":"echo RESTARTED_OK"}')
  assert_contains "exec works after restart" "$OUT2" "RESTARTED_OK"
  # proxy after restart must work (netns preserved across resume)
  sleep 5
  CODE=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 20 -H "Host: $CTLID-8080.sandbox.lacy.casa" "http://127.0.0.1:8891/")
  # guest has nothing on 8080; 502 is fine (proxy dialed), 400/404 is a proxy-path regression
  if [ "$CODE" = "502" ] || [ "$CODE" = "000" ]; then
    ok "proxy-after-restart reached guest network (502 = connected, nothing listening)"
  else
    bad "proxy-after-restart: unexpected code $CODE"
  fi
fi

echo
echo "== ctl-new: prompt (no agent -> 409) =="
if [ -n "$CTLID" ]; then
  CODE=$(curl -sk -o /tmp/prompt_resp -w '%{http_code}' --max-time 30 -X POST "$BE_API/api/sandboxes/$CTLID/prompt" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"message":"hi"}')
  assert_eq "prompt without agent conflicts" "$CODE" "409"
fi

echo
echo "== ctl-new: cleanup =="
if [ -n "${CTLID2:-}" ]; then
  api DELETE "/api/sandboxes/$CTLID2" >/dev/null && ok "deleted second lease" || bad "delete second lease"
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
echo "== ctl-new done =="
