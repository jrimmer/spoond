#!/bin/bash
# test_identity.sh — identity + ownership + quota integration tests
# (epic #26: T1 identity, T2 ownership, T4 quotas, T5 ctl scoping).
# Requires: BE_API, TOKEN (admin consumer token), live backend with
# USERS_FILE enabled.
set -u
. "$(dirname "$0")/lib.sh"

ADMIN_TOKEN="$TOKEN"
# Unique names to survive repeated runs.
U1="itest-owner-$RANDOM"
U2="itest-other-$RANDOM"
T1="itest-tok-$RANDOM"
T2="itest-tok2-$RANDOM"

# --- bootstrap admin -----------------------------------------------------
# First created user becomes admin (KTD-2). Use a fresh unique name; the
# store may already have users from previous runs, so create via admin
# token instead of relying on bootstrap being open.
ADMIN_NAME="itest-admin-$RANDOM"
ADMIN_TOK="itest-adm-tok-$RANDOM"
resp=$(api POST /api/users "{\"name\":\"$ADMIN_NAME\",\"kind\":\"person\",\"fingerprints\":[],\"token\":\"$ADMIN_TOK\"}")
assert_contains "create user (admin token)" "$resp" '"admin":true'

# --- user CRUD -----------------------------------------------------------
resp=$(curl -sk -X POST "$BE_API/api/users" -H "Authorization: Bearer $ADMIN_TOK" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"$U1\",\"kind\":\"person\",\"fingerprints\":[],\"token\":\"$T1\"}")
assert_contains "create owner user" "$resp" '"kind":"person"'
U1_ID=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['user']['id'])" 2>/dev/null)

resp=$(curl -sk -X POST "$BE_API/api/users" -H "Authorization: Bearer $ADMIN_TOK" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"$U2\",\"kind\":\"agent\",\"fingerprints\":[],\"token\":\"$T2\"}")
assert_contains "create other user (agent)" "$resp" '"kind":"agent"'
U2_ID=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['user']['id'])" 2>/dev/null)

# duplicate name rejected
dup=$(curl -sk -X POST "$BE_API/api/users" -H "Authorization: Bearer $ADMIN_TOK" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"$U1\",\"kind\":\"person\",\"fingerprints\":[]}")
assert_contains "duplicate user rejected" "$dup" 'already exists'

# non-admin cannot create users
nonadmin=$(curl -sk -X POST "$BE_API/api/users" -H "Authorization: Bearer $T1" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"itest-x-$RANDOM\",\"kind\":\"person\",\"fingerprints\":[]}")
assert_contains "non-admin create rejected" "$nonadmin" 'admin required'

# --- owner serialization (U2) -------------------------------------------
lid=$(new_lease dev-base 120 false)   # created as admin token
[ -n "$lid" ] || bad "identity: create lease"
out=$(api GET /api/sandboxes)
assert_contains "list carries owner" "$out" '"owner"'

# --- owner scoping: other user cannot see/rm the lease ------------------
other_list=$(curl -sk "$BE_API/api/sandboxes" -H "Authorization: Bearer $T2")
if echo "$other_list" | grep -q "$lid"; then
  bad "identity: cross-owner list leak"
else
  ok "identity: cross-owner list isolated"
fi

other_rm=$(curl -sk -o /dev/null -w '%{http_code}' -X DELETE "$BE_API/api/sandboxes/$lid" \
  -H "Authorization: Bearer $T2")
assert_eq "identity: cross-owner rm denied" "$other_rm" "404"

# --- quota (U5) ----------------------------------------------------------
curl -sk -X POST "$BE_API/api/users/$U1_ID/quota" -H "Authorization: Bearer $ADMIN_TOK" \
  -H "Content-Type: application/json" -d '{"max_leases":1,"max_ttl":10}' >/dev/null
# first lease as U1 (TTL clamps 3600 -> 10)
r1=$(curl -sk -w '\n%{http_code}' -X POST "$BE_API/api/sandboxes" -H "Authorization: Bearer $T1" \
  -H "Content-Type: application/json" -d '{"image":"dev-base","ttl":3600}')
assert_contains "quota: first lease ok" "$r1" "201"
assert_contains "quota: ttl clamped" "$r1" '"ttl":10'
# second lease as U1 -> 429
r2=$(curl -sk -o /dev/null -w '%{http_code}' -X POST "$BE_API/api/sandboxes" -H "Authorization: Bearer $T1" \
  -H "Content-Type: application/json" -d '{"image":"dev-base","ttl":60}')
assert_eq "quota: cap hit -> 429" "$r2" "429"

# --- trusted gateway impersonation (U6) ----------------------------------
GWT=$(sed -n 's/.*backend-token \([^ ]*\).*/\1/p' /etc/systemd/system/spoond-sshd-gateway.service 2>/dev/null | head -1)
if [ -n "$GWT" ]; then
  imp=$(curl -sk -X POST "$BE_API/api/sandboxes" -H "Authorization: Bearer $GWT" \
    -H "X-Spoond-User-Id: $U2_ID" -H "Content-Type: application/json" \
    -d '{"image":"dev-base","ttl":60}')
  assert_contains "impersonated create owner" "$imp" "\"owner\":\"$U2_ID\""
  # unknown user impersonation -> 403
  code=$(curl -sk -o /dev/null -w '%{http_code}' -X POST "$BE_API/api/sandboxes" \
    -H "Authorization: Bearer $GWT" -H "X-Spoond-User-Id: u-nope" \
    -H "Content-Type: application/json" -d '{"image":"dev-base","ttl":60}')
  assert_eq "impersonate unknown user -> 403" "$code" "403"
else
  bad "identity: no gateway token found (unit missing)"
fi

# --- cleanup -------------------------------------------------------------
[ -n "${lid:-}" ] && curl -sk -X DELETE "$BE_API/api/sandboxes/$lid" -H "Authorization: Bearer $ADMIN_TOK" >/dev/null
curl -sk -X DELETE "$BE_API/api/users/$U1_ID" -H "Authorization: Bearer $ADMIN_TOK" >/dev/null
curl -sk -X DELETE "$BE_API/api/users/$U2_ID" -H "Authorization: Bearer $ADMIN_TOK" >/dev/null
ok "identity: cleanup"

summary
