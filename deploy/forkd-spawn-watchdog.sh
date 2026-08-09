#!/bin/bash
# forkd-spawn-watchdog.sh — auto-recover from the forkd spawn outage.
#
# Failure signature (seen repeatedly): warm pool logs
#   "socket ... never appeared within 10s"  (controller)
#   "warmPool: spawn ..."                    (backend)
# and/or the pool is empty (0 firecrackers) while the backend is active.
#
# BEHAVIOUR
#   1. Detect: journald failure strings + pool-emptiness check.
#   2. CAPTURE DIAGNOSTICS FIRST (tarball under /var/log/forkd/watchdog/)
#      so the failure can be triaged and corrected after recovery —
#      recovery destroys the evidence (kills firecrackers, removes
#      daemon dirs), so the snapshot must happen BEFORE any action.
#   3. Recover (identical to the manual fix, minus the netns mistake):
#        a. kill -9 all firecracker processes  (NEVER touch /var/run/netns)
#        b. remove stale daemon dirs lacking child-1.sock
#        c. restart forkd-controller (fresh live_vms -> slots freed)
#        d. backend reconcile + pool refill handle the rest
#   4. Log the diagnostics path + intervention outcome to journald.
#
# Test mode: FORKD_WATCHDOG_TEST_CAPTURE=1 runs only the diagnostics
# capture (no recovery) and exits 0 — for verifying the pipeline.
set -u

LOG_TAG="forkd-watchdog"
DIAG_ROOT="/var/log/forkd/watchdog"
KEEP_DIAGS=5

log() { logger -t "$LOG_TAG" "$1"; echo "$1"; }

# --- Diagnostics capture (BEFORE any action) -------------------------------
# Writes a timestamped tarball with everything needed to triage:
# trigger reason, controller+backend journals, firecracker process table,
# daemon-dir inventory, netns list, controller sandbox list, dmesg OOM
# kills, and service status. Keeps the newest KEEP_DIAGS tarballs.
capture_diagnostics() {
    local reason="$1"
    mkdir -p "$DIAG_ROOT"
    local ts dir tarball
    ts=$(date +%Y%m%d-%H%M%S)
    dir="$DIAG_ROOT/$ts"
    tarball="$DIAG_ROOT/forkd-watchdog-$ts.tar.gz"
    mkdir -p "$dir"

    {
        echo "trigger: $reason"
        echo "captured_at_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "backend_active: $(systemctl is-active forkd-backend 2>/dev/null || echo unknown)"
        echo "controller_active: $(systemctl is-active forkd-controller 2>/dev/null || echo unknown)"
        echo "runner_active: $(systemctl is-active forkd-runner 2>/dev/null || echo unknown)"
        echo "firecrackers_running: $(pgrep -c firecracker 2>/dev/null || echo 0)"
        echo "netns_provisioned: $(ls /var/run/netns/ 2>/dev/null | wc -l)"
    } > "$dir/trigger.txt"

    journalctl -u forkd-controller --since "30 min ago" --no-pager > "$dir/controller-journal.txt" 2>&1
    journalctl -u forkd-backend    --since "30 min ago" --no-pager > "$dir/backend-journal.txt" 2>&1
    journalctl -u forkd-runner     --since "30 min ago" --no-pager > "$dir/runner-journal.txt" 2>&1

    ps -eo pid,etimes,comm,args | grep '[f]irecracker' > "$dir/firecrackers.txt" 2>&1

    {
        echo "daemon-dir inventory (child-1.sock present = live VM; missing = stale)"
        for d in /tmp/forkd-daemon-*/; do
            [ -d "$d" ] || continue
            local sock=no
            [ -e "${d}child-1.sock" ] && sock=yes
            echo "$(stat -c '%y' "$d" | cut -d. -f1) sock=$sock $d"
        done
    } > "$dir/daemon-dirs.txt" 2>&1

    ls -la /var/run/netns/ > "$dir/netns.txt" 2>&1
    curl -s http://127.0.0.1:8889/v1/sandboxes > "$dir/sandboxes.json" 2>&1
    dmesg 2>/dev/null | grep -iE 'oom|killed process|firecracker' | tail -50 > "$dir/dmesg.txt" 2>&1
    systemctl status forkd-backend forkd-controller forkd-runner --no-pager > "$dir/services.txt" 2>&1

    tar czf "$tarball" -C "$dir" . 2>/dev/null
    rm -rf "$dir"

    # Rotate: keep the newest KEEP_DIAGS tarballs.
    ls -1t "$DIAG_ROOT"/forkd-watchdog-*.tar.gz 2>/dev/null | tail -n +$((KEEP_DIAGS+1)) | xargs -r rm -f

    log "diagnostics captured: $tarball (before recovery)"
    echo "$tarball"
}

# --- Healthy check ---------------------------------------------------------
NOW=$(date +%s)
CTL_FAILS=$(journalctl -u forkd-controller --since "15 min ago" --no-pager 2>/dev/null | grep -c "socket.*never appeared")
BACKEND_FAILS=$(journalctl -u forkd-backend --since "15 min ago" --no-pager 2>/dev/null | grep -cE "socket never|warmPool: spawn .*error")
BACKEND_ACTIVE=$(systemctl is-active forkd-backend 2>/dev/null)
FIRECRACKERS=$(pgrep -c firecracker 2>/dev/null || echo 0)

# Test mode: exercise the diagnostics capture without recovering.
if [ "${FORKD_WATCHDOG_TEST_CAPTURE:-0}" = "1" ]; then
    capture_diagnostics "TEST_CAPTURE (no recovery performed)"
    exit 0
fi

# A healthy idle system has ~12 firecrackers (3 x 4 images). 0 with an
# active backend for >15 min = pool wedged even if no error string
# matched (e.g. log rotation, different message).
if [ "$CTL_FAILS" -eq 0 ] && [ "$BACKEND_FAILS" -eq 0 ] && { [ "$BACKEND_ACTIVE" != "active" ] || [ "$FIRECRACKERS" -gt 0 ]; }; then
    exit 0
fi

REASON="controller_fails=$CTL_FAILS backend_fails=$BACKEND_FAILS firecrackers=$FIRECRACKERS backend_active=$BACKEND_ACTIVE"
log "spawn failure signature detected ($REASON); capturing diagnostics, then recovering"
DIAG_TARBALL=$(capture_diagnostics "$REASON")

# --- Recovery --------------------------------------------------------------
# 1. Kill firecrackers (per-PID kill -9 handles stragglers).
pkill -9 firecracker 2>/dev/null
sleep 2
for pid in $(pgrep firecracker 2>/dev/null); do
    kill -9 "$pid" 2>/dev/null
done
sleep 2

# 2. Remove stale daemon dirs (no child-1.sock => no live VM).
removed=0
for d in /tmp/forkd-daemon-*/; do
    [ -d "$d" ] || continue
    if [ ! -e "${d}child-1.sock" ]; then
        rm -rf "$d" && removed=$((removed+1))
    fi
done
log "removed $removed stale daemon dirs (netns untouched)"

# 3. Restart controller (clears live_vms; snapshots stay on disk).
systemctl kill -s KILL forkd-controller 2>/dev/null
systemctl reset-failed forkd-controller 2>/dev/null
systemctl start forkd-controller 2>/dev/null

sleep 10
if ! systemctl is-active --quiet forkd-controller; then
    log "FORKD-WATCHDOG: controller failed to restart after recovery; manual intervention required (diags: $DIAG_TARBALL)"
    exit 1
fi
log "forkd-controller restarted; warm pool rebuilding (diags: $DIAG_TARBALL)"

exit 1
