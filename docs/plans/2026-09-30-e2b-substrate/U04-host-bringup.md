# U04 — host host bring-up (beside forkd)

## Purpose

Install and start the E2B orchestrator on the host next to the running forkd,
without disturbing forkd or CI.

## Preconditions

- U03 is done. The binaries are at
  `/root/src/e2b-runtime/packages/{orchestrator,envd}/bin/`.
- All commands run on the host as root.

## Facts relied on

- **Host:**
  - Debian 13, kernel 6.17, cgroup v2, 4 KiB pages.
  - `net.ipv4.ip_forward=1` and `vm.max_map_count=1048576` are already set.
  - `vm.nr_hugepages=0`; `tun` and `nbd` are not loaded; `kvm_intel` is
    loaded.
  - Checked 2026-09-30.
- **iptables:** filter `FORWARD` policy is `DROP` (Docker). E2B appends
  per-slot `FORWARD` accepts in both directions (A3 D4), so nothing more is
  needed for sandbox traffic.
- **Primary address:** `10.0.0.11` on `vmbr0`, which is the default-route
  interface (E2B requires a default route; A3 D3).
- **Artifacts:** URLs and SHA-256s in A3 A1.7.
- **Host prerequisites:** A3 E4.
- **Ports:** every orchestrator listener except pprof binds `0.0.0.0`
  (A3 A5).
- **Port 4317** is already in use on the host (an unrelated `otel-plugin`
  process).

## Steps

### 1. Packages and modules

```bash
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
```

`nbds_max=256` covers the 100-sandbox cap (one NBD device per running
sandbox) plus `NBD_POOL_SIZE=32` of headroom.

### 2. Sysctls and hugepages

First check that the host has room (production forkd VMs are running).
`MemAvailable` must be at least 32 GiB (24 GiB of hugepages plus 8 GiB of
margin); otherwise STOP and report the value:

```bash
awk '/^MemAvailable:/ { if ($2 < 33554432) { print "short: " $2 " kB"; exit 1 } }' /proc/meminfo
```

Then:

```bash
cat > /etc/sysctl.d/90-e2b.conf <<'EOF'
vm.nr_hugepages = 12288
vm.max_map_count = 1048576
net.ipv4.ip_forward = 1
EOF
sysctl -p /etc/sysctl.d/90-e2b.conf
grep HugePages_Total /proc/meminfo   # must print 12288
```

Use `sysctl -p` on this one file, never `sysctl --system` (that re-applies
every sysctl file on the shared host).

That is 12,288 × 2 MiB = **24 GiB** reserved for sandbox memory while forkd
still runs. U12 raises it to 24576 (48 GiB). If `HugePages_Total` is below
12288 (fragmented memory), run `echo 3 > /proc/sys/vm/drop_caches && sysctl
-w vm.compact_memory=1 && sysctl -w vm.nr_hugepages=12288`, then re-check.
If it is still short, STOP and report the number.

### 3. MSS clamp (same rule as E2B Embed)

```bash
iptables -w 5 -t mangle -C FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null \
  || iptables -w 5 -t mangle -I FORWARD 1 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
```

Persist it: add the same command as `ExecStartPre` in the unit (step 8).

### 4. Storage

```bash
zfs list forkdcache/e2b >/dev/null 2>&1 || zfs create -o mountpoint=/forkdcache/e2b forkdcache/e2b
mkdir -p /forkdcache/e2b/{orchestrator,storage/templates,storage/build-cache,tmp,registry}
chmod 700 /forkdcache/e2b/tmp
```

### 5. Artifacts (exact URLs and SHA-256s)

```bash
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
```

Any checksum mismatch is fatal: STOP. Do **not** fetch E2B's released envd
or orchestrator; ours come from U03.

### 6. Configuration files

Run `install -d -m 700 /etc/e2b` first. Write `/etc/e2b/orchestrator.env` with **exactly** the contents in
`01-architecture.md`, plus these lines from U03 P2:

```ini
SHUTDOWN_ADMISSION_GRACE=0s
SANDBOX_DRAIN_TIMEOUT=60s
TEMPLATE_MANAGER_SHUTDOWN_GRACE=0s
```

Write `/etc/e2b/flags.json` with exactly the contents in
`01-architecture.md`. Then:

```bash
chmod 600 /etc/e2b/orchestrator.env /etc/e2b/flags.json
install -d -m 700 /etc/spoond
test -s /etc/spoond/e2b-token-seed || openssl rand -hex 32 > /etc/spoond/e2b-token-seed
chmod 600 /etc/spoond/e2b-token-seed
```

### 7. Host firewall (nftables table `inet e2b_guard`, loaded by `e2b-guard.service`)

Why: every orchestrator listener binds `0.0.0.0`. The gRPC API is
unauthenticated.

**Never** edit `/etc/nftables.conf` and never enable, start, restart or
reload `nftables.service`. Debian's `/etc/nftables.conf` begins with
`flush ruleset`, which would wipe Docker's and forkd's rules. This table is
loaded by its own oneshot unit instead.

