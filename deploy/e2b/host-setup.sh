#!/bin/bash
# U04 host bring-up, steps 1-5, 7 and 9 of
# docs/plans/2026-09-30-e2b-substrate/U04-host-bringup.md.
# Runs as root on the E2B host. Steps 6, 7b, 8 and 10 are applied separately.
set -euo pipefail

# Step 1: packages and modules
apt-get install -y --no-install-recommends iptables rsync e2fsprogs iproute2 util-linux curl nftables
printf 'nbd\ntun\nkvm\n' > /etc/modules-load.d/e2b.conf
echo 'options nbd nbds_max=256 max_part=16' > /etc/modprobe.d/e2b-nbd.conf
modprobe nbd nbds_max=256 max_part=16
modprobe tun
test "$(cat /sys/module/nbd/parameters/nbds_max)" -ge 256   # if this fails, nbd was already loaded with a smaller nbds_max: STOP
cat > /etc/udev/rules.d/97-nbd-device.rules <<'EOF'
KERNEL=="nbd*", GROUP="disk", MODE="0660"
EOF
udevadm control --reload

# Step 2: sysctls and hugepages (MemAvailable guard first)
awk '/^MemAvailable:/ { if ($2 < 33554432) { print "short: " $2 " kB"; exit 1 } }' /proc/meminfo
cat > /etc/sysctl.d/90-e2b.conf <<'EOF'
vm.nr_hugepages = 12288
vm.max_map_count = 1048576
net.ipv4.ip_forward = 1
EOF
sysctl -p /etc/sysctl.d/90-e2b.conf
grep HugePages_Total /proc/meminfo   # must print 12288

# Step 3: MSS clamp (same rule as E2B Embed)
iptables -w 5 -t mangle -C FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null \
  || iptables -w 5 -t mangle -I FORWARD 1 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu

# Step 4: storage
zfs list forkdcache/e2b >/dev/null 2>&1 || zfs create -o mountpoint=/forkdcache/e2b forkdcache/e2b
mkdir -p /forkdcache/e2b/{orchestrator,storage/templates,storage/build-cache,tmp,registry}
chmod 700 /forkdcache/e2b/tmp

# Step 5: artifacts (exact URLs and SHA-256s)
B=https://storage.googleapis.com/e2b-artifact-binaries
fetch() { # url dest mode sha256
  install -d "$(dirname "$2")"
  curl -fsSL "$1" -o "$2.tmp"
  echo "$4  $2.tmp" | sha256sum -c -
  install -m "$3" "$2.tmp" "$2" && rm -f "$2.tmp"
}
fetch $B/firecrackers/v1.14-0.2.0/amd64/firecracker /fc-versions/v1.14-0.2.0/amd64/firecracker 0755 ef22aec7cbffcf6cc44a8436a4db79f9e6fe5c52218c81af321dd20f10ad6e5d
fetch $B/kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin /fc-kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin 0644 9191ced12d24e6e381753a7ab12ec850877524d453eae75c20aeb176f3b5ad05
fetch $B/busybox/1.36.1/amd64/busybox /fc-busybox/1.36.1/amd64/busybox 0755 d7cce939adb09a41a22a5f846d22ba8d576b38dbb2b46a5c77a3a3e27ec52520
install -D -m 0755 /root/src/e2b-runtime/packages/envd/bin/envd /fc-envd/envd
install -D -m 0755 /root/src/e2b-runtime/packages/orchestrator/bin/orchestrator /usr/local/lib/e2b/orchestrator
/fc-versions/v1.14-0.2.0/amd64/firecracker --version | head -1

# Step 7: host firewall (nftables table inet e2b_guard, loaded by e2b-guard.service)
install -d /etc/nftables.d
cat > /etc/nftables.d/e2b-guard.nft <<'EOF'
table inet e2b_guard
delete table inet e2b_guard
table inet e2b_guard {
  set sandbox_src {
    type ipv4_addr; flags interval;
    elements = { 10.11.0.0/16, 10.12.0.0/16 }
  }
  chain input {
    type filter hook input priority -10; policy accept;
    iif "lo" accept
    # Sandboxes may reach only the egress proxy and hyperloop.
    ip saddr @sandbox_src tcp dport { 5010, 5016, 5017, 5018 } accept
    ip saddr @sandbox_src ct state established,related accept
    ip saddr @sandbox_src drop
    # Nobody off-host may reach orchestrator ports.
    tcp dport { 5007, 5008, 5010, 5011, 5012, 5016, 5017, 5018 } drop
  }
}
EOF
cat > /etc/systemd/system/e2b-guard.service <<'EOF'
[Unit]
Description=E2B host firewall (nftables table inet e2b_guard)
After=network-pre.target
Before=e2b-orchestrator.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/nftables.d/e2b-guard.nft
ExecStop=/usr/sbin/nft delete table inet e2b_guard

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now e2b-guard.service
nft list table inet e2b_guard >/dev/null

# Step 9: local registry (docker)
docker inspect spoond-registry >/dev/null 2>&1 || docker run -d --name spoond-registry --restart=always \
  -p 127.0.0.1:5000:5000 \
  -v /forkdcache/e2b/registry:/var/lib/registry \
  registry:2.8.3
curl -fsS http://127.0.0.1:5000/v2/   # prints {}
