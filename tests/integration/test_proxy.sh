#!/bin/bash
# E2E test for #15 HTTP proxy: create lease, start http.server inside,
# fetch through <lease-id>.sandbox.lacy.casa (Caddy TLS → backend proxy).
source "$(dirname "$0")/lib.sh"
set -u

echo "== proxy: create lease + start web server inside =="
L=$(new_lease dev-base 300 false)
[ -n "$L" ] || { echo "  ❌ no lease"; exit 1; }
ok "create dev-base lease ($L)"
sleep 3

# Start a python http.server on port 3000 inside the sandbox. One-shot
# exec kills background children on return, so detach with setsid into a
# new session (a real user would run it in tmux, which survives too).
R=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"setsid python3 -m http.server 3000 --bind 0.0.0.0 >/tmp/httpd.log 2>&1 < /dev/null & sleep 2; pgrep -f http.server >/dev/null && echo SERVER_UP || echo SERVER_DOWN"}')
echo "  server start: $(echo "$R" | head -c 200)"

echo
echo "== proxy: default port 3000 =="
OUT=$(curl -sk --max-time 20 "https://$L.sandbox.lacy.casa/" 2>&1)
RC=$?
echo "  rc=$RC"
echo "$OUT" | grep -oE 'Directory listing for /|http.server' | head -1
if echo "$OUT" | grep -q 'Directory listing for'; then ok "public URL serves sandbox web server"; else bad "public URL serves sandbox web server (rc=$RC out=$(echo "$OUT" | head -c 120))"; fi

echo
echo "== proxy: custom port + Host passthrough =="
# Start a custom header-echo server on port 8001 (setsid detach).
R2=$(api POST "/api/sandboxes/$L/exec" '{"cmd":"cat > /tmp/hdr.py <<EOF\nfrom http.server import BaseHTTPRequestHandler, HTTPServer\nclass H(BaseHTTPRequestHandler):\n    def do_GET(self):\n        body=(\"host=\" + self.headers.get(\"Host\",\"\")).encode()\n        self.send_response(200); self.send_header(\"Content-Length\", str(len(body))); self.end_headers(); self.wfile.write(body)\n    def log_message(self, *a): pass\nHTTPServer((\"0.0.0.0\", 8001), H).serve_forever()\nEOF\nsetsid python3 /tmp/hdr.py >/tmp/hdr.log 2>&1 < /dev/null & sleep 2; pgrep -f hdr.py >/dev/null && echo HDR_UP || echo HDR_DOWN"}')
echo "  hdr start: $(echo "$R2" | head -c 100)"
sleep 1
OUT3=$(curl -sk --max-time 20 "https://$L-8001.sandbox.lacy.casa/" 2>&1)
echo "  host echo: $OUT3"
if echo "$OUT3" | grep -q "$L.*sandbox.lacy.casa"; then ok "custom port + Host passthrough"; else bad "custom port + Host passthrough (got: $OUT3)"; fi

echo
echo "== proxy: unknown lease 404 =="
OUT4=$(curl -sk --max-time 15 "https://ffffffffffffffffffffffffffffffff.sandbox.lacy.casa/" -o /dev/null -w '%{http_code}' 2>&1)
if [ "$OUT4" = "404" ]; then ok "unknown lease 404"; else bad "unknown lease 404 (got $OUT4)"; fi

echo
echo "== proxy: cleanup =="
del_lease "$L"
echo "cleaned"
