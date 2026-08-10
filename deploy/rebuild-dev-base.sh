#!/bin/bash
# Rebuild dev-base with UsePAM no fix (pam_systemd fails in microVM without
# systemd/logind -> pam_open_session() error -> sshd aborts plain exec).
# Based on rebuild_devbase_v3.sh; adds the UsePAM fix in the init patch
# AND in the live step before branching (snapshot captures live state).
set -e
API="http://127.0.0.1:8889"
GWPUB=$(cat /etc/forkd-gateway/gateway_ed25519.pub)

echo "=== 0. delete stale dev-base-new ==="
curl -s -X DELETE "$API/v1/snapshots/dev-base-new" | head -c 120
echo

echo "=== 1. register rootfs ==="
curl -s -X POST "$API/v1/snapshots" -H "Content-Type: application/json" \
  -d '{"tag":"dev-base-new","kernel":"/var/lib/forkd/kernels/vmlinux","rootfs":"/var/cache/forkd/dev-base-rootfs.ext4","rw":true,"tap":"forkd-tap0","boot_wait_secs":15}' | head -c 160
echo

echo "=== 2. spawn ==="
SB=$(curl -s -X POST "$API/v1/sandboxes" -H "Content-Type: application/json" -d '{"snapshot_tag":"dev-base-new","n":1,"per_child_netns":true}' | python3 -c "import sys,json; d=json.load(sys.stdin); print(d[0]['id'] if isinstance(d,list) and d else d.get('id',''))")
echo "sandbox: $SB"
[ -z "$SB" ] && { echo "SPAWN FAILED"; exit 1; }

echo "=== 3. poll agent ==="
ok=0
for i in $(seq 1 12); do
    sleep 5
    R=$(curl -s -X POST "$API/v1/sandboxes/$SB/ping" -H "Content-Type: application/json" -d '{}')
    if echo "$R" | grep -q '"pong"'; then echo "agent up after $((i*5))s"; ok=1; break; fi
done
[ "$ok" != "1" ] && { echo "AGENT NEVER CAME UP"; curl -s -X DELETE "$API/v1/sandboxes/$SB" >/dev/null; exit 1; }

echo "=== 3b. confirm agent has cwd fix ==="
curl -s -X POST "$API/v1/sandboxes/$SB/exec" -H "Content-Type: application/json" -d '{"args":["bash","-c","grep -c \"or None\" /forkd-agent.py"]}' | python3 -c "import sys,json; d=json.load(sys.stdin); print('or-None refs:', d.get('stdout','').strip())"

PATCH=$(cat <<'PATCH_EOF'
# dev-base: start sshd so interactive sessions can reach the sandbox.
mkdir -p /dev/pts /run/sshd
mount -t devpts devpts /dev/pts 2>/dev/null
if [ ! -f /etc/ssh/ssh_host_ed25519_key ]; then
  ssh-keygen -A >/dev/null 2>&1
fi
# MicroVM has no systemd/logind: pam_systemd fails pam_open_session() and
# sshd aborts exec. We authenticate by key only, so disable PAM.
sed -i 's/^UsePAM yes/UsePAM no/' /etc/ssh/sshd_config 2>/dev/null || true
/usr/sbin/sshd 2>/dev/null || echo "sshd failed to start"

# dev-base: attach to (or create) the tmux session on login.
if [ -d /etc/profile.d ]; then
  cat > /etc/profile.d/forkd-tmux.sh <<'EOF2'
if [ -z "$TMUX" ] && [ -z "$FORKD_NO_TMUX" ] && [ -n "$SSH_CONNECTION" ]; then
  tmux attach -t dev 2>/dev/null || tmux new -s dev
fi
EOF2
  chmod +x /etc/profile.d/forkd-tmux.sh
fi
PATCH_EOF
)
B64=$(echo "$PATCH" | base64 -w0)

echo "=== 4. insert dev-base section ==="
curl -s -X POST "$API/v1/sandboxes/$SB/exec" -H "Content-Type: application/json" -d "{\"args\":[\"bash\",\"-c\",\"echo $B64 | base64 -d > /tmp/dbpatch.sh && sed -i '/^echo \\\"forkd-init: launching agent/ r /tmp/dbpatch.sh' /forkd-init.sh && grep -c sshd /forkd-init.sh\"]}" | python3 -c "import sys,json; d=json.load(sys.stdin); print('sshd lines:', d.get('stdout','').strip())"

echo "=== 5. gateway key ==="
echo "=== 5. gateway key (base64, append) ==="
GWB64=$(echo "$GWPUB" | base64 -w0)
curl -s -X POST "$API/v1/sandboxes/$SB/exec" -H "Content-Type: application/json" \
  -d "{\"args\":[\"bash\",\"-c\",\"mkdir -p /root/.ssh && touch /root/.ssh/authorized_keys && echo $GWB64 | base64 -d >> /root/.ssh/authorized_keys; wc -l /root/.ssh/authorized_keys\"]}" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())"

echo "=== 6. UsePAM no + devpts + sshd LIVE + verify pty + verify exec ==="
curl -s -X POST "$API/v1/sandboxes/$SB/exec" -H "Content-Type: application/json" -d '{"args":["bash","-c","mkdir -p /dev/pts /run/sshd; mount -t devpts devpts /dev/pts 2>/dev/null; sed -i \"s/^UsePAM yes/UsePAM no/\" /etc/ssh/sshd_config; grep -n UsePAM /etc/ssh/sshd_config; /usr/sbin/sshd 2>/dev/null; sleep 1; python3 -c \"import pty; m,s=pty.openpty(); print(\\\\\"PTY_OK\\\\\")\""]}' | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('stdout',''))"

echo "=== 7. branch -> dev-base ==="
curl -s -X DELETE "$API/v1/snapshots/dev-base" >/dev/null
curl -s -X POST "$API/v1/sandboxes/$SB/branch" -H "Content-Type: application/json" -d '{"tag":"dev-base"}' | head -c 200
echo
curl -s -X DELETE "$API/v1/sandboxes/$SB" >/dev/null
echo "killed $SB"
echo "REBUILD DONE"
