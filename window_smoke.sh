window_smoke() {
  set -a; . /etc/spoond/conformance.env; set +a
  H="Authorization: Bearer $CONFORMANCE_TOKEN"
  id=$(curl -fsS -H "$H" -d '{"image":"py-base","ttl":120}' "$CONFORMANCE_API/api/sandboxes" \
       | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])') || return 1
  out=$(curl -fsS -H "$H" -d '{"cmd":"echo ok"}' "$CONFORMANCE_API/api/sandboxes/$id/exec" \
       | python3 -c 'import json,sys; print(json.load(sys.stdin)["stdout"].strip())')
  curl -fsS -X DELETE -H "$H" "$CONFORMANCE_API/api/sandboxes/$id" >/dev/null
  [ "$out" = ok ]
}
if window_smoke; then echo SMOKE_OK; else echo SMOKE_FAIL; exit 1; fi
