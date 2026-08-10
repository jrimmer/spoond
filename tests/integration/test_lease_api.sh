#!/bin/bash
source "$(dirname "$0")/lib.sh"
# test_lease_api.sh — lease API lifecycle integration tests.
# Requires: lib.sh sourced, live backend on BE_API.
set -u

echo "== lease API: health + auth =="
assert_status "healthz unauthenticated 200" 200 "$BE_API/healthz"
assert_status "create without token 401" 401 -X POST "$BE_API/api/sandboxes" -H "Content-Type: application/json" -d '{"image":"dev-base"}'
assert_status "list without token 401" 401 "$BE_API/api/sandboxes"
assert_status "bad token 401" 401 "$BE_API/api/sandboxes" -H "Authorization: Bearer deadbeef"
assert_status "images requires auth 401" 401 "$BE_API/api/images"

echo
echo "== lease API: images =="
IMGS=$(api GET /api/images)
assert_contains "images lists dev-base" "$IMGS" "dev-base"
assert_contains "images lists go-base" "$IMGS" "go-base"
assert_contains "images lists py-base" "$IMGS" "py-base"
assert_contains "images lists elixir-base" "$IMGS" "elixir-base"
assert_contains "images lists llm-review" "$IMGS" "llm-review"

echo
echo "== lease API: create + list =="
L1=$(new_lease dev-base 120 false)
[ -n "$L1" ] && ok "create dev-base lease ($L1)" || bad "create dev-base lease"
LIST=$(api GET /api/sandboxes)
assert_contains "list shows lease" "$LIST" "$L1"
assert_eq "lease image is dev-base" "$(lease_image "$L1")" "dev-base"
assert_contains "lease is non-persistent" "$LIST" '"persistent":false'

echo
echo "== lease API: exec =="
# Agent may need a moment; poll for readiness, then exec.
READY=$(wait_agent "$L1" "ready")
if [ "$?" -eq 0 ]; then ok "agent reachable"; else bad "agent reachable"; fi
OUT=$(api POST "/api/sandboxes/$L1/exec" '{"cmd":"echo EXEC_OK; hostname"}')
assert_contains "exec returns command output" "$OUT" "EXEC_OK"
assert_contains "exec returns guest hostname (10.42.x)" "$OUT" "10.42"

echo
echo "== lease API: endpoint =="
EP=$(api GET "/api/sandboxes/$L1/endpoint")
assert_contains "endpoint has netns" "$EP" '"netns"'
assert_contains "endpoint has guest_addr" "$EP" '"guest_addr"'
assert_contains "endpoint has image" "$EP" '"dev-base"'

echo
echo "== lease API: cross-consumer denial =="
OTHER="deadbeefdeadbeefdeadbeefdeadbeef"
# A second token isn't available; simulate with an unguessable id owned by nobody.
assert_status "exec on unknown lease 404" 404 -X POST "$BE_API/api/sandboxes/$OTHER/exec" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"cmd":"true"}'

echo
echo "== lease API: unknown image =="
R=$(api POST /api/sandboxes '{"image":"does-not-exist","ttl":60}')
assert_contains "unknown image rejected" "$R" "unknown image"

echo
echo "== lease API: keepalive (persistent) =="
LP=$(new_lease dev-base 60 true)
[ -n "$LP" ] && ok "create persistent lease ($LP)" || bad "create persistent lease"
KA=$(api POST "/api/sandboxes/$LP/keepalive" '{"ttl":3600}')
assert_contains "keepalive extends persistent" "$KA" "expires"
# verify it survived a TTL-cycle wait (30s < 60s TTL, but sweep runs; persistent must persist)
sleep 5
LIST2=$(api GET /api/sandboxes)
assert_contains "persistent lease survives sweep window" "$LIST2" "$LP"
del_lease "$LP"

echo
echo "== lease API: delete =="
del_lease "$L1"
LIST3=$(api GET /api/sandboxes)
if echo "$LIST3" | grep -q "$L1"; then bad "deleted lease gone from list"; else ok "deleted lease gone from list"; fi
assert_status "delete again 404" 404 -X DELETE "$BE_API/api/sandboxes/$L1" -H "Authorization: Bearer $TOKEN"

echo
echo "== lease API: TTL expiry =="
L2=$(new_lease dev-base 2 false)
[ -n "$L2" ] && ok "create short-TTL lease ($L2)" || bad "create short-TTL lease"
sleep 8
LIST4=$(api GET /api/sandboxes)
if echo "$LIST4" | grep -q "$L2"; then bad "short-TTL lease swept"; else ok "short-TTL lease swept"; fi

echo
echo "== lease API done =="
