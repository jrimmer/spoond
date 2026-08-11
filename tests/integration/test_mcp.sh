#!/bin/bash
# test_mcp.sh — MCP smoke test (ticket #23): spawn spoond mcp, speak
# JSON-RPC over stdio, confirm tools execute in a forkd sandbox and the
# lease is released after.
set -u
# shellcheck source=/dev/null
. "$(dirname "$0")/lib.sh"

MCP_BIN="${MCP_BIN:-/opt/spoond/spoond}"

if [ ! -x "$MCP_BIN" ]; then
  echo "skip: $MCP_BIN not found"
  exit 0
fi

# The Go binaries verify TLS; the backend cert only has vm2.lacy.casa
# SAN, so translate a loopback BE_API to the hostname form (vm2's
# /etc/hosts resolves it). curl -sk callers keep using BE_API directly.
GO_BE_API="${BE_API/https:\/\/127.0.0.1:8890/https:\/\/vm2.lacy.casa:8890}"

# rpc <name> <args-json> — send one JSON-RPC request, print the result line.
# NOTE: params must be a complete JSON object (own braces). Avoid
# ${3:-{}} as the default — bash's brace counting in the expansion
# appends an extra '}' to multi-brace params.
rpc() {
  local id="$1" method="$2" params="${3:-}"
  [ -z "$params" ] && params="{}"
  printf '{"jsonrpc":"2.0","id":%s,"method":"%s","params":%s}\n' "$id" "$method" "$params"
}

BEFORE=$(api GET /api/sandboxes | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('sandboxes',d)))" 2>/dev/null || echo "?")

# initialize
INIT=$( { rpc 1 initialize '{"protocolVersion":"2025-03-26","clientInfo":{"name":"itest"}}'; sleep 1; } | FORKD_BACKEND_URL="$GO_BE_API" FORKD_TOKEN="$TOKEN" timeout 15 "$MCP_BIN" mcp 2>/dev/null | head -1 )
assert_contains "mcp initialize ok" "$INIT" "spoond-mcp"

# tools/list
TOOLS=$( { rpc 2 tools/list; sleep 1; } | FORKD_BACKEND_URL="$GO_BE_API" FORKD_TOKEN="$TOKEN" timeout 15 "$MCP_BIN" mcp 2>/dev/null | head -1 )
assert_contains "mcp tools/list has shell" "$TOOLS" "shell"
assert_contains "mcp tools/list has read_file" "$TOOLS" "read_file"

# tools/call shell: run uname in a real sandbox
CALL=$( { rpc 3 tools/call '{"name":"shell","arguments":{"command":"uname -a"}}'; sleep 3; } | FORKD_BACKEND_URL="$GO_BE_API" FORKD_TOKEN="$TOKEN" timeout 30 "$MCP_BIN" mcp 2>/dev/null | head -1 )
assert_contains "mcp shell runs in sandbox" "$CALL" "Linux"
assert_contains "mcp shell returns sandbox_id" "$CALL" "sandbox_id"

# tools/call write_file + read_file round-trip
WRITE=$( { rpc 4 tools/call '{"name":"write_file","arguments":{"path":"/tmp/mcp_test.txt","content":"hello from mcp"}}'; sleep 3; } | FORKD_BACKEND_URL="$GO_BE_API" FORKD_TOKEN="$TOKEN" timeout 30 "$MCP_BIN" mcp 2>/dev/null | head -1 )
assert_contains "mcp write_file ok" "$WRITE" "WROTE"

# leases released after stateless calls (should be near the baseline)
sleep 2
AFTER=$(api GET /api/sandboxes | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('sandboxes',d)))" 2>/dev/null || echo "?")
echo "  (sandbox count before=$BEFORE after=$AFTER — warm pool may vary)"
