#!/bin/bash
source "$(dirname "$0")/lib.sh"
# test_images.sh — per-image capability integration tests.
# Requires: lib.sh sourced, live backend.
set -u

echo "== images: go-base (Go toolchain) =="
L=$(new_lease go-base 120 false)
[ -n "$L" ] && ok "create go-base lease ($L)" || bad "create go-base lease"
sleep 3
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"export GOTOOLCHAIN=local; go version"}')
assert_contains "go version runs" "$OUT" "go1.25"
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"export GOTOOLCHAIN=local GOCACHE=/root/.cache GOPATH=/root/go HOME=/root; cd /tmp && printf \"package main\\nfunc main(){println(\\\"GO_COMPILE_OK\\\")}\\n\" > m.go && go run m.go"}')
assert_contains "go compiles+runs code" "$OUT" "GO_COMPILE_OK"
del_lease "$L"

echo
echo "== images: py-base (Python toolchain) =="
L=$(new_lease py-base 120 false)
[ -n "$L" ] && ok "create py-base lease ($L)" || bad "create py-base lease"
sleep 3
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"python3 -c \"print(\\\"PY_OK\\\")\""}')
assert_contains "python3 runs" "$OUT" "PY_OK"
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"pip --version"}')
assert_contains "pip available" "$OUT" "pip"
del_lease "$L"

echo
echo "== images: elixir-base =="
L=$(new_lease elixir-base 120 false)
[ -n "$L" ] && ok "create elixir-base lease ($L)" || bad "create elixir-base lease"
sleep 3
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"elixir --version"}')
assert_contains "elixir runs" "$OUT" "Elixir"
del_lease "$L"

echo
echo "== images: llm-review =="
L=$(new_lease llm-review 120 false)
[ -n "$L" ] && ok "create llm-review lease ($L)" || bad "create llm-review lease"
sleep 3
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"echo LLM_REVIEW_OK"}')
assert_contains "llm-review shell works" "$OUT" "LLM_REVIEW_OK"
del_lease "$L"

echo
echo "== images: dev-base (interactive: sshd + tmux + stream agent) =="
L=$(new_lease dev-base 120 false)
[ -n "$L" ] && ok "create dev-base lease ($L)" || bad "create dev-base lease"
sleep 3
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"ps aux | grep [s]shd | head -1"}')
assert_contains "sshd running" "$OUT" "sshd"
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"which tmux && echo TMUX_OK"}')
assert_contains "tmux installed" "$OUT" "TMUX_OK"
OUT=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"grep -c stream /forkd-agent.py"}')
STREAMCOUNT=$(echo "$OUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','0').strip())" 2>/dev/null)
if [ "${STREAMCOUNT:-0}" -gt 0 ]; then ok "agent has stream action ($STREAMCOUNT refs)"; else bad "agent has stream action"; fi
del_lease "$L"

echo
echo "== images done =="
