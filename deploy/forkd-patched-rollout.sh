#!/bin/bash
# forkd-patched-rollout.sh — build forkd from a branch of our fork and
# install it on this host.
#
# Why this exists: the forkd fixes our instance needs are small and
# upstream-PR-shaped, but they land on the host before the upstream
# release cadence does. This builds a chosen branch of our fork *on the
# host*, keeps the binaries it replaces, and leaves the restart to the
# operator.
#
# The restart is deliberately not automatic. forkd-controller's startup
# reconcile prunes sandboxes it cannot vouch for, so restarting it while
# CI is running kills live jobs — see the controller-restart findings in
# the Forgejo tickets. Install and restart are separate decisions.
#
# Usage:
#   deploy/forkd-patched-rollout.sh --branch fix/bake-space-and-staging
#   deploy/forkd-patched-rollout.sh --branch <branch> --restart
#   deploy/forkd-patched-rollout.sh --rollback
#
# Env:
#   FORKD_SRC_DIR  checkout of our fork      (default ~/work/forkd)
#   FORKD_REMOTE   git remote to fetch from  (default: fork, i.e. jrimmer/forkd)
#   BIN_DIR        install directory         (default /usr/local/bin)
set -euo pipefail

# The host may be a root-only box with no sudo installed at all.
if [ "$(id -u)" -eq 0 ]; then SUDO=""; else SUDO="sudo"; fi

FORKD_SRC_DIR="${FORKD_SRC_DIR:-$HOME/work/forkd}"
FORKD_REMOTE="${FORKD_REMOTE:-fork}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

BRANCH=""
RESTART=0
ROLLBACK=0

die() { echo "forkd-patched-rollout: $*" >&2; exit 1; }

usage() {
  sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --branch)   BRANCH="${2:-}"; [ -n "$BRANCH" ] || die "--branch needs a value"; shift 2 ;;
    --restart)  RESTART=1; shift ;;
    --rollback) ROLLBACK=1; shift ;;
    -h|--help)  usage ;;
    *)          die "unknown argument: $1 (try --help)" ;;
  esac
done

[ -d "$FORKD_SRC_DIR/.git" ] || die "no git checkout at $FORKD_SRC_DIR (set FORKD_SRC_DIR)"

latest_backup() {
  # Backups are named <binary>.bak-<epoch>; newest wins.
  ls -1 "$BIN_DIR"/"$1".bak-* 2>/dev/null | sort -t- -k2 -n | tail -1
}

install_binary() {
  local name="$1" src="$2"
  local dest="$BIN_DIR/$name"
  [ -f "$src" ] || die "built binary missing: $src"
  if [ -f "$dest" ]; then
    local bak="$dest.bak-$(date +%s)"
    cp -p "$dest" "$bak"
    echo "    backed up $dest → $bak"
  fi
  install -m 0755 "$src" "$dest"
  echo "    installed $dest"
}

if [ "$ROLLBACK" = 1 ]; then
  for name in forkd forkd-controller; do
    bak="$(latest_backup "$name")"
    [ -n "$bak" ] || die "no backup of $name in $BIN_DIR — nothing to roll back to"
    install -m 0755 "$bak" "$BIN_DIR/$name"
    echo "    restored $BIN_DIR/$name from $bak"
  done
  echo
  echo "Rolled back. Restart to take effect (this prunes live sandboxes):"
  echo "  ${SUDO:+$SUDO }systemctl restart forkd-controller"
  exit 0
fi

[ -n "$BRANCH" ] || die "--branch is required (or use --rollback)"

echo "== forkd patched rollout =="
echo "  source: $FORKD_SRC_DIR (remote: $FORKD_REMOTE)"
echo "  branch: $BRANCH"

git -C "$FORKD_SRC_DIR" fetch --prune "$FORKD_REMOTE"
git -C "$FORKD_SRC_DIR" rev-parse --verify --quiet "$FORKD_REMOTE/$BRANCH" >/dev/null \
  || die "$FORKD_REMOTE/$BRANCH not found — push the branch first"
git -C "$FORKD_SRC_DIR" checkout --quiet -B "$BRANCH" "$FORKD_REMOTE/$BRANCH"

echo "  head:   $(git -C "$FORKD_SRC_DIR" log --oneline -1)"

echo "== build (release) =="
# The CLI carries every fix a bare-metal bake needs (forkd snapshot,
# parent build); the controller carries the daemon-side ones.
( cd "$FORKD_SRC_DIR" && cargo build --release -p forkd-cli -p forkd-controller )

echo "== install =="
install_binary forkd "$FORKD_SRC_DIR/target/release/forkd"
install_binary forkd-controller "$FORKD_SRC_DIR/target/release/forkd-controller"

echo
echo "  installed version: $("$BIN_DIR/forkd" --version 2>/dev/null || echo '(no --version)')"

if [ "$RESTART" = 1 ]; then
  echo "== restart controller (prunes live sandboxes) =="
  $SUDO systemctl restart forkd-controller
  $SUDO systemctl --no-pager --lines=0 status forkd-controller || true
else
  echo
  echo "Installed but NOT restarted. The running daemon still has the old"
  echo "code, and a restart prunes live sandboxes — so drain first:"
  echo "  systemctl stop spoond-runner"
  echo "  curl -s http://127.0.0.1:8889/v1/sandboxes   # DELETE stale ids"
  echo "  ${SUDO:+$SUDO }systemctl restart forkd-controller"
  echo "  systemctl start spoond-runner"
fi
