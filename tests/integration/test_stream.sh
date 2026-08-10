#!/bin/bash
source "$(dirname "$0")/lib.sh"
# test_stream.sh — streaming exec (WebSocket PTY) integration tests.
# Requires: lib.sh sourced, wsclient binary built, live backend.
set -u
WSCLIENT="${WSCLIENT:-$(dirname "$0")/wsclient/wsclient}"

echo "== stream: WebSocket connects + streams incrementally =="
L=$(new_lease dev-base 120 false)
[ -n "$L" ] && ok "create stream lease ($L)" || bad "create stream lease"
sleep 3

OUT=$("$WSCLIENT" -mode stream -url "$BE_API" -token "$TOKEN" -lease "$L" \
  -args 'echo STREAM_ONE; sleep 0.3; echo STREAM_TWO; sleep 0.3; echo STREAM_THREE')
assert_contains "stream starts (started frame)" "$OUT" '"stream": "started"'
assert_contains "first output arrives" "$OUT" "STREAM_ONE"
assert_contains "second output arrives" "$OUT" "STREAM_TWO"
assert_contains "third output arrives" "$OUT" "STREAM_THREE"
assert_contains "exit_code reported" "$OUT" '"exit_code": 0'
assert_contains "client sees completion" "$OUT" "STREAM_COMPLETE"

echo
echo "== stream: stdin roundtrip (interactive bash) =="
OUT2=$("$WSCLIENT" -mode stdin -url "$BE_API" -token "$TOKEN" -lease "$L")
assert_contains "stdin reaches process" "$OUT2" "STDIN_ROUNDTRIP_OK"
assert_contains "stdin mode completes" "$OUT2" "STDIN_ROUNDTRIP_OK"

del_lease "$L"
echo
echo "== stream done =="
