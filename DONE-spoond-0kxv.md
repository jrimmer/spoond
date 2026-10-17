# DONE spoond-0kxv: 4.0 S3a, the host phase's system steps

The `host` phase's first five steps land in `install/host_system.go`,
registered from one init into the scaffold's `hostSteps` table: packages,
modules, udev, hugepages and iptables (the store, docker and apparmor
steps stay for S3b in `install/host_store.go`). Today's manual source is
`deploy/e2b/host-setup.sh` steps 1–2 and the iptables backend from the
lacy-infra runbook gaps list.

## packages

Each package is checked with `dpkg-query -W -f=${Status}` (kept when
`install ok installed`); the missing ones go through one
`apt-get install -y --no-install-recommends …` after `apt-get update` —
the update runs only when something is missing. The set: iptables
nftables rsync e2fsprogs iproute2 util-linux curl ca-certificates
docker.io docker-buildx containerd runc; with `--store-disk` plus
linux-headers-amd64 zfs-dkms zfsutils-linux zfs-zed. Because zfs-dkms
lives in Debian contrib, `apt-cache policy zfs-dkms` runs after the list
refresh and before the install; `Candidate: (none)` fails with the
"enable the contrib component in /etc/apt/sources.list.d/debian.sources"
fix and the install never starts.

## modules

`/etc/modules-load.d/e2b.conf` (nbd, tun, kvm) and
`/etc/modprobe.d/e2b-nbd.conf` (`options nbd nbds_max=256 max_part=16`)
land at 0644 through `env.writeFile`. The running state decides the
modprobes: `/sys/module/nbd/parameters/nbds_max` present with 256 means
both a kept row and no command, absent means `modprobe nbd …` (and tun,
whose `/sys/module/tun` marker works the same way). nbd loaded with a
smaller count — the common case after an earlier life of the host —
fails with "run `rmmod nbd` (no leases running) or reboot, then rerun".
In a dry run the modprobe is skipped and the row says what the real run
would load; the read-back that a real modprobe is followed by is skipped
with it.

## udev

`/etc/udev/rules.d/97-nbd-device.rules`, the one KERNEL=="nbd*" line
host-setup.sh ships, 0644; `udevadm control --reload` is deliberately
not run (the devices appear after the reboot the hugepages step asks
for, and systemd-udevd re-reads the rules directory per trigger).

## hugepages

`--hugepages N` is used as given; auto (the default) reads MemTotal and
keeps `max(6 GiB, 17 %)` for the host, so pages = (MemTotal − keep) / 2
MiB rounded down (16 GiB → 5120, 128 GiB → 54394). Below 3072 pages of
lease memory fails with "not enough memory for leases: N MiB" — the
check phase's 16 GiB floor and this floor agree.

The count lands in `/etc/sysctl.d/90-e2b.conf`
(vm.nr_hugepages, vm.max_map_count = 1048576, net.ipv4.ip_forward = 1),
applied with `sysctl -p` only when the file changed; and in
`/etc/default/grub`, where `setGrubHugepages` replaces an existing
hugepages= token inside the quoted GRUB_CMDLINE_LINUX_DEFAULT value and
keeps every other token and line (update-grub only when the body
changed). HugePages_Total is read back from /proc/meminfo: short of the
count, the step sets `st.RebootNeeded` and reports a skipped row — no
error, hostPhase returns ErrRebootNeeded after the later steps ran.

## iptables

`update-alternatives --query iptables` (and ip6tables) decides: Value
already the -legacy path is kept; otherwise
`update-alternatives --set <name> /usr/sbin/{ip6,}tables-legacy` runs
and the row notes what it was. A rerun is two queries.

## Tests

`install/host_system_test.go` drives each step directly with a fresh
Reporter and hostState over `t.TempDir()` and recordRunner; fakes stand
in for /proc/meminfo, /etc/default/grub and the /sys module markers, and
a small `kernelSim` Runner wrapper makes a successful modprobe leave the
markers and a successful update-alternatives --set flip the query
answer. Covered: the apt grouping and the contrib failure; the
nbds_max=16 failure text; the auto formula at 16/128/8 GiB and the
explicit count; grub token replace/append/keep with update-grub only on
change; each step's settled second pass reporting kept and running no
changing command; and a whole-table dry run writing no file and running
only the dpkg and alternatives queries.

Gates: `go test -count=1 ./install/... ./cmd/spoond/...`,
`go vet ./install/...`, `go build -tags noinstall ./cmd/spoond`.
