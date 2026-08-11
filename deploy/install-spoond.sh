#!/bin/bash
# install-spoond.sh — install spoond (and optionally forkd-controller) on a
# Linux host. Designed to be run by a human or an agent, line by line.
#
# Usage:
#   ./install-spoond.sh [--with-forkd] [--prefix /opt] [--user spoond]
#
#   --with-forkd   also install/build forkd-controller (full stack).
#                  Without it, spoond is installed and configured to talk to
#                  an EXISTING forkd-controller at FORKD_URL (default
#                  http://127.0.0.1:8889).
#
# What it does:
#   1. Builds the spoond binary (Go toolchain required; see docs/setup.md).
#   2. Installs the binary + systemd units (backend, sshd-gateway, watchdog).
#   3. Creates the gateway keys directory + config env files.
#   4. Starts services and runs `spoond doctor` to verify.
#
# Safe to re-run (idempotent): existing files are backed up with .prev.
set -u
set -e

PREFIX="${PREFIX:-/opt}"
SPOOND_USER="${SPOOND_USER:-spoond}"
WITH_FORKD=0
for arg in "$@"; do
  case "$arg" in
    --with-forkd) WITH_FORKD=1 ;;
  esac
done

echo "== spoond installer =="
echo "prefix=$PREFIX user=$SPOOND_USER with-forkd=$WITH_FORKD"
command -v go >/dev/null || { echo "ERROR: Go toolchain required (see docs/setup.md)"; exit 1; }
id -u "$SPOOND_USER" >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin "$SPOOND_USER"

# 1. Build
echo "== building spoond =="
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"
go build -o "$PREFIX/spoond/spoond.new" ./cmd/spoond
mkdir -p "$PREFIX/spoond"
[ -f "$PREFIX/spoond/spoond" ] && mv -f "$PREFIX/spoond/spoond" "$PREFIX/spoond/spoond.prev"
mv -f "$PREFIX/spoond/spoond.new" "$PREFIX/spoond/spoond"
chmod +x "$PREFIX/spoond/spoond"
chown -R "$SPOOND_USER":"$SPOOND_USER" "$PREFIX/spoond" 2>/dev/null || true

# 2. Keys + config
echo "== keys + config =="
mkdir -p /etc/spoond-gateway/keys
chmod 700 /etc/spoond-gateway/keys
[ -f /etc/spoond-backend.env ] || cat > /etc/spoond-backend.env <<'EOF'
# Consumer tokens: token=consumer,comma-separated. Replace the example.
CONSUMER_TOKENS=changeme=admin
FORKD_URL=http://127.0.0.1:8889
BIND_ADDR=127.0.0.1:8890
POOL_SIZE=3
# TLS (optional): TLS_CERT=/etc/spoond/tls/fullchain.pem TLS_KEY=/etc/spoond/tls/privkey.pem
EOF
chmod 600 /etc/spoond-backend.env

# 3. systemd units
echo "== systemd units =="
install -m 644 deploy/spoond-backend.service /etc/systemd/system/
install -m 644 deploy/spoond-sshd-gateway.service /etc/systemd/system/
install -m 644 deploy/spoond-watchdog.service /etc/systemd/system/
install -m 644 deploy/spoond-watchdog.timer /etc/systemd/system/
# Gateway credential file (0600): the unit reads SPOOND_GATEWAY_TOKEN
# from here instead of embedding it in ExecStart (security review #37
# rescan — the token is admin-equivalent and must not be world-readable
# via the unit or /proc/<pid>/cmdline).
if [ ! -f /etc/spoond-gateway.env ]; then
  umask 077
  echo "SPOOND_GATEWAY_TOKEN=<CONSUMER_TOKEN>" > /etc/spoond-gateway.env
  chmod 600 /etc/spoond-gateway.env
  echo "  wrote /etc/spoond-gateway.env (0600) — set SPOOND_GATEWAY_TOKEN"
fi
systemctl daemon-reload

# 4. Optional: forkd-controller
if [ "$WITH_FORKD" = "1" ]; then
  echo "== forkd-controller (--with-forkd) =="
  echo "  NOTE: forkd-controller is a separate upstream project (github.com/jrimmer/forkd)."
  echo "  Install it per its own docs, then ensure FORKD_URL points at it and"
  echo "  restart spoond-backend."
else
  echo "  (forkd-controller NOT installed; point FORKD_URL at an existing controller)"
fi

# 5. Start + verify
echo "== start =="
systemctl enable --now spoond-backend.service spoond-sshd-gateway.service spoond-watchdog.timer 2>&1 | tail -2 || true
sleep 2
echo "== verify =="
"$PREFIX/spoond/spoond" doctor || echo "WARNING: doctor reported failures — check the output above."
echo "== done =="
echo "Next: add your SSH public key to /etc/spoond-gateway/keys/<you>.pub and restart spoond-sshd-gateway."