Write `/etc/nftables.d/e2b-guard.nft`. The first two lines make it
idempotent: they create the table if missing and then delete it, so each
load replaces the table instead of appending duplicate rules.

```nft
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
```

Write `/etc/systemd/system/e2b-guard.service`:

```ini
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
```

Load it and make it persistent:

```bash
install -d /etc/nftables.d
systemctl daemon-reload
systemctl enable --now e2b-guard.service
nft list table inet e2b_guard >/dev/null
```

Do not modify any existing table or rule (Docker's, forkd's).

### 7b. Record forkd's Firecracker processes (before the orchestrator starts)

```bash
pgrep -x firecracker | sort > /root/fc-pids-before-e2b
wc -l < /root/fc-pids-before-e2b
```

### 8. systemd unit `/etc/systemd/system/e2b-orchestrator.service`

```ini
[Unit]
Description=E2B orchestrator (spoond substrate)
After=network-online.target e2b-guard.service
Wants=network-online.target
Requires=e2b-guard.service

[Service]
Type=simple
EnvironmentFile=/etc/e2b/orchestrator.env
ExecStartPre=/bin/sh -c 'iptables -w 5 -t mangle -C FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null || iptables -w 5 -t mangle -I FORWARD 1 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu'
ExecStartPre=/bin/mkdir -p /forkdcache/e2b/tmp /fc-vm
ExecStart=/usr/local/lib/e2b/orchestrator
Restart=always
RestartSec=5
TimeoutStartSec=420
TimeoutStopSec=330
KillMode=mixed
Delegate=yes
LimitNOFILE=1048576
LimitMEMLOCK=infinity
TasksMax=infinity

[Install]
WantedBy=multi-user.target
```

`TimeoutStartSec=420` and `TimeoutStopSec=330` leave room for the drain
`ExecStartPost`/`ExecStop` that U10 adds. Then:

```bash
systemctl daemon-reload
systemctl enable --now e2b-orchestrator
sleep 10
curl -fsS http://127.0.0.1:5008/health
```

The body must be JSON with `"status":"healthy"` (A3 A6; the value is
lowercase). If not, collect `journalctl -u e2b-orchestrator -n 200` and STOP.

### 9. Local registry (docker)

```bash
docker inspect spoond-registry >/dev/null 2>&1 || docker run -d --name spoond-registry --restart=always \
  -p 127.0.0.1:5000:5000 \
  -v /forkdcache/e2b/registry:/var/lib/registry \
  registry:2.8.3
curl -fsS http://127.0.0.1:5000/v2/   # prints {}
```

### 10. Coexistence checks (must all pass)

```bash
systemctl is-active forkd-controller spoond-backend spoond-runner spoond-sshd-gateway e2b-guard e2b-orchestrator
pgrep -af 'forkd-controller' >/dev/null
nft list table inet e2b_guard >/dev/null
# P1 must not have touched forkd's VMs: the orchestrator's startup reclaim
# must report zero Firecracker processes reclaimed.
journalctl -u e2b-orchestrator --no-pager | grep 'startup resource reclaim completed'
# Informational: forkd PIDs that vanished since step 7b. CI churn makes some
# normal; they are a problem only if the reclaim log above shows any
# Firecracker reclaimed.
comm -23 /root/fc-pids-before-e2b <(pgrep -x firecracker | sort)
```

The forkd sandbox count changes constantly with CI, so it is not compared.
The authoritative check is the startup reclaim log line
`startup resource reclaim completed ...` (A3 P1, reclaim.go L127-L136). Its
`reclaimed` field is a map by resource. If `reclaimed` has a non-zero
`firecracker` entry,
stop the orchestrator (`systemctl stop e2b-orchestrator`) and STOP: P1 is not
effective. If the journal has no such line, also STOP and report the
journal.

### 11. Deploy notes in the repo

Add `deploy/e2b/` to the spoond repo with copies of:
- `orchestrator.env` (exact, from step 6);
- `flags.json`;
- `e2b-guard.nft`;
- `e2b-guard.service`;
- `e2b-orchestrator.service`;
- `host-setup.sh`, which contains steps 1–5, 7 and 9 exactly as written
  above (including the `MemAvailable` guard, `zfs list ||`, `test -s ||`,
  `docker inspect ||`, the `iptables -C ||` check and the idempotent
  `e2b-guard.nft`), with `set -euo pipefail`. Running it twice must change
  nothing the second time.

**Commit (spoond):** `feat(deploy): E2B orchestrator host setup for host`

## Done when

- `e2b-orchestrator` is active, and `/health` returns healthy.
- `HugePages_Total=12288`, `nbds_max=256`, `e2b-guard.service` is active and
  the `e2b_guard` table is loaded.
- The registry answers on `127.0.0.1:5000`.
- forkd and every spoond service are unaffected, and the startup reclaim
  reclaimed no Firecracker process.

## Do not

- Do not restart forkd or spoond services.
- Do not change Docker's or forkd's firewall rules.
- Do not edit `/etc/nftables.conf` or touch `nftables.service`.
- Do not run `sysctl --system`.
- Do not set `ENVIRONMENT=local`.
