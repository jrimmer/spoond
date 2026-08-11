#!/bin/bash
# test_acp.sh — ACP smoke test (ticket #24): spawn ONE spoond acp,
# drive initialize → session/new → session/prompt over the same
# process, confirm the protocol handshake works and the prompt returns
# a result without hanging.
#
# ACP sessions live inside a server process, so all requests must go
# through a single spawn. The live prompt needs the LLM gateway; if
# the upstream is unreachable the prompt still returns a JSON-RPC
# error (never hangs) — which is what we assert here.
set -u
# shellcheck source=/dev/null
. "$(dirname "$0")/lib.sh"

ACP_BIN="${ACP_BIN:-/opt/spoond/spoond}"

if [ ! -x "$ACP_BIN" ]; then
  echo "skip: $ACP_BIN not found"
  exit 0
fi

# Go binaries verify TLS; backend cert has vm2.lacy.casa SAN only.
GO_BE_API="${BE_API/https:\/\/127.0.0.1:8890/https:\/\/vm2.lacy.casa:8890}"

rpc() {
  local id="$1" method="$2" params="${3:-}"
  [ -z "$params" ] && params="{}"
  printf '{"jsonrpc":"2.0","id":%s,"method":"%s","params":%s}\n' "$id" "$method" "$params"
}

# Feed the whole conversation to ONE server process, sleep between
# requests so the server has time to respond, then read all output.
OUT=$( {
  rpc 1 initialize '{"protocolVersion":1,"clientInfo":{"name":"itest"}}'
  sleep 1
  rpc 2 session/new '{"cwd":"/root"}'
  sleep 3
  rpc 3 session/prompt '{"sessionId":"sess-0","prompt":[{"type":"text","text":"run uname -a and tell me the kernel"}]}'
  sleep 25
} | FORKD_BACKEND_URL="$GO_BE_API" FORKD_TOKEN="$TOKEN" FORKD_LLM_MODEL="gpt-oss-20b-fireworks" timeout 40 "$ACP_BIN" acp 2>/dev/null )

INIT=$(echo "$OUT" | sed -n '1p')
NEW=$(echo "$OUT" | sed -n '2p')
PROMPT=$(echo "$OUT" | tail -1)

assert_contains "acp initialize ok" "$INIT" "spoond-acp"
assert_contains "acp protocol version" "$INIT" "protocolVersion"
assert_contains "acp session/new ok" "$NEW" "sessionId"

# The prompt must return a JSON-RPC result or error — either way it is
# a protocol-level response, never a hang/empty.
if echo "$PROMPT" | grep -q '"stopReason"'; then
  ok "acp prompt returns stop reason"
elif echo "$PROMPT" | grep -q '"error"'; then
  echo "  (prompt returned an error — LLM gateway upstream unreachable; protocol path exercised)"
  ok "acp prompt surfaces a JSON-RPC error (no hang)"
else
  bad "acp prompt returns a response (got: $(echo "$PROMPT" | head -c 120))"
fi
