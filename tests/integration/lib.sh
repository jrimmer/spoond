#!/bin/bash
# lib.sh — shared helpers for forkd integration tests.
# Sourced by each test file. Requires: BE_API (backend base URL),
# TOKEN (consumer token), and optional SSHHOST for remote execution.
set -u

# Backend API (https, self-signed — always -k).
BE_API="${BE_API:-https://127.0.0.1:8890}"
TOKEN="${TOKEN:-}"
if [ -z "$TOKEN" ] && [ -f /etc/forkd-backend.env ]; then
  # Format: CONSUMER_TOKENS=<token>=<consumer-id> — take the part
  # before the first '=' after the key.
  TOKEN=$(grep -oE 'CONSUMER_TOKENS=[^ ]+' /etc/forkd-backend.env | cut -d= -f2- | cut -d= -f1)
fi

# Optional: run everything through ssh (tests run from Hermes host).
SSHHOST="${SSHHOST:-}"
run() { # run <cmd...> — locally or via ssh
  if [ -n "$SSHHOST" ]; then
    timeout 120 ssh -o BatchMode=yes -o ConnectTimeout=6 "$SSHHOST" "$@"
  else
    "$@"
  fi
}

# --- counters ------------------------------------------------------------
# Results are appended to $RESULTS_FILE (set by run.sh; truncated there)
# so that test files running in subshells contribute to the parent's
# summary. Without this the orchestrator only sees its own assertions.
RESULTS_FILE="${RESULTS_FILE:-/tmp/forkd-itest-results.txt}"
PASS=0; FAIL=0; FAILED_NAMES=()

ok()   { PASS=$((PASS+1)); echo "OK $1" >> "$RESULTS_FILE"; echo "  ✅ $1"; }
bad()  { FAIL=$((FAIL+1)); FAILED_NAMES+=("$1"); echo "FAIL $1" >> "$RESULTS_FILE"; echo "  ❌ $1"; }

assert_eq() { # assert_eq <desc> <actual> <expected>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi
}
assert_contains() { # assert_contains <desc> <haystack> <needle>
  if echo "$2" | grep -q -- "$3"; then ok "$1"; else bad "$1 (missing '$3')"; fi
}
assert_success() { # assert_success <desc> <cmd...>
  if "$@" >/dev/null 2>&1; then ok "$1"; else bad "$1 (cmd failed: $*)"; fi
}
assert_status() { # assert_status <desc> <expected_code> <url>
  local code
  code=$(curl -sk -o /dev/null -w '%{http_code}' "${@:3}")
  assert_eq "$1" "$code" "$2"
}

# --- API helpers ---------------------------------------------------------
api() { # api <method> <path> [data]
  local method="$1" path="$2" data="${3:-}"
  if [ -n "$data" ]; then
    curl -sk -X "$method" "$BE_API$path" -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" -d "$data"
  else
    curl -sk -X "$method" "$BE_API$path" -H "Authorization: Bearer $TOKEN"
  fi
}

# new_lease <image> [ttl] [persistent] -> echoes lease id (or empty)
new_lease() {
  local image="${1:-dev-base}" ttl="${2:-120}" pers="${3:-false}"
  local body
  body=$(api POST /api/sandboxes "{\"image\":\"$image\",\"ttl\":$ttl,\"persistent\":$pers}")
  echo "$body" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null
}

del_lease() { api DELETE "/api/sandboxes/$1" >/dev/null; }

# lease_image <id> -> echoes image tag
lease_image() {
  api GET "/api/sandboxes" | python3 -c "
import sys,json
d=json.load(sys.stdin)
for l in d.get('sandboxes', d):
    if l.get('id')=='$1': print(l.get('image',''))
" 2>/dev/null
}

# wait_exec_ok <id> <cmd> — poll exec until stdout contains needle or timeout
wait_agent() {
  local id="$1" needle="${2:-pong}" tries="${3:-12}"
  for i in $(seq 1 "$tries"); do
    sleep 3
    local out
    out=$(api POST "/api/sandboxes/$id/exec" "{\"cmd\":\"echo ready\"}" 2>/dev/null)
    if echo "$out" | grep -q "$needle"; then return 0; fi
  done
  return 1
}

summary() {
  # Aggregate results contributed by subshell test files (results file).
  PASS=$(grep -c '^OK ' "$RESULTS_FILE" 2>/dev/null || true)
  FAIL=$(grep -c '^FAIL ' "$RESULTS_FILE" 2>/dev/null || true)
  FAILED_NAMES=($(grep '^FAIL ' "$RESULTS_FILE" 2>/dev/null | sed 's/^FAIL //'))
  echo
  echo "==================== SUMMARY ===================="
  echo "PASS: $PASS   FAIL: $FAIL"
  if [ "$FAIL" -gt 0 ]; then
    printf 'Failed: %s\n' "${FAILED_NAMES[@]}"
    return 1
  fi
  echo "ALL TESTS PASSED"
  return 0
}
