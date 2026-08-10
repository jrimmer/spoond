#!/bin/bash
source "$(dirname "$0")/lib.sh"
# test_netpolicy.sh — per-session networking policy (ticket #13).
# Requires: lib.sh sourced, run ON vm2, backend with NETPOL_DNS set.
# Creates leases with each policy and probes egress from inside the guest.
set -u

echo "== netpolicy: create with none (no egress) =="
LID=$(api POST "/api/sandboxes" '{"image":"dev-base","persistent":true,"network_policy":"none"}' | grep -oE '"id":"[0-9a-f]{32}"' | cut -d'"' -f4)
if [ -n "$LID" ]; then ok "none lease created ($LID)"; else bad "none lease created"; fi
sleep 5
OUT=$(api POST "/api/sandboxes/$LID/exec" '{"cmd":"curl -s -o /dev/null -w %{http_code} --max-time 5 http://10.1.0.47:3000/ 2>&1 || echo FAIL","timeout":20}')
if echo "$OUT" | grep -qE 'FAIL|000'; then ok "none blocks LAN egress"; else bad "none blocks LAN egress (got $OUT)"; fi

echo
echo "== netpolicy: lan (default, RFC1918 allowed) =="
LID2=$(api POST "/api/sandboxes" '{"image":"dev-base","persistent":true}' | grep -oE '"id":"[0-9a-f]{32}"' | cut -d'"' -f4)
[ -n "$LID2" ] && ok "lan lease created ($LID2)" || bad "lan lease created"
sleep 5
OUT=$(api POST "/api/sandboxes/$LID2/exec" '{"cmd":"curl -s -o /dev/null -w %{http_code} --max-time 5 http://10.1.0.47:3000/ 2>&1 || echo FAIL","timeout":20}')
if echo "$OUT" | grep -qE '"stdout":"200"'; then ok "lan allows LAN egress"; else bad "lan allows LAN egress (got $OUT)"; fi

echo
echo "== netpolicy: internet (permissive) =="
LID3=$(api POST "/api/sandboxes" '{"image":"dev-base","persistent":true,"network_policy":"internet"}' | grep -oE '"id":"[0-9a-f]{32}"' | cut -d'"' -f4)
[ -n "$LID3" ] && ok "internet lease created ($LID3)" || bad "internet lease created"
sleep 5
OUT=$(api POST "/api/sandboxes/$LID3/exec" '{"cmd":"curl -s -o /dev/null -w %{http_code} --max-time 6 http://1.1.1.1/ 2>&1 || echo FAIL","timeout":20}')
if echo "$OUT" | grep -qE '"stdout":"(301|200|302|403|404)"'; then ok "internet allows egress"; else bad "internet allows egress (got $OUT)"; fi

echo
echo "== netpolicy: restricted requires allowlist =="
CODE=$(curl -sk -o /dev/null -w '%{http_code}' -X POST "$BE_API/api/sandboxes" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"image":"dev-base","persistent":true,"network_policy":"restricted"}')
assert_eq "restricted without allowlist rejected" "$CODE" "400"
CODE=$(curl -sk -o /dev/null -w '%{http_code}' -X POST "$BE_API/api/sandboxes" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"image":"dev-base","persistent":true,"network_policy":"bogus"}')
assert_eq "unknown policy rejected" "$CODE" "400"

echo
echo "== netpolicy: restricted with allowlist =="
LID4=$(api POST "/api/sandboxes" '{"image":"dev-base","persistent":true,"network_policy":"restricted","egress_allowlist":["10.1.0.47"]}' | grep -oE '"id":"[0-9a-f]{32}"' | cut -d'"' -f4)
[ -n "$LID4" ] && ok "restricted lease created ($LID4)" || bad "restricted lease created"
sleep 5
# allowed IP should work
OUT=$(api POST "/api/sandboxes/$LID4/exec" '{"cmd":"curl -s -o /dev/null -w %{http_code} --max-time 5 http://10.1.0.47:3000/ 2>&1 || echo FAIL","timeout":20}')
if echo "$OUT" | grep -qE '"stdout":"200"'; then ok "restricted allows allowlisted IP"; else bad "restricted allows allowlisted IP (got $OUT)"; fi
# non-allowlisted LAN IP should be blocked
OUT=$(api POST "/api/sandboxes/$LID4/exec" '{"cmd":"curl -s -o /dev/null -w %{http_code} --max-time 5 http://10.1.0.203:80/ 2>&1 || echo FAIL","timeout":20}')
if echo "$OUT" | grep -qE 'FAIL|000'; then ok "restricted blocks non-allowlisted IP"; else bad "restricted blocks non-allowlisted IP (got $OUT)"; fi

echo
echo "== netpolicy: policy survives restart (re-applied on resume) =="
OUT=$(api POST "/api/sandboxes/$LID/restart" '{}' 2>/dev/null || api POST "/api/sandboxes/$LID/restart" '{"dummy":1}')
sleep 8
OUT=$(api POST "/api/sandboxes/$LID/exec" '{"cmd":"curl -s -o /dev/null -w %{http_code} --max-time 5 http://10.1.0.47:3000/ 2>&1 || echo FAIL","timeout":20}')
if echo "$OUT" | grep -qE 'FAIL|000'; then ok "none policy re-applied after restart"; else bad "none policy re-applied after restart (got $OUT)"; fi

echo
echo "== netpolicy: cleanup =="
for L in "$LID" "$LID2" "$LID3" "$LID4"; do
  [ -n "$L" ] && api DELETE "/api/sandboxes/$L" >/dev/null && ok "deleted lease ${L:0:8}" || bad "delete lease ${L:0:8}"
done
echo
echo "== netpolicy done =="
