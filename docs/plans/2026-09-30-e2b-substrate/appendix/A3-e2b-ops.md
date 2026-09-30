# Appendix: E2B orchestrator — standalone deployment and patch-site facts

Source: `github.com/e2b-dev/infra` at HEAD `e473dd130015ca1ff8cf9300341e25034c38e178` (commit date 2026-09-30). Every code block is
verbatim from that commit, prefixed with the file's own line numbers (`NNNNN  code`). Prose outside
code blocks is analysis; where it states a behaviour it cites the block it comes from.

Conventions: "L12-L40" = line range at HEAD. Paths are repo-relative.

## Contents

- A1 Embed deployment scripts, compose service, version pins, artifact URLs
- A2 Orchestrator configuration (env vars, flags) with table
- A3 Build: Makefile, Go version, CGO, Dockerfile
- A4 Host directories and on-disk layout
- A5 Listening ports
- A6 Health endpoint and InfoService
- B  Patch sites P1-P6
- C  Snapshot lineage, storage layout, deletion, header format
- D  Networking (CIDRs, NETWORK_VERSION, default route, iptables, guest DNS)
- E  Gotchas collected while reading (things that bite a standalone Debian 13 deploy)

---

# A. Deployment facts

## A1. Embed compose scripts, service block and pins

The Embed package (`embed/compose`) runs the released orchestrator **binary on the host** (not in a
container): the `tools` image only carries the scripts; `orchestrator-launch.sh` `nsenter`s PID 1's
namespaces and execs `/var/lib/e2b/bin/orchestrator`. Order enforced by compose `depends_on`:
`preflight` -> `host-setup` -> `fetch-artifacts` -> `orchestrator` (plus redis, vector, clickhouse-migrator).
A standalone deploy reproduces these steps with systemd instead of compose.

### A1.1 preflight.sh

`embed/compose/scripts/preflight.sh` (full file, 106 lines):

````bash
    1  #!/usr/bin/env bash
    2  # preflight: verify the host can run the E2B orchestrator. Runs in PID 1's
    3  # namespaces (nsenter) from the compose service of the same name. Every
    4  # variable below is overridable so the checks are unit-testable.
    5  if [ -z "${PF_NO_MAIN:-}" ]; then set -euo pipefail; fi
    6  
    7  PF_SYS="${PF_SYS:-/sys}"
    8  PF_DEV="${PF_DEV:-/dev}"
    9  PF_UNAME_R="${PF_UNAME_R:-$(uname -r)}"
   10  PF_ARCH="${PF_ARCH:-$(uname -m)}"
   11  PF_PAGE_SIZE="${PF_PAGE_SIZE:-$(getconf PAGESIZE 2>/dev/null || echo 0)}"
   12  if [ -z "${PF_GLIBC:-}" ]; then
   13    pf_ldd="$(ldd --version 2>/dev/null || true)"
   14    PF_GLIBC="$(awk 'NR==1{print $NF}' <<<"$pf_ldd")"
   15  fi
   16  : "${PF_GLIBC:=0}"
   17  PF_FREE_GIB="${PF_FREE_GIB:-$(df -BG --output=avail / 2>/dev/null | tail -n1 | tr -dc '0-9' || echo 0)}"
   18  PF_MIN_FREE_GIB="${PF_MIN_FREE_GIB:-20}"
   19  PF_MODPROBE="${PF_MODPROBE:-modprobe}"
   20  PF_SKIP_DEVICES="${PF_SKIP_DEVICES:-}"
   21  # Binaries the orchestrator execs on the host (rsync and e2fsprogs for template
   22  # and sandbox rootfs work, iptables and ip for sandbox networking). Standard on
   23  # Ubuntu 24.04 server; a host without one fails a template build minutes later
   24  # with an opaque error, so it is checked here.
   25  PF_TOOLS="${PF_TOOLS:-iptables rsync mkfs.ext4 tune2fs e2fsck ip}"
   26  
   27  FAILURES=()
   28  fail() { FAILURES+=("$1"); }
   29  
   30  # version_ge A B: true when A >= B using sort -V
   31  version_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]; }
   32  
   33  check_arch() {
   34    case "$PF_ARCH" in
   35      x86_64 | aarch64) ;;
   36      *) fail "host architecture is $PF_ARCH. FIX: only x86_64 and aarch64 hosts are supported; the released orchestrator and envd have no $PF_ARCH build" ;;
   37    esac
   38  }
   39  # The orchestrator's memory snapshots and its 2 MiB hugepages assume 4 KiB
   40  # pages; an arm64 kernel built with 64 KiB pages (RHEL's choice) would reserve
   41  # 512 MiB hugepages and mis-map every snapshot.
   42  check_page_size() {
   43    [ "$PF_PAGE_SIZE" = 4096 ] ||
   44      fail "the kernel page size is $PF_PAGE_SIZE bytes, need 4096. FIX: boot a 4 KiB-page kernel (Ubuntu's default on x86_64 and aarch64); the orchestrator's snapshots and 2 MiB hugepages assume 4 KiB pages"
   45  }
   46  # arm64 restores sandboxes through userfaultfd write-protect, which arrived in 6.10.
   47  check_kernel() {
   48    local floor=6.8
   49    case "$PF_ARCH" in aarch64) floor=6.10 ;; esac
   50    version_ge "${PF_UNAME_R%%-*}" "$floor" ||
   51      fail "kernel $PF_UNAME_R is older than $floor. FIX: apt-get install linux-generic-hwe-24.04 on the host and reboot"
   52  }
   53  check_kvm() {
   54    [ -n "$PF_SKIP_DEVICES" ] || [ -c "$PF_DEV/kvm" ] ||
   55      fail "$PF_DEV/kvm is missing. FIX: $(kvm_fix)"
   56  }
   57  # The remedy differs per architecture: x86 has firmware switches and two
   58  # modules to load; arm64 KVM is built into the kernel and only exists when the
   59  # firmware booted it at EL2 or the hypervisor exposes nested virtualization.
   60  kvm_fix() {
   61    case "$PF_ARCH" in
   62      aarch64) echo "on bare metal check that the firmware boots the kernel at EL2 (dmesg | grep -i kvm); on a VM recreate it with nested virtualization enabled (Apple silicon: a Lima or other Virtualization.framework VM with nestedVirtualization on, M3 or newer, macOS 15 or newer)" ;;
   63      *) echo "on bare metal enable VT-x or AMD-V in firmware and modprobe kvm_intel or kvm_amd; on a VM recreate it with nested virtualization enabled (GCE: --enable-nested-virtualization)" ;;
   64    esac
   65  }
   66  check_tun() {
   67    [ -n "$PF_SKIP_DEVICES" ] || [ -c "$PF_DEV/net/tun" ] ||
   68      fail "$PF_DEV/net/tun is missing. FIX: modprobe tun on the host"
   69  }
   70  check_cgroup2() {
   71    [ -f "$PF_SYS/fs/cgroup/cgroup.controllers" ] ||
   72      fail "cgroup v2 is not mounted at /sys/fs/cgroup. FIX: boot the host with systemd.unified_cgroup_hierarchy=1"
   73  }
   74  check_glibc() {
   75    version_ge "$PF_GLIBC" 2.34 ||
   76      fail "glibc $PF_GLIBC is older than 2.34, the released orchestrator needs 2.34+. FIX: use an Ubuntu 24.04 host"
   77  }
   78  check_nbd() {
   79    "$PF_MODPROBE" -n nbd >/dev/null 2>&1 ||
   80      fail "the running kernel has no nbd module. FIX: apt-get install linux-modules-$PF_UNAME_R on the host and reboot"
   81  }
   82  check_tools() {
   83    local t missing=()
   84    # shellcheck disable=SC2086
   85    for t in $PF_TOOLS; do
   86      command -v "$t" >/dev/null 2>&1 || missing+=("$t")
   87    done
   88    [ "${#missing[@]}" -eq 0 ] ||
   89      fail "missing on the host: ${missing[*]}. FIX: apt-get install iptables rsync e2fsprogs iproute2 on the host (the orchestrator execs them there), then start the stack again"
   90  }
   91  check_disk() {
   92    [ "${PF_FREE_GIB:-0}" -ge "$PF_MIN_FREE_GIB" ] ||
   93      fail "only ${PF_FREE_GIB} GiB free on /, need ${PF_MIN_FREE_GIB}. FIX: free space on / or give the host a larger disk"
   94  }
   95  
   96  main() {
   97    check_arch; check_page_size; check_kernel; check_kvm; check_tun; check_cgroup2; check_glibc; check_nbd; check_tools; check_disk
   98    if [ "${#FAILURES[@]}" -gt 0 ]; then
   99      for f in "${FAILURES[@]}"; do echo "preflight: $f" >&2; done
  100      echo "FIX: resolve the ${#FAILURES[@]} item(s) above, then start the stack again" >&2
  101      exit 1
  102    fi
  103    echo "preflight: ok (arch=$PF_ARCH page=$PF_PAGE_SIZE kernel=$PF_UNAME_R glibc=$PF_GLIBC free=${PF_FREE_GIB}GiB)"
  104  }
  105  
  106  if [ -z "${PF_NO_MAIN:-}" ]; then main; fi
````

Notes for Debian 13 (trixie): kernel 6.12 >= 6.8 floor (L47-L52), glibc 2.41 >= 2.34 (L74-L77). `PF_TOOLS`
(L25): `iptables rsync mkfs.ext4 tune2fs e2fsck ip` -> Debian packages `iptables rsync e2fsprogs iproute2`.
`modprobe -n nbd` must succeed (Debian's stock kernel ships nbd as a module).

### A1.2 host-setup.sh

`embed/compose/scripts/host-setup.sh` (full file, 126 lines):

````bash
    1  #!/usr/bin/env bash
    2  # host-setup: kernel modules, sysctls, udev rule and directories the
    3  # orchestrator needs. Runs in PID 1's namespaces (nsenter) as root.
    4  # The `set` is guarded because tests source this file for its functions
    5  # (HS_NO_MAIN=1) and must not have the harness shell switched to -euo pipefail,
    6  # the same shape preflight.sh and fetch-artifacts.sh use.
    7  if [ -z "${HS_NO_MAIN:-}" ]; then set -euo pipefail; fi
    8  
    9  NBDS_MAX="${NBDS_MAX:-64}"
   10  NBD_MAX_PART="${NBD_MAX_PART:-16}"
   11  HUGEPAGES="${HUGEPAGES:-2048}"
   12  
   13  log() { echo "host-setup: $*"; }
   14  die() { echo "host-setup: $1" >&2; echo "FIX: $2" >&2; exit 1; }
   15  positive_int() {
   16    case "$2" in
   17      ''|*[!0-9]*|0) die "$1=$2 is not a positive integer" "set $1 to a whole number in .env or the environment, or leave it unset for the default" ;;
   18    esac
   19  }
   20  
   21  HS_IPTABLES="${HS_IPTABLES:-iptables}"
   22  
   23  # Ownership marker for the MSS clamp, written only when THIS script inserted
   24  # the rule. host-teardown removes the clamp only if the marker is there, so a
   25  # host that already had an identical TCPMSS rule keeps its own firewall
   26  # configuration instead of having it purged from under it. The marker lives
   27  # inside /var/lib/e2b, which host-setup creates and host-teardown removes
   28  # anyway, so the two scripts' path lists stay identical.
   29  HS_MSS_MARKER="${HS_MSS_MARKER:-/var/lib/e2b/mss-clamp.owned}"
   30  
   31  # Clamp TCP MSS on forwarded traffic. Cloud NICs commonly have an MTU below
   32  # 1500 (GCE: 1460) while the sandbox tap and veth links are 1500; without the
   33  # clamp downloads above a few MB inside sandboxes stall or arrive truncated
   34  # (seen on every GCE VM). Idempotent; harmless on hosts whose MTU is already 1500.
   35  MSS_RULE=(-p tcp --tcp-flags "SYN,RST" SYN -j TCPMSS --clamp-mss-to-pmtu)
   36  clamp_mss() {
   37    # dockerd rewrites iptables whenever any container starts or stops, so a
   38    # -C check can lose the xtables lock race; -w 5 waits for it instead of
   39    # racing, and the status is inspected rather than used as a bare
   40    # loop/if condition so a lock collision cannot be misread as "absent".
   41    local rc=0 err
   42    err="$("$HS_IPTABLES" -w 5 -t mangle -C FORWARD "${MSS_RULE[@]}" 2>&1 1>/dev/null)" || rc=$?
   43    case "$rc" in
   44    0) log "MSS clamp already present" ;;
   45    1)
   46      "$HS_IPTABLES" -w 5 -t mangle -I FORWARD 1 "${MSS_RULE[@]}" ||
   47        die "iptables -t mangle -I FORWARD 1 ... -j TCPMSS --clamp-mss-to-pmtu failed" "install iptables on the host (apt-get install iptables), then start the stack again"
   48      # Claim the rule only here, after an insert of our own succeeded: the
   49      # already-present branch above leaves the marker absent, which is how
   50      # host-teardown knows to leave that rule alone. The directory is created
   51      # here rather than relied on, because this runs before the install -d of
   52      # step 4 below and can be the first thing to touch /var/lib/e2b.
   53      install -d -m 0755 "$(dirname "$HS_MSS_MARKER")" ||
   54        die "cannot create $(dirname "$HS_MSS_MARKER")" "check permissions and free space on the host, then start the stack again"
   55      printf 'The TCPMSS clamp in mangle/FORWARD was inserted by host-setup; host-teardown removes it.\n' > "$HS_MSS_MARKER" ||
   56        die "cannot write $HS_MSS_MARKER" "check permissions and free space on the host, then start the stack again"
   57      log "MSS clamp added"
   58      ;;
   59    *)
   60      die "iptables -w 5 -t mangle -C FORWARD ... TCPMSS check failed with status $rc${err:+: $err}" "run iptables -t mangle -S FORWARD by hand; if the TCPMSS rule needs removing, run iptables -w 5 -t mangle -D FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu, then start the stack again"
   61      ;;
   62    esac
   63  }
   64  
   65  # Tests source this file with HS_NO_MAIN=1 to reach the functions above.
   66  # shellcheck disable=SC2317  # `return` only succeeds when sourced; `exit` is the executed-script fallback
   67  if [ -n "${HS_NO_MAIN:-}" ]; then return 0 2>/dev/null || exit 0; fi
   68  
   69  # The four /etc writes and the two /sys and /proc reads below each end in a
   70  # FIX line of their own. Without that, an immutable or read-only /etc (the
   71  # host shape this stack does not support) fails the
   72  # script with a bare `bash: ...: Read-only file system` and no remedy.
   73  ETC_FIX="check that /etc is writable on the host: an immutable or read-only root filesystem cannot run this stack, so use a host you can mutate, then start the stack again"
   74  
   75  positive_int NBDS_MAX "$NBDS_MAX"
   76  positive_int HUGEPAGES "$HUGEPAGES"
   77  
   78  # 1. Modules, persisted for reboots and loaded now.
   79  printf 'nbd\ntun\nkvm\n' > /etc/modules-load.d/e2b.conf ||
   80    die "cannot write /etc/modules-load.d/e2b.conf" "$ETC_FIX"
   81  echo "options nbd nbds_max=${NBDS_MAX} max_part=${NBD_MAX_PART}" > /etc/modprobe.d/e2b-nbd.conf ||
   82    die "cannot write /etc/modprobe.d/e2b-nbd.conf" "$ETC_FIX"
   83  modprobe nbd nbds_max="${NBDS_MAX}" max_part="${NBD_MAX_PART}" ||
   84    die "modprobe nbd nbds_max=${NBDS_MAX} max_part=${NBD_MAX_PART} failed" "load the module by hand and check dmesg on the host"
   85  modprobe tun || die "modprobe tun failed" "load the module by hand and check dmesg on the host"
   86  modprobe kvm || true
   87  have_nbds="$(cat /sys/module/nbd/parameters/nbds_max)" ||
   88    die "cannot read /sys/module/nbd/parameters/nbds_max although modprobe nbd succeeded" "check that the nbd module is loaded on the host (lsmod | grep nbd) and check dmesg, then start the stack again"
   89  [ "$have_nbds" -ge "$NBDS_MAX" ] ||
   90    die "nbd is loaded with nbds_max=$have_nbds, need $NBDS_MAX" "with the stack stopped, wait until ls /sys/block/nbd*/pid prints nothing (up to two minutes), run modprobe -r nbd on the host and start the stack again; or reboot so /etc/modprobe.d/e2b-nbd.conf takes effect"
   91  [ -b /dev/nbd0 ] || die "/dev/nbd0 is missing after modprobe" "check dmesg for nbd errors on the host"
   92  
   93  # 2. udev rule for the nbd devices (the same rule E2B's own Kubernetes node setup uses).
   94  cat > /etc/udev/rules.d/97-nbd-device.rules <<'EOF' ||
   95  KERNEL=="nbd*", GROUP="disk", MODE="0660"
   96  EOF
   97    die "cannot write /etc/udev/rules.d/97-nbd-device.rules" "$ETC_FIX"
   98  if command -v udevadm >/dev/null 2>&1; then { udevadm control --reload && udevadm trigger; } || true; fi
   99  
  100  # 3. sysctls, persisted and applied.
  101  cat > /etc/sysctl.d/90-e2b.conf <<EOF ||
  102  vm.nr_hugepages=${HUGEPAGES}
  103  net.ipv4.tcp_max_syn_backlog=65535
  104  vm.max_map_count=1048576
  105  EOF
  106    die "cannot write /etc/sysctl.d/90-e2b.conf" "$ETC_FIX"
  107  sysctl -q -p /etc/sysctl.d/90-e2b.conf ||
  108    die "sysctl -p /etc/sysctl.d/90-e2b.conf failed" "apply the file by hand and check dmesg on the host"
  109  have_hp="$(cat /proc/sys/vm/nr_hugepages)" ||
  110    die "cannot read /proc/sys/vm/nr_hugepages although sysctl -p succeeded" "check that /proc is mounted on the host, then start the stack again"
  111  [ "$have_hp" -ge "$HUGEPAGES" ] ||
  112    die "only $have_hp of $HUGEPAGES 2 MiB hugepages could be reserved" "give the host more memory (12 GiB recommended) or lower HUGEPAGES"
  113  
  114  # 3b. TCP MSS clamp for forwarded traffic, plus the ownership marker under
  115  #     /var/lib/e2b that lets host-teardown tell our rule from the host's own.
  116  clamp_mss
  117  
  118  # 4. Directories the orchestrator, template-manager and fetch-artifacts use.
  119  # host-teardown covers the same paths (it removes the parent directories and
  120  # /var/run/netns only when empty).
  121  install -d -m 0755 \
  122    /var/lib/e2b/bin /var/lib/e2b/storage/templates /var/lib/e2b/storage/build-cache \
  123    /fc-versions /fc-kernels /fc-busybox /fc-envd /fc-vm /orchestrator /var/run/netns ||
  124    die "install -d of the e2b directories failed" "check permissions and free space on the host, then rerun"
  125  
  126  log "ok (nbds_max=$have_nbds hugepages=$have_hp)"
````

Summary of host mutations: `/etc/modules-load.d/e2b.conf` (nbd, tun, kvm), `/etc/modprobe.d/e2b-nbd.conf`
(`options nbd nbds_max=64 max_part=16`), udev rule `/etc/udev/rules.d/97-nbd-device.rules`,
`/etc/sysctl.d/90-e2b.conf` (`vm.nr_hugepages=2048`, `net.ipv4.tcp_max_syn_backlog=65535`,
`vm.max_map_count=1048576`), a mangle/FORWARD TCPMSS clamp, and directories (L121-L123).
**It does not set `net.ipv4.ip_forward=1`** (see D; the Docker host it targets already has it on).

### A1.3 fetch-artifacts.sh

`embed/compose/scripts/fetch-artifacts.sh` (full file, 136 lines):

````bash
    1  #!/usr/bin/env bash
    2  # fetch-artifacts: download the released binaries into the host filesystem
    3  # (mounted at HOST_ROOT) and verify them. Runs unprivileged in the tools image.
    4  if [ -z "${FA_NO_MAIN:-}" ]; then set -euo pipefail; fi
    5  
    6  HOST_ROOT="${HOST_ROOT:-/host}"
    7  BUCKET="${BUCKET:-https://storage.googleapis.com/e2b-artifact-binaries}"
    8  FA_ARCH="${FA_ARCH:-$(uname -m)}"
    9  
   10  die() { echo "fetch-artifacts: $1" >&2; echo "FIX: $2" >&2; exit 1; }
   11  
   12  # Checksums pinned inside the tools image. envd, the kernel, Firecracker and
   13  # BusyBox are pinned by hand in .env, so a bump adds its row here first; a
   14  # version .env names is installable only once a tools image carrying that row
   15  # is published, or once its release has written the .sha256 the fallback
   16  # below reads. The orchestrator moves with the platform release instead
   17  # (.env carries a release marker on it), and a release cannot know the
   18  # checksum of a binary it has not built yet — its rows below cover the
   19  # versions pinned before that; newer ones are verified by the .sha256 the
   20  # release writes beside the binary (see sidecar_sha256). Superseded rows stay
   21  # for the rollback path.
   22  declare -gA SHA256=(
   23    ["orchestrator/v0.11.0/orchestrator"]="d43e9c6d0b64433e71d5442367dab5142bc868028c144d0439bc84634c116629"
   24    ["envd/v0.7.0/envd"]="9cda948e73383708c2a3fe2b323ee02483bc0f85d3c2dd8ffea4b1c60cfe35d5"
   25    ["firecrackers/v1.14-0.2.0/amd64/firecracker"]="ef22aec7cbffcf6cc44a8436a4db79f9e6fe5c52218c81af321dd20f10ad6e5d"
   26    ["kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin"]="9191ced12d24e6e381753a7ab12ec850877524d453eae75c20aeb176f3b5ad05"
   27    ["busybox/1.36.1/amd64/busybox"]="d7cce939adb09a41a22a5f846d22ba8d576b38dbb2b46a5c77a3a3e27ec52520"
   28    ["orchestrator/v0.15.0/orchestrator"]="b46e64241f830ceaedf91fccdf4598fcf818179ea156130def16946ce342ecc4"
   29    ["envd/v0.9.0/envd"]="c42a31d738718b5cf7654e258e5b111308646a905331b266294cdcbeb0a02355"
   30    ["envd/v0.9.202609130627-59497eb9134/envd"]="9b788bb48aef37afc317a09ff7a5b247ec365049333cf3144e2d3e4b29890612"
   31    # arm64. The three Firecracker artifacts are published for both
   32    # architectures already. The orchestrator and envd publish arm64 objects
   33    # (orchestrator/<version>/arm64/orchestrator, envd/<version>/arm64/envd)
   34    # with a .sha256 beside each; fetch() verifies those from the sidecar
   35    # when this table has no row. Rows here are only for versions that
   36    # predate the sidecar, or a rollback that still needs an in-image pin.
   37    ["firecrackers/v1.14-0.2.0/arm64/firecracker"]="66a8347a08741e47f850da1720cc6153a6998a2c9e87666958261afbcc9ba05e"
   38    ["kernels/vmlinux-6.1.177_5008931/arm64/vmlinux.bin"]="3e134b55a6e4feec481f6f3a2813d42860e28c273eeb91c02df0661322e61dab"
   39    ["busybox/1.36.1/arm64/busybox"]="eddab48ed02fe55034a3f9321c6d4f9db10d699bc92880bd334d94a5c55363e3"
   40  )
   41  
   42  # The checksum the release workflow published beside the binary, as
   43  # <key>.sha256 in sha256sum format. The bucket is create-only, so the binary
   44  # and its sidecar are written once, by the same run, and neither can change
   45  # afterwards. Returns 1 when the sidecar is missing or is not a sha256.
   46  sidecar_sha256() {
   47    local key="$1" line
   48    line="$(curl -fsSL --retry 3 "$BUCKET/$key.sha256" 2>/dev/null)" || return 1
   49    line="${line%%[[:space:]]*}"
   50    [[ "$line" =~ ^[0-9a-f]{64}$ ]] || return 1
   51    printf '%s\n' "$line"
   52  }
   53  
   54  goarch() {
   55    case "$1" in
   56      x86_64|amd64) echo amd64 ;;
   57      aarch64|arm64) echo arm64 ;;
   58      *) return 1 ;;
   59    esac
   60  }
   61  
   62  sha256_of() { sha256sum "$1" | awk '{print $1}'; }
   63  
   64  # fetch <bucket key> <destination path> <mode>
   65  fetch() {
   66    local key="$1" dest="$2" mode="$3" want got
   67    want="${SHA256[$key]:-}"
   68    if [ -z "$want" ]; then
   69      want="$(sidecar_sha256 "$key")" || {
   70        echo "fetch-artifacts: no sha256 pinned for $key and no $key.sha256 beside it" >&2
   71        echo "FIX: add the object's checksum to scripts/fetch-artifacts.sh, or publish the version through the release workflow, which writes the .sha256" >&2
   72        return 1
   73      }
   74    fi
   75    if [ -f "$dest" ] && [ "$(sha256_of "$dest")" = "$want" ]; then
   76      # The mode is re-applied even though the bytes are already right: an
   77      # artifact can be in place with the wrong bits (an earlier run interrupted
   78      # between mv and chmod, a file copied in by hand, a restored backup), and a
   79      # non-executable orchestrator, envd, firecracker or busybox fails much
   80      # later, as an exec error from a service that looks correctly installed.
   81      chmod "$mode" "$dest" ||
   82        die "cannot chmod $mode $dest" "check that the host filesystem mounted at $HOST_ROOT is writable, then rerun"
   83      echo "fetch-artifacts: $key already present"
   84      return 0
   85    fi
   86    # The three host-filesystem writes below each end in a FIX line of their own:
   87    # HOST_ROOT is a bind mount of the host's /, so a full or read-only host
   88    # would otherwise fail the one-shot with a bare mkdir/mv/chmod error.
   89    mkdir -p "$(dirname "$dest")" ||
   90      die "cannot create $(dirname "$dest")" "check that the host filesystem mounted at $HOST_ROOT is writable and has free space, then rerun"
   91    echo "fetch-artifacts: downloading $key"
   92    curl -fsSL --retry 3 -o "$dest.tmp" "$BUCKET/$key" || {
   93      rm -f "$dest.tmp"
   94      die "download of $key failed" "check network access to $BUCKET and the pinned versions in .env, then rerun"
   95    }
   96    got="$(sha256_of "$dest.tmp")"
   97    if [ "$got" != "$want" ]; then
   98      rm -f "$dest.tmp"
   99      echo "fetch-artifacts: sha256 mismatch for $key: got $got want $want" >&2
  100      echo "FIX: the version in .env and its checksum (scripts/fetch-artifacts.sh, or the published $key.sha256) disagree" >&2
  101      return 1
  102    fi
  103    mv "$dest.tmp" "$dest" || {
  104      rm -f "$dest.tmp"
  105      die "cannot move the verified $key into $dest" "check that the host filesystem mounted at $HOST_ROOT is writable and has free space, then rerun"
  106    }
  107    chmod "$mode" "$dest" ||
  108      die "cannot chmod $mode $dest" "check that the host filesystem mounted at $HOST_ROOT is writable, then rerun"
  109  }
  110  
  111  main() {
  112    local v
  113    for v in E2B_ORCHESTRATOR_VERSION E2B_ENVD_VERSION E2B_KERNEL_VERSION E2B_FIRECRACKER_VERSION E2B_BUSYBOX_VERSION; do
  114      [ -n "${!v:-}" ] || die "$v is not set" "set $v in .env (the pinned release versions) and rerun"
  115    done
  116    local ga
  117    ga="$(goarch "$FA_ARCH")" || {
  118      echo "fetch-artifacts: unsupported architecture $FA_ARCH" >&2
  119      echo "FIX: only x86_64 and aarch64 hosts are supported; the released orchestrator and envd have no $FA_ARCH build" >&2
  120      exit 1
  121    }
  122    # The orchestrator and envd objects carry no arch segment for amd64 (the key
  123    # every earlier release published under) and an arm64/ segment otherwise, the
  124    # layout the three Firecracker artifacts have always used. On the host both
  125    # land at arch-less paths: the orchestrator reads envd from /fc-envd/envd.
  126    local seg=""
  127    [ "$ga" = amd64 ] || seg="$ga/"
  128    fetch "orchestrator/${E2B_ORCHESTRATOR_VERSION}/${seg}orchestrator" "$HOST_ROOT/var/lib/e2b/bin/orchestrator" 0755 || return 1
  129    fetch "envd/${E2B_ENVD_VERSION}/${seg}envd" "$HOST_ROOT/fc-envd/envd" 0755 || return 1
  130    fetch "firecrackers/${E2B_FIRECRACKER_VERSION}/${ga}/firecracker" "$HOST_ROOT/fc-versions/${E2B_FIRECRACKER_VERSION}/${ga}/firecracker" 0755 || return 1
  131    fetch "kernels/${E2B_KERNEL_VERSION}/${ga}/vmlinux.bin" "$HOST_ROOT/fc-kernels/${E2B_KERNEL_VERSION}/${ga}/vmlinux.bin" 0644 || return 1
  132    fetch "busybox/${E2B_BUSYBOX_VERSION}/${ga}/busybox" "$HOST_ROOT/fc-busybox/${E2B_BUSYBOX_VERSION}/${ga}/busybox" 0755 || return 1
  133    echo "fetch-artifacts: ok"
  134  }
  135  
  136  if [ -z "${FA_NO_MAIN:-}" ]; then main; fi
````

### A1.4 orchestrator-launch.sh

`embed/compose/scripts/orchestrator-launch.sh` (full file, 89 lines):

````bash
    1  #!/usr/bin/env bash
    2  # orchestrator-launch: PID 1 of the compose service "orchestrator". Starts the
    3  # released orchestrator binary in PID 1's namespaces through nsenter and, on
    4  # SIGTERM, ends every sandbox before forwarding the signal. Firecracker
    5  # processes live in /sys/fs/cgroup/e2b/<sandbox>, outside compose's reach, and
    6  # the orchestrator itself only drains (waits) on SIGTERM; killing the cgroups
    7  # first is what makes `docker compose down`, `stop`, `restart` and a bare
    8  # `docker stop` leave no sandbox behind and lets the drain finish at once.
    9  # It sweeps again when the child dies on its own, so `restart: unless-stopped`
   10  # cannot start a fresh orchestrator beside a crashed one's sandboxes.
   11  # Compose pre_stop hooks were verified not to fire on v5.5.0.
   12  # Test hooks: OL_CGROUP_ROOT (fake cgroup tree), OL_COMMAND (child command line).
   13  set -euo pipefail
   14  
   15  OL_CGROUP_ROOT="${OL_CGROUP_ROOT:-/sys/fs/cgroup/e2b}"
   16  
   17  log() { echo "orchestrator-launch: $*"; }
   18  
   19  # shellcheck disable=SC2329  # invoked from the SIGTERM trap
   20  sweep() {
   21    local d ended=0 failed=0
   22    for d in "$OL_CGROUP_ROOT"/*/; do
   23      [ -d "$d" ] || continue
   24      if echo 1 > "$d/cgroup.kill" 2>/dev/null; then
   25        ended=$((ended + 1))
   26      else
   27        failed=$((failed + 1))
   28        log "warning: could not write $d/cgroup.kill"
   29      fi
   30    done
   31    log "sweep: ended $ended sandbox cgroup(s), $failed not writable"
   32  }
   33  
   34  if [ -n "${OL_COMMAND:-}" ]; then
   35    # Test hook: a command line with shell quoting, evaluated on purpose.
   36    eval "set -- $OL_COMMAND"
   37  else
   38    set -- nsenter -t 1 -m -u -i -n -C -w/ -- \
   39      /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
   40      /var/lib/e2b/bin/orchestrator
   41  fi
   42  
   43  # The trap is installed before the child starts so a SIGTERM arriving in the
   44  # gap between fork and `trap` cannot skip the sweep and fall back to bash's
   45  # default (immediate exit, sandboxes and child left running). `terminating`
   46  # distinguishes an operator-requested stop from the child dying on its own,
   47  # which decides below whether a non-zero exit gets a FIX line.
   48  terminating=""
   49  child=""
   50  
   51  # shellcheck disable=SC2329  # invoked from the SIGTERM trap
   52  # shellcheck disable=SC2317  # invoked only through the trap below; shellcheck 0.9 cannot see that
   53  on_term() {
   54    terminating=1
   55    log "SIGTERM: ending sandboxes, then the orchestrator (pid ${child:-unknown})"
   56    sweep
   57    if [ -n "$child" ]; then
   58      kill -TERM "$child" 2>/dev/null || true
   59    fi
   60  }
   61  trap on_term TERM INT
   62  
   63  "$@" &
   64  child=$!
   65  # Only claim success once the child is confirmed alive; a child that failed
   66  # to exec (e.g. a missing /var/lib/e2b/bin/orchestrator) must not be logged
   67  # as "started".
   68  if kill -0 "$child" 2>/dev/null; then
   69    log "started $1 (pid $child)"
   70  fi
   71  
   72  # `wait` returns early when a trapped signal arrives; loop until the child is gone.
   73  set +e
   74  wait "$child"; status=$?
   75  while kill -0 "$child" 2>/dev/null; do
   76    wait "$child"; status=$?
   77  done
   78  set -e
   79  log "orchestrator exited with status $status"
   80  if [ "$status" -ne 0 ] && [ -z "$terminating" ]; then
   81    # The child died on its own: a crash, or a failed exec. This is the one exit
   82    # path `restart: unless-stopped` reacts to, so sweep here as well. Without
   83    # it Compose starts a fresh orchestrator beside the Firecracker processes of
   84    # the dead one, which sit in host cgroups outside this container and which
   85    # nothing else would end. Logged before the FIX line, which stays last.
   86    sweep
   87    echo "FIX: read the orchestrator and api logs for the orchestrator's own error; if /var/lib/e2b/bin/orchestrator is missing, fetch-artifacts did not finish, so start the stack again (it runs before the orchestrator)" >&2
   88  fi
   89  exit "$status"
````

Why the launcher exists (its header, L2-L11): the orchestrator only *drains* on SIGTERM (waits for
sandboxes to end by themselves, see P2), and Firecracker lives in `/sys/fs/cgroup/e2b/<sandbox>`, so the
launcher writes `1` to every `cgroup.kill` under `/sys/fs/cgroup/e2b/*/` before forwarding SIGTERM, and
again if the binary dies on its own. A systemd unit should replicate this with `ExecStop=`/`ExecStopPost=`
(or rely on the P2 patch).

### A1.5 compose.yaml — one-shot host services and the orchestrator service

`embed/compose/compose.yaml` L173-L213:

````yaml
  173    preflight:
  174      <<: *tools
  175      restart: "no"
  176      network_mode: host
  177      pid: host
  178      privileged: true
  179      environment:
  180        PF_MIN_FREE_GIB: ${PF_MIN_FREE_GIB:-20}
  181      command: ["/bin/sh", "-c", "exec nsenter -t 1 -m -u -i -n -p -- /bin/bash -s < /opt/e2b/scripts/preflight.sh"]
  182  
  183    host-setup:
  184      <<: *tools
  185      restart: "no"
  186      network_mode: host
  187      pid: host
  188      privileged: true
  189      depends_on:
  190        preflight:
  191          condition: service_completed_successfully
  192      environment:
  193        HUGEPAGES: ${HUGEPAGES:-2048}
  194        NBDS_MAX: ${NBDS_MAX:-64}
  195      command: ["/bin/sh", "-c", "exec nsenter -t 1 -m -u -i -n -p -- /bin/bash -s < /opt/e2b/scripts/host-setup.sh"]
  196  
  197    fetch-artifacts:
  198      <<: *tools
  199      restart: "no"
  200      network_mode: host
  201      depends_on:
  202        host-setup:
  203          condition: service_completed_successfully
  204      environment:
  205        HOST_ROOT: /host
  206        E2B_ORCHESTRATOR_VERSION: ${E2B_ORCHESTRATOR_VERSION:?fetch the .env that ships beside compose.yaml (README, Install)}
  207        E2B_ENVD_VERSION: ${E2B_ENVD_VERSION:?fetch the .env that ships beside compose.yaml (README, Install)}
  208        E2B_KERNEL_VERSION: ${E2B_KERNEL_VERSION:?fetch the .env that ships beside compose.yaml (README, Install)}
  209        E2B_FIRECRACKER_VERSION: ${E2B_FIRECRACKER_VERSION:?fetch the .env that ships beside compose.yaml (README, Install)}
  210        E2B_BUSYBOX_VERSION: ${E2B_BUSYBOX_VERSION:?fetch the .env that ships beside compose.yaml (README, Install)}
  211      volumes:
  212        - /:/host
  213      command: ["/bin/bash", "/opt/e2b/scripts/fetch-artifacts.sh"]
````

`embed/compose/compose.yaml` L251-L311:

````yaml
  251    orchestrator:
  252      <<: *tools
  253      restart: unless-stopped
  254      network_mode: host
  255      pid: host
  256      cgroup: host
  257      privileged: true
  258      environment:
  259        ORCHESTRATOR_SERVICES: orchestrator,template-manager
  260        NODE_ID: orchestrator
  261        NODE_IP: 127.0.0.1
  262        ENVIRONMENT: local
  263        GRPC_PORT: "5008"
  264        PROXY_PORT: "5007"
  265        PPROF_PORT: "6061"
  266        REDIS_URL: 127.0.0.1:6379
  267        CLICKHOUSE_CONNECTION_STRING: clickhouse://clickhouse:clickhouse@127.0.0.1:9000/default
  268        LOGS_COLLECTOR_ADDRESS: http://127.0.0.1:30006
  269        # Where this service and api, client-proxy and dashboard-api export their
  270        # metrics, traces and logs over OTLP/gRPC. Empty unless .env sets
  271        # E2B_OTEL_COLLECTOR_GRPC_ENDPOINT, and empty exports nothing.
  272        OTEL_COLLECTOR_GRPC_ENDPOINT: ${E2B_OTEL_COLLECTOR_GRPC_ENDPOINT:-}
  273        TEMPLATE_STORAGE_URL: file:///var/lib/e2b/storage/templates
  274        BUILD_CACHE_STORAGE_URL: file:///var/lib/e2b/storage/build-cache
  275        ARTIFACTS_REGISTRY_PROVIDER: Local
  276        LOCAL_UPLOAD_BASE_URL: http://127.0.0.1:5008
  277        FIRECRACKER_VERSIONS_DIR: /fc-versions
  278        HOST_KERNELS_DIR: /fc-kernels
  279        HOST_BUSYBOX_DIR: /fc-busybox
  280        BUSYBOX_VERSION: ${E2B_BUSYBOX_VERSION:?fetch the .env that ships beside compose.yaml (README, Install)}
  281        HOST_ENVD_PATH: /fc-envd/envd
  282        ORCHESTRATOR_BASE_PATH: /orchestrator
  283        SANDBOX_DIR: /fc-vm
  284        # Warm-device buffer, kept below NBDS_MAX: at parity the refill loop spins and warns.
  285        NBD_POOL_SIZE: "32"
  286        NETWORK_VERSION: "1"
  287        DEFAULT_KERNEL_VERSION: ${E2B_KERNEL_VERSION:?fetch the .env that ships beside compose.yaml (README, Install)}
  288        DEFAULT_FIRECRACKER_VERSION: ${E2B_FIRECRACKER_VERSION:?fetch the .env that ships beside compose.yaml (README, Install)}
  289      depends_on:
  290        fetch-artifacts:
  291          condition: service_completed_successfully
  292        clickhouse-migrator:
  293          condition: service_completed_successfully
  294        redis:
  295          condition: service_healthy
  296        vector:
  297          condition: service_healthy
  298      # orchestrator-launch.sh is PID 1: it sweeps sandbox cgroups on SIGTERM,
  299      # because Compose pre_stop hooks do not fire and the orchestrator itself
  300      # only drains. It sweeps again when the binary dies on its own, so the
  301      # restart policy above cannot bring up a fresh orchestrator beside a
  302      # crashed one's Firecrackers. See scripts/orchestrator-launch.sh.
  303      command: ["/bin/bash", "/opt/e2b/scripts/orchestrator-launch.sh"]
  304      stop_grace_period: 60s
  305      healthcheck:
  306        test: ["CMD-SHELL", "curl -sf --max-time 2 http://127.0.0.1:5008/health"]
  307        interval: 10s
  308        timeout: 3s
  309        retries: 60
  310        start_period: 30s
  311  
````

Orchestrator service properties: `network_mode: host`, `pid: host`, `cgroup: host`, `privileged: true`,
no `ports:` (host networking), no volumes (runs in host mount ns via nsenter), `stop_grace_period: 60s`,
healthcheck `curl -sf --max-time 2 http://127.0.0.1:5008/health`. Note `ENVIRONMENT: local` (L262): this
has side effects beyond the shutdown sleep — see P2 and E.

### A1.6 Version pins (.env)

`embed/compose/.env` (full file, 42 lines):

````ini
    1  # Platform pins. Embed is released at the platform version once that release
    2  # is tagged, and each Embed release moves these lines to it: the trailing
    3  # marker is what the release tooling rewrites. Everything without a marker is
    4  # pinned by hand.
    5  E2B_API_IMAGE=us-docker.pkg.dev/e2b-artifacts/api/api:v0.14.202609170000-908833e4c12 # x-release-please-version
    6  E2B_DB_MIGRATOR_IMAGE=us-docker.pkg.dev/e2b-artifacts/api/db-migrator:v0.14.202609170000-908833e4c12 # x-release-please-version
    7  E2B_CLIENT_PROXY_IMAGE=us-docker.pkg.dev/e2b-artifacts/client-proxy/client-proxy:v0.3.202609130627-59497eb9134 # x-release-please-version
    8  E2B_CLICKHOUSE_MIGRATOR_IMAGE=us-docker.pkg.dev/e2b-artifacts/clickhouse-migrator/clickhouse-migrator:v0.4.202609130627-59497eb9134 # x-release-please-version
    9  # dashboard-api is a platform image and moves with the release like the api it
   10  # serves; it must come from the same source commit as the api and db-migrator.
   11  E2B_DASHBOARD_API_IMAGE=us-docker.pkg.dev/e2b-artifacts/dashboard-api/dashboard-api:v0.7.202609170000-908833e4c12 # x-release-please-version
   12  # The dashboard has its own release line (github.com/e2b-dev/dashboard) and is
   13  # pinned by hand.
   14  E2B_DASHBOARD_IMAGE=us-docker.pkg.dev/e2b-artifacts/dashboard/dashboard:v0.2.1
   15  E2B_ORCHESTRATOR_VERSION=v0.16.202609130627-59497eb9134 # x-release-please-version
   16  # envd has its own release line; the kernel, Firecracker and BusyBox are not
   17  # released here at all. Bumping one adds its checksum row to
   18  # scripts/fetch-artifacts.sh first.
   19  E2B_ENVD_VERSION=v0.9.202609130627-59497eb9134
   20  E2B_KERNEL_VERSION=vmlinux-6.1.177_5008931
   21  E2B_FIRECRACKER_VERSION=v1.14-0.2.0
   22  E2B_BUSYBOX_VERSION=1.36.1
   23  RUNTIME_COMMIT=7278c2a380767c9989da73cf4c04b1af1b32da18
   24  
   25  # Optional: pin the team API key the seed inserts, e2b_ plus at least 32 hex characters (openssl rand -hex 16 makes the hex part). Unset, the seed generates one on the first up and keeps it in the seed-state volume; docker compose logs ready prints it. Changing it later rotates the key on the next up.
   26  #TEAM_API_KEY=
   27  # Optional: pin the api's two secrets, 64 hex characters each (openssl rand -hex 32). Unset, the api-secrets one-shot generates both on the first up and keeps them in the seed-state volume; a value here wins for that variable only. Changing one later takes effect when the api is recreated.
   28  #ADMIN_TOKEN=
   29  #SANDBOX_ACCESS_TOKEN_HASH_SEED=
   30  # Optional: the address a browser uses to reach sandbox traffic through client-proxy (port 3002). Unset, the dashboard tells the browser http://localhost:3002, which is right when you open the dashboard on the machine itself or through an SSH tunnel. Opening it from another machine without a tunnel needs that machine's address here, then `up` again.
   31  #E2B_DASHBOARD_HOST=
   32  # Optional: the host:port of an OpenTelemetry collector that api, the orchestrator, client-proxy and dashboard-api send their metrics, traces and logs to over OTLP/gRPC (plaintext). Unset or empty, nothing is exported. 127.0.0.1:4317 is the built-in collector the next line starts; for a collector of your own, put its address here and leave the next line commented. Change either, then `up` again; to stop the built-in collector, comment both lines out and run `docker compose rm -sf otel-collector` before `up`.
   33  #E2B_OTEL_COLLECTOR_GRPC_ENDPOINT=127.0.0.1:4317
   34  # Optional: run the built-in collector (the otel-collector service), which keeps the e2b.* metrics in this stack's ClickHouse for the api's metrics endpoints and the dashboard's charts. Compose reads this variable itself.
   35  #COMPOSE_PROFILES=otel
   36  
   37  # The three stack images this package builds, published by the same release;
   38  # tests/pins.bats keeps them in step with kubernetes/kustomization.yaml and
   39  # docker-bake.hcl.
   40  E2B_TOOLS_IMAGE=us-docker.pkg.dev/e2b-artifacts/embed/tools:v0.3.202609120109-ad1cddd091b # x-release-please-version
   41  E2B_NODE_E2B_IMAGE=us-docker.pkg.dev/e2b-artifacts/embed/node-e2b:v0.3.202609120109-ad1cddd091b # x-release-please-version
   42  E2B_SEED_IMAGE=us-docker.pkg.dev/e2b-artifacts/embed/seed:v0.3.202609120109-ad1cddd091b # x-release-please-version
````

There are no separate "versions" files for firecracker/kernel/busybox/envd; `.env` is the single pin
source, and the SHA-256 table lives in `fetch-artifacts.sh` L22-L40.

### A1.7 Artifact URLs, destinations and checksums (amd64, from .env + fetch-artifacts.sh)

Base URL: `BUCKET=https://storage.googleapis.com/e2b-artifact-binaries` (fetch-artifacts.sh L7).
amd64 keys: orchestrator/envd have **no** arch segment; firecracker/kernel/busybox have `amd64/` (L126-L132).

| Artifact | URL | Host destination (mode) | SHA-256 |
|---|---|---|---|
| orchestrator v0.16.202609130627-59497eb9134 | `https://storage.googleapis.com/e2b-artifact-binaries/orchestrator/v0.16.202609130627-59497eb9134/orchestrator` | `/var/lib/e2b/bin/orchestrator` (0755) | not pinned in the table; read from sidecar `.../orchestrator.sha256` (L42-L52, L68-L74) |
| envd v0.9.202609130627-59497eb9134 | `https://storage.googleapis.com/e2b-artifact-binaries/envd/v0.9.202609130627-59497eb9134/envd` | `/fc-envd/envd` (0755) | `9b788bb48aef37afc317a09ff7a5b247ec365049333cf3144e2d3e4b29890612` (L30) |
| firecracker v1.14-0.2.0 | `https://storage.googleapis.com/e2b-artifact-binaries/firecrackers/v1.14-0.2.0/amd64/firecracker` | `/fc-versions/v1.14-0.2.0/amd64/firecracker` (0755) | `ef22aec7cbffcf6cc44a8436a4db79f9e6fe5c52218c81af321dd20f10ad6e5d` (L25) |
| kernel vmlinux-6.1.177_5008931 | `https://storage.googleapis.com/e2b-artifact-binaries/kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin` | `/fc-kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin` (0644) | `9191ced12d24e6e381753a7ab12ec850877524d453eae75c20aeb176f3b5ad05` (L26) |
| busybox 1.36.1 | `https://storage.googleapis.com/e2b-artifact-binaries/busybox/1.36.1/amd64/busybox` | `/fc-busybox/1.36.1/amd64/busybox` (0755) | `d7cce939adb09a41a22a5f846d22ba8d576b38dbb2b46a5c77a3a3e27ec52520` (L27) |

Sidecar format (L46-L52): `<key>.sha256` in `sha256sum` format; first whitespace-delimited token must
match `^[0-9a-f]{64}$`. Every key also has a sidecar published by the release workflow, so
`curl -fsSL "$BUCKET/<key>.sha256"` can double-check the table.

If building the orchestrator from source (P-series patches), the orchestrator row is replaced by the
locally built binary; the other four artifacts are still fetched. `.env` L23 pins
`RUNTIME_COMMIT=7278c2a380767c9989da73cf4c04b1af1b32da18` (the infra commit the released images come from),
which differs from the HEAD this appendix was extracted from.

**Version mismatch to be aware of:** the code default kernel is `vmlinux-6.1.158`
(`packages/shared/pkg/featureflags/flags.go` L913) and default FC is `v1.14-0.2.0` (L934-L939); the Embed
stack overrides the kernel through `DEFAULT_KERNEL_VERSION` (compose L287). A standalone deploy must set
`DEFAULT_KERNEL_VERSION=vmlinux-6.1.177_5008931` and `DEFAULT_FIRECRACKER_VERSION=v1.14-0.2.0`, or
template builds look for a kernel directory that does not exist.

---

## A2. Orchestrator configuration

### A2.1 cfg.Config and BuilderConfig (caarlos0/env tags)

`packages/orchestrator/pkg/cfg/model.go` L1-L207:

````go
    1  //go:build linux
    2  
    3  package cfg
    4  
    5  import (
    6  	"fmt"
    7  	"net"
    8  	"os"
    9  	"path/filepath"
   10  	"reflect"
   11  	"strconv"
   12  	"strings"
   13  
   14  	"github.com/caarlos0/env/v11"
   15  	"github.com/willscott/go-nfs"
   16  
   17  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
   18  	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
   19  )
   20  
   21  const DefaultBusyboxVersion = "1.36.1"
   22  
   23  type BuilderConfig struct {
   24  	DomainName             string `env:"DOMAIN_NAME"              envDefault:""`
   25  	FirecrackerVersionsDir string `env:"FIRECRACKER_VERSIONS_DIR" envDefault:"/fc-versions"`
   26  	BusyboxVersion         string `env:"BUSYBOX_VERSION"          envDefault:"1.36.1"`
   27  	HostBusyboxDir         string `env:"HOST_BUSYBOX_DIR"         envDefault:"/fc-busybox"`
   28  	HostEnvdPath           string `env:"HOST_ENVD_PATH"           envDefault:"/fc-envd/envd"`
   29  	HostKernelsDir         string `env:"HOST_KERNELS_DIR"         envDefault:"/fc-kernels"`
   30  	OrchestratorBaseDir    string `env:"ORCHESTRATOR_BASE_PATH"   envDefault:"/orchestrator"`
   31  	SandboxDir             string `env:"SANDBOX_DIR"              envDefault:"/fc-vm"`
   32  	SharedChunkCacheDir    string `env:"SHARED_CHUNK_CACHE_PATH"`
   33  	TemplatesDir           string `env:"TEMPLATES_DIR,expand"     envDefault:"${ORCHESTRATOR_BASE_PATH}/build-templates"`
   34  
   35  	DefaultCacheDir string `env:"DEFAULT_CACHE_DIR,expand" envDefault:"${ORCHESTRATOR_BASE_PATH}/build"`
   36  
   37  	Provider string `env:"PROVIDER" envDefault:"gcp"`
   38  
   39  	StorageConfig storage.Config
   40  	NetworkConfig network.Config
   41  }
   42  
   43  func makePathsAbsolute(c *BuilderConfig) error {
   44  	for _, item := range []*string{
   45  		&c.DefaultCacheDir,
   46  		&c.FirecrackerVersionsDir,
   47  		&c.HostBusyboxDir,
   48  		&c.HostEnvdPath,
   49  		&c.HostKernelsDir,
   50  		&c.OrchestratorBaseDir,
   51  		&c.StorageConfig.SandboxCacheDir,
   52  		&c.SandboxDir,
   53  		&c.SharedChunkCacheDir,
   54  		&c.StorageConfig.TemplateCacheDir,
   55  		&c.TemplatesDir,
   56  	} {
   57  		dir := *item
   58  
   59  		if dir == "" {
   60  			continue
   61  		}
   62  
   63  		if filepath.IsAbs(dir) {
   64  			continue
   65  		}
   66  
   67  		dir, err := filepath.Abs(dir)
   68  		if err != nil {
   69  			return fmt.Errorf("failed to resolve %q to absolute path: %w", *item, err)
   70  		}
   71  
   72  		*item = dir
   73  	}
   74  
   75  	return nil
   76  }
   77  
   78  type Config struct {
   79  	BuilderConfig
   80  
   81  	ClickhouseConnectionString  string            `env:"CLICKHOUSE_CONNECTION_STRING"`
   82  	ClickhouseConnectionStrings []string          `env:"CLICKHOUSE_CONNECTION_STRINGS" envSeparator:";"`
   83  	DisableStartupReclaim       bool              `env:"DISABLE_STARTUP_RECLAIM"`
   84  	ForceStop                   bool              `env:"FORCE_STOP"`
   85  	GRPCPort                    uint16            `env:"GRPC_PORT"                     envDefault:"5008"`
   86  	InstanceGroupName           string            `env:"INSTANCE_GROUP_NAME"`
   87  	LocalUploadBaseURL          string            `env:"LOCAL_UPLOAD_BASE_URL"`
   88  	NodeIP                      string            `env:"NODE_IP"                       envDefault:"localhost"`
   89  	NodeLabels                  []string          `env:"NODE_LABELS"                   envSeparator:","`
   90  	OrchestratorLockPath        string            `env:"ORCHESTRATOR_LOCK_PATH"        envDefault:"/orchestrator.lock"`
   91  	NFSProxyLogging             bool              `env:"NFS_PROXY_LOGGING"             envDefault:"false"`
   92  	NFSProxyTracing             bool              `env:"NFS_PROXY_TRACING"             envDefault:"false"`
   93  	NFSProxyMetrics             bool              `env:"NFS_PROXY_METRICS"             envDefault:"true"`
   94  	NFSProxyRecordHandleCalls   bool              `env:"NFS_PROXY_RECORD_HANDLE_CALLS" envDefault:"false"`
   95  	NFSProxyRecordStatCalls     bool              `env:"NFS_PROXY_RECORD_STAT_CALLS"   envDefault:"false"`
   96  	NFSProxyLogLevel            nfs.LogLevel      `env:"NFS_PROXY_LOG_LEVEL"           envDefault:"info"`
   97  	ProxyPort                   uint16            `env:"PROXY_PORT"                    envDefault:"5007"`
   98  	RedisClusterURL             string            `env:"REDIS_CLUSTER_URL"`
   99  	RedisTLSCABase64            string            `env:"REDIS_TLS_CA_BASE64"`
  100  	RedisTLSEnabled             bool              `env:"REDIS_TLS_ENABLED"`
  101  	RedisPassword               string            `env:"REDIS_PASSWORD"`
  102  	RedisURL                    string            `env:"REDIS_URL"`
  103  	RedisPoolSize               int               `env:"REDIS_POOL_SIZE"               envDefault:"5"`
  104  	RedisMinIdleConns           int               `env:"REDIS_MIN_IDLE_CONNS"          envDefault:"2"`
  105  	NBDPoolSize                 int               `env:"NBD_POOL_SIZE"                 envDefault:"64"`
  106  	Services                    []string          `env:"ORCHESTRATOR_SERVICES"         envDefault:"orchestrator"`
  107  	PersistentVolumeMounts      map[string]string `env:"PERSISTENT_VOLUME_MOUNTS"`
  108  }
  109  
  110  // AdditionalClickhouseEndpoints returns the non-blank entries from
  111  // CLICKHOUSE_CONNECTION_STRINGS that are *in addition to* the singular
  112  // CLICKHOUSE_CONNECTION_STRING. Order is preserved; first occurrence wins on
  113  // dedup. Returns nil endpoints if nothing remains.
  114  func (c Config) AdditionalClickhouseEndpoints() (endpoints, droppedDuplicates []string) {
  115  	singular := strings.TrimSpace(c.ClickhouseConnectionString)
  116  	seen := make(map[string]struct{}, len(c.ClickhouseConnectionStrings))
  117  	if singular != "" {
  118  		seen[singular] = struct{}{}
  119  	}
  120  
  121  	for _, raw := range c.ClickhouseConnectionStrings {
  122  		s := strings.TrimSpace(raw)
  123  		if s == "" {
  124  			continue
  125  		}
  126  		if _, dup := seen[s]; dup {
  127  			droppedDuplicates = append(droppedDuplicates, s)
  128  
  129  			continue
  130  		}
  131  		seen[s] = struct{}{}
  132  		endpoints = append(endpoints, s)
  133  	}
  134  
  135  	return endpoints, droppedDuplicates
  136  }
  137  
  138  func (c Config) NodeAddress() *string {
  139  	if c.NodeIP == "localhost" {
  140  		return nil
  141  	}
  142  
  143  	addr := net.JoinHostPort(c.NodeIP, strconv.FormatUint(uint64(c.GRPCPort), 10))
  144  
  145  	return &addr
  146  }
  147  
  148  func Parse() (Config, error) {
  149  	config, err := env.ParseAsWithOptions[Config](env.Options{
  150  		FuncMap: map[reflect.Type]env.ParserFunc{
  151  			reflect.TypeFor[nfs.LogLevel](): func(s string) (any, error) {
  152  				s = strings.ToLower(s)
  153  
  154  				return nfs.Log.ParseLevel(s)
  155  			},
  156  		},
  157  	})
  158  	if err != nil {
  159  		return config, err
  160  	}
  161  
  162  	bc := config.BuilderConfig
  163  	if err = makePathsAbsolute(&bc); err != nil {
  164  		return config, err
  165  	}
  166  
  167  	config.BuilderConfig = bc
  168  
  169  	if err = config.BuilderConfig.NetworkConfig.Validate(); err != nil {
  170  		return config, err
  171  	}
  172  
  173  	if config.PersistentVolumeMounts != nil {
  174  		for name, path := range config.PersistentVolumeMounts {
  175  			path = filepath.Clean(path)
  176  			path, err = filepath.Abs(path)
  177  			if err != nil {
  178  				return config, fmt.Errorf("failed to make persistent volume mount %q an absolute path: %w", name, err)
  179  			}
  180  
  181  			if _, err := os.Stat(path); err != nil {
  182  				return config, fmt.Errorf("failed to access persistent volume mount %q (%q): %w", name, path, err)
  183  			}
  184  
  185  			config.PersistentVolumeMounts[name] = path // store the cleaned path
  186  		}
  187  	}
  188  
  189  	return config, nil
  190  }
  191  
  192  func ParseBuilder() (BuilderConfig, error) {
  193  	model, err := env.ParseAs[BuilderConfig]()
  194  	if err != nil {
  195  		return BuilderConfig{}, err
  196  	}
  197  
  198  	if err = makePathsAbsolute(&model); err != nil {
  199  		return BuilderConfig{}, err
  200  	}
  201  
  202  	if err = model.NetworkConfig.Validate(); err != nil {
  203  		return BuilderConfig{}, err
  204  	}
  205  
  206  	return model, nil
  207  }
````

### A2.2 Nested structs parsed into BuilderConfig
`StorageConfig storage.Config` (model.go L39):

`packages/shared/pkg/storage/sandbox.go` L21-L26:

````go
   21  type Config struct {
   22  	CompressConfig
   23  
   24  	SandboxCacheDir  string `env:"SANDBOX_CACHE_DIR,expand"  envDefault:"${ORCHESTRATOR_BASE_PATH}/sandbox"`
   25  	TemplateCacheDir string `env:"TEMPLATE_CACHE_DIR,expand" envDefault:"${ORCHESTRATOR_BASE_PATH}/template"`
   26  }
````

`packages/shared/pkg/storage/compress_config.go` L26-L34:

````go
   26  type CompressConfig struct {
   27  	Enabled            bool   `env:"COMPRESS_ENABLED"              envDefault:"false"`
   28  	Type               string `env:"COMPRESS_TYPE"                 envDefault:""`
   29  	Level              int    `env:"COMPRESS_LEVEL"                envDefault:"0"`
   30  	FrameSizeKB        int    `env:"COMPRESS_FRAME_SIZE_KB"        envDefault:"0"`
   31  	MinPartSizeMB      int    `env:"COMPRESS_MIN_PART_SIZE_MB"     envDefault:"0"`
   32  	FrameEncodeWorkers int    `env:"COMPRESS_FRAME_ENCODE_WORKERS" envDefault:"0"`
   33  	EncoderConcurrency int    `env:"COMPRESS_ENCODER_CONCURRENCY"  envDefault:"0"`
   34  }
````
`NetworkConfig network.Config` (model.go L40):

`packages/orchestrator/pkg/sandbox/network/pool.go` L59-L160:

````go
   59  type ReleaseNotify func(ctx context.Context, ip string)
   60  
   61  type Config struct {
   62  	// Using reserver IPv4 in range that is used for experiments and documentation
   63  	// https://en.wikipedia.org/wiki/Reserved_IP_addresses
   64  	OrchestratorInSandboxIPAddress string `env:"SANDBOX_ORCHESTRATOR_IP" envDefault:"192.0.2.1"`
   65  
   66  	HyperloopProxyPort uint16 `env:"SANDBOX_HYPERLOOP_PROXY_PORT" envDefault:"5010"`
   67  	NFSProxyPort       uint16 `env:"SANDBOX_NFS_PROXY_PORT"       envDefault:"5011"`
   68  	PortmapperPort     uint16 `env:"SANDBOX_PORTMAPPER_PORT"      envDefault:"5012"`
   69  
   70  	// Comma-separated CIDRs to allow through the predefined firewall deny list.
   71  	// These are allowed before the private-range deny rules, so they can
   72  	// reach hosts in the 10.0.0.0/8, 172.16.0.0/12, etc. blocks.
   73  	// The exemption applies to every sandbox on the node. A wide prefix such as
   74  	// 0.0.0.0/0, a link-local prefix or 100.64.0.0/10 also opens cloud metadata
   75  	// endpoints such as 169.254.169.254.
   76  	AllowSandboxInternalCIDRs []string `env:"ALLOW_SANDBOX_INTERNAL_CIDRS" envDefault:"" envSeparator:","`
   77  
   78  	// TCP firewall ports - separate ports for different traffic types to avoid
   79  	// protocol detection blocking on server-first protocols like SSH.
   80  	// - HTTP port: for traffic destined to port 80 (HTTP Host header inspection)
   81  	// - TLS port: for traffic destined to port 443 (TLS SNI inspection)
   82  	// - Other port: for all other traffic (CIDR-only check, no protocol inspection)
   83  	SandboxTCPFirewallHTTPPort  uint16 `env:"SANDBOX_TCP_FIREWALL_HTTP_PORT"  envDefault:"5016"`
   84  	SandboxTCPFirewallTLSPort   uint16 `env:"SANDBOX_TCP_FIREWALL_TLS_PORT"   envDefault:"5017"`
   85  	SandboxTCPFirewallOtherPort uint16 `env:"SANDBOX_TCP_FIREWALL_OTHER_PORT" envDefault:"5018"`
   86  
   87  	// 0 disables; valid range 0..63 (DSCP is 6 bits). CS1=8 is the canonical Scavenger class (RFC 3662).
   88  	SandboxEgressDSCP uint8 `env:"SANDBOX_EGRESS_DSCP" envDefault:"0"`
   89  
   90  	// Egress DSCP for template builds. Nil (not 0) inherits SANDBOX_EGRESS_DSCP;
   91  	// an explicit 0 disables marking for builds only. Range 0..63.
   92  	// Set-but-empty behaves as unset (inherits): the env parser skips it.
   93  	BuildSandboxEgressDSCP *uint8 `env:"BUILD_SANDBOX_EGRESS_DSCP"`
   94  
   95  	// NetworkVersion selects v1 (iptables per-sandbox) or v2 (nftables, host sets).
   96  	NetworkVersion int `env:"NETWORK_VERSION" envDefault:"1"`
   97  }
   98  
   99  const maxDSCP = 63 // DSCP is the top 6 bits of the IPv4 TOS / IPv6 traffic-class byte.
  100  
  101  // EgressDSCP returns the DSCP class to stamp on egress for the given kind of
  102  // sandbox. 0 means "leave the field alone".
  103  func (c Config) EgressDSCP(class sandboxtypes.EgressClass) uint8 {
  104  	if class == sandboxtypes.EgressClassBuild && c.BuildSandboxEgressDSCP != nil {
  105  		return *c.BuildSandboxEgressDSCP
  106  	}
  107  
  108  	return c.SandboxEgressDSCP
  109  }
  110  
  111  // EgressTOS is the per-class IPv4 TOS / IPv6 traffic-class byte (DSCP in the
  112  // top 6 bits; 0 = leave the field alone): Config resolved to the form the
  113  // connection proxies consume.
  114  type EgressTOS struct {
  115  	Sandbox int
  116  	Build   int
  117  }
  118  
  119  // For picks the byte for the class.
  120  func (e EgressTOS) For(class sandboxtypes.EgressClass) int {
  121  	if class == sandboxtypes.EgressClassBuild {
  122  		return e.Build
  123  	}
  124  
  125  	return e.Sandbox
  126  }
  127  
  128  // EgressTOS resolves both configured DSCP classes into TOS bytes.
  129  func (c Config) EgressTOS() EgressTOS {
  130  	return EgressTOS{
  131  		Sandbox: int(c.EgressDSCP(sandboxtypes.EgressClassSandbox)) << 2,
  132  		Build:   int(c.EgressDSCP(sandboxtypes.EgressClassBuild)) << 2,
  133  	}
  134  }
  135  
  136  // untenantedDSCP is the class an idle pooled slot carries between tenants:
  137  // CreateNetwork seeds it and recycle restores it — the two must agree.
  138  func (c Config) untenantedDSCP() uint8 {
  139  	return c.EgressDSCP(sandboxtypes.EgressClassSandbox)
  140  }
  141  
  142  // DSCP builds the pointer BuildSandboxEgressDSCP takes: DSCP(0) is an
  143  // explicit "disable for builds", distinct from nil (inherit).
  144  func DSCP(v uint8) *uint8 { return new(v) }
  145  
  146  func (c Config) Validate() error {
  147  	if c.NetworkVersion != 1 && c.NetworkVersion != 2 {
  148  		return fmt.Errorf("NETWORK_VERSION=%d unsupported (must be 1 or 2)", c.NetworkVersion)
  149  	}
  150  
  151  	if c.SandboxEgressDSCP > maxDSCP {
  152  		return fmt.Errorf("SANDBOX_EGRESS_DSCP=%d out of range (0..%d)", c.SandboxEgressDSCP, maxDSCP)
  153  	}
  154  
  155  	if c.BuildSandboxEgressDSCP != nil && *c.BuildSandboxEgressDSCP > maxDSCP {
  156  		return fmt.Errorf("BUILD_SANDBOX_EGRESS_DSCP=%d out of range (0..%d)", *c.BuildSandboxEgressDSCP, maxDSCP)
  157  	}
  158  
  159  	return nil
  160  }
````

### A2.3 Storage URL resolution (read lazily, not part of cfg.Config)

`packages/orchestrator/pkg/cfg/storage.go` L24-L112:

````go
   24  // storageEnv is the environment surface for storage role resolution, parsed
   25  // lazily on each resolution because the CLI tools set these from flags after
   26  // startup. Empty values behave as unset.
   27  type storageEnv struct {
   28  	TemplateURL   string `env:"TEMPLATE_STORAGE_URL"`
   29  	BuildCacheURL string `env:"BUILD_CACHE_STORAGE_URL"`
   30  
   31  	// Legacy environment style, converted to storage URLs by legacyStorageURL.
   32  	Provider           storage.Provider `env:"STORAGE_PROVIDER"`
   33  	TemplateBucket     string           `env:"TEMPLATE_BUCKET_NAME"`
   34  	TemplateBasePath   string           `env:"LOCAL_TEMPLATE_STORAGE_BASE_PATH"`
   35  	BuildCacheBucket   string           `env:"BUILD_CACHE_BUCKET_NAME"`
   36  	BuildCacheBasePath string           `env:"LOCAL_BUILD_CACHE_STORAGE_BASE_PATH"`
   37  	// Parsed strictly: a malformed value fails resolution loudly (even for
   38  	// URL-configured roles) instead of being silently treated as false.
   39  	S3PathStyle bool `env:"S3_USE_PATH_STYLE"`
   40  }
   41  
   42  // TemplateStorage resolves the template storage destination.
   43  func TemplateStorage() (storage.Spec, error) {
   44  	e, err := env.ParseAs[storageEnv]()
   45  	if err != nil {
   46  		return storage.Spec{}, fmt.Errorf("parse storage environment: %w", err)
   47  	}
   48  
   49  	return resolveStorage(e, e.TemplateURL, e.TemplateBucket, e.TemplateBasePath,
   50  		"template", "TEMPLATE_BUCKET_NAME", "/tmp/templates")
   51  }
   52  
   53  // BuildCacheStorage resolves the build-cache storage destination.
   54  func BuildCacheStorage() (storage.Spec, error) {
   55  	e, err := env.ParseAs[storageEnv]()
   56  	if err != nil {
   57  		return storage.Spec{}, fmt.Errorf("parse storage environment: %w", err)
   58  	}
   59  
   60  	return resolveStorage(e, e.BuildCacheURL, e.BuildCacheBucket, e.BuildCacheBasePath,
   61  		"build cache", "BUILD_CACHE_BUCKET_NAME", "/tmp/build-cache")
   62  }
   63  
   64  func resolveStorage(e storageEnv, rawURL, bucket, basePath, name, bucketEnv, defaultBasePath string) (storage.Spec, error) {
   65  	if raw := strings.TrimSpace(rawURL); raw != "" {
   66  		return storage.ParseStorageURL(raw)
   67  	}
   68  
   69  	legacy, err := legacyStorageURL(e, bucket, basePath, name, bucketEnv, defaultBasePath)
   70  	if err != nil {
   71  		return storage.Spec{}, err
   72  	}
   73  
   74  	return storage.ParseStorageURL(legacy)
   75  }
   76  
   77  // legacyStorageURL converts the legacy env style into a storage URL. Delete
   78  // together with the legacy fields of storageEnv once the legacy envs are
   79  // retired.
   80  func legacyStorageURL(e storageEnv, bucket, basePath, name, bucketEnv, defaultBasePath string) (string, error) {
   81  	provider := cmp.Or(e.Provider, storage.DefaultStorageProvider)
   82  	switch provider {
   83  	case storage.LocalStorageProvider:
   84  		basePath = cmp.Or(basePath, defaultBasePath)
   85  		if strings.HasPrefix(basePath, "/") {
   86  			return (&url.URL{Scheme: "file", Path: basePath}).String(), nil
   87  		}
   88  
   89  		// The hierarchical file:// form cannot express a relative base
   90  		// path; use the opaque form.
   91  		return (&url.URL{Scheme: "file", Opaque: basePath}).String(), nil
   92  	case storage.GCPStorageProvider, storage.AWSStorageProvider, storage.AzureStorageProvider:
   93  		if bucket == "" {
   94  			return "", fmt.Errorf("%s storage bucket not configured: set %s", name, bucketEnv)
   95  		}
   96  
   97  		u := url.URL{Scheme: "gs", Host: bucket}
   98  		switch provider {
   99  		case storage.AWSStorageProvider:
  100  			u.Scheme = "s3"
  101  			if e.S3PathStyle {
  102  				u.RawQuery = url.Values{"s3ForcePathStyle": []string{"true"}}.Encode()
  103  			}
  104  		case storage.AzureStorageProvider:
  105  			u.Scheme = "azblob"
  106  		}
  107  
  108  		return u.String(), nil
  109  	default:
  110  		return "", fmt.Errorf("unknown storage provider: %s", e.Provider)
  111  	}
  112  }
````

### A2.4 Other env reads (outside cfg.Config)

`packages/shared/pkg/env/env.go` (full file, 52 lines):

````go
    1  package env
    2  
    3  import (
    4  	"os"
    5  	"strconv"
    6  
    7  	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
    8  )
    9  
   10  var environment = GetEnv("ENVIRONMENT", "prod")
   11  
   12  func IsLocal() bool {
   13  	return environment == "local"
   14  }
   15  
   16  func IsDevelopment() bool {
   17  	return environment == "dev" || environment == "local"
   18  }
   19  
   20  func IsDebug() bool {
   21  	return GetEnv("E2B_DEBUG", "false") == "true"
   22  }
   23  
   24  func GetEnv(key, defaultValue string) string {
   25  	value := os.Getenv(key)
   26  	if len(value) == 0 {
   27  		return defaultValue
   28  	}
   29  
   30  	return value
   31  }
   32  
   33  func GetEnvAsInt(key string, defaultValue int) (int, error) {
   34  	if v := os.Getenv(key); v != "" {
   35  		value, err := strconv.Atoi(v)
   36  		if err != nil {
   37  			return defaultValue, err
   38  		}
   39  
   40  		return value, nil
   41  	}
   42  
   43  	return defaultValue, nil
   44  }
   45  
   46  func GetNodeID() string {
   47  	return utils.RequiredEnv("NODE_ID", "Node ID of the instance node is required")
   48  }
   49  
   50  func LogsCollectorAddress() string {
   51  	return os.Getenv("LOGS_COLLECTOR_ADDRESS")
   52  }
````

`packages/shared/pkg/telemetry/pprof.go` L31-L56:

````go
   31  // DefaultPprofPort is the default port that we should use to mount pprof endpoint.
   32  const DefaultPprofPort = 6060
   33  
   34  // pprofPortEnv overrides DefaultPprofPort so concurrent local instances don't
   35  // collide on the hardcoded port.
   36  const pprofPortEnv = "PPROF_PORT"
   37  
   38  // PprofPort returns the port the pprof server should bind to: the value of
   39  // PPROF_PORT when set to a valid port, otherwise DefaultPprofPort.
   40  func PprofPort() int {
   41  	if raw, ok := os.LookupEnv(pprofPortEnv); ok {
   42  		if port, err := strconv.Atoi(raw); err == nil && port > 0 && port <= 65535 {
   43  			return port
   44  		}
   45  	}
   46  
   47  	return DefaultPprofPort
   48  }
   49  
   50  func NewPprofServer() *http.Server {
   51  	return &http.Server{
   52  		// We mount only to the localhost to prevent accidental exposure.
   53  		Addr:    fmt.Sprintf("127.0.0.1:%d", PprofPort()),
   54  		Handler: NewPprofMux(),
   55  	}
   56  }
````

`packages/shared/pkg/telemetry/config.go` L14-L18:

````go
   14  var otelCollectorGRPCEndpoint = os.Getenv("OTEL_COLLECTOR_GRPC_ENDPOINT")
   15  
   16  func OTELCollectorGRPCEndpoint() string {
   17  	return otelCollectorGRPCEndpoint
   18  }
````

`packages/shared/pkg/featureflags/client.go` L21-L26:

````go
   21  // launchDarklyOfflineStore is a test fixture that provides dynamically updatable feature flag state
   22  var launchDarklyOfflineStore = ldtestdata.DataSource()
   23  
   24  var launchDarklyApiKey = os.Getenv("LAUNCH_DARKLY_API_KEY")
   25  
   26  const waitForInit = 5 * time.Second
````

`packages/shared/pkg/utils/env.go` L21-L38:

````go
   21  // TargetArch returns the target architecture for binary paths and OCI platform.
   22  // If TARGET_ARCH is set, it is normalized to Go convention ("amd64" or "arm64");
   23  // otherwise defaults to the host architecture (runtime.GOARCH).
   24  func TargetArch() string {
   25  	if arch := os.Getenv("TARGET_ARCH"); arch != "" {
   26  		if normalized, ok := archAliases[arch]; ok {
   27  			return normalized
   28  		}
   29  
   30  		archWarningOnce.Do(func() {
   31  			fmt.Fprintf(os.Stderr, "WARNING: unrecognized TARGET_ARCH=%q, falling back to %s\n", arch, runtime.GOARCH)
   32  		})
   33  
   34  		return runtime.GOARCH
   35  	}
   36  
   37  	return runtime.GOARCH
   38  }
````

`packages/orchestrator/pkg/sandbox/network/slot.go` L28-L48:

````go
   28  const (
   29  	defaultHostNetworkCIDR = "10.11.0.0/16"
   30  	defaultVrtNetworkCIDR  = "10.12.0.0/16"
   31  
   32  	hostMask          = 32
   33  	vrtMask           = 31                  // 2 usable ips per block (vpeer and veth)
   34  	vrtAddressPerSlot = 1 << (32 - vrtMask) // vrt addresses per slot (vpeer and veth)
   35  
   36  	tapMask          = 30
   37  	tapInterfaceName = "tap0"
   38  	tapIp            = "169.254.0.22"
   39  	tapMAC           = "02:FC:00:00:00:05"
   40  	tapHostMAC       = "02:FC:00:00:00:06"
   41  )
   42  
   43  var (
   44  	hostNetworkCIDR     = getHostNetworkCIDR()
   45  	vrtNetworkCIDR      = getVrtNetworkCIDR()
   46  	vrtSlotsSize        = GetVrtSlotsSize()
   47  	tapHostHardwareAddr = getTapHostHardwareAddr()
   48  )
````

`packages/orchestrator/pkg/sandbox/network/slot.go` L492-L549:

````go
  492  func getHostNetworkCIDR() *net.IPNet {
  493  	cidr := env.GetEnv("SANDBOXES_HOST_NETWORK_CIDR", defaultHostNetworkCIDR)
  494  
  495  	_, subnet, err := net.ParseCIDR(cidr)
  496  	if err != nil {
  497  		log.Fatalf("Failed to parse network CIDR %s: %v", cidr, err)
  498  	}
  499  
  500  	log.Println("Using host network cidr", "cidr", cidr)
  501  
  502  	return subnet
  503  }
  504  
  505  // TapHostHardwareAddr returns the fixed host-side tap MAC.
  506  func TapHostHardwareAddr() net.HardwareAddr {
  507  	return slices.Clone(tapHostHardwareAddr)
  508  }
  509  
  510  // getTapHostHardwareAddr parses the fixed tapHostMAC constant once at package
  511  // init, so CreateNetwork doesn't reparse a value that can never change or
  512  // fail at runtime.
  513  func getTapHostHardwareAddr() net.HardwareAddr {
  514  	addr, err := net.ParseMAC(tapHostMAC)
  515  	if err != nil {
  516  		log.Fatalf("Failed to parse tap host MAC address %s: %v", tapHostMAC, err)
  517  	}
  518  
  519  	return addr
  520  }
  521  
  522  func getVrtNetworkCIDR() *net.IPNet {
  523  	cidr := env.GetEnv("SANDBOXES_VRT_NETWORK_CIDR", defaultVrtNetworkCIDR)
  524  
  525  	_, subnet, err := net.ParseCIDR(cidr)
  526  	if err != nil {
  527  		log.Fatalf("Failed to parse network CIDR %s: %v", cidr, err)
  528  	}
  529  
  530  	log.Printf("Using vrt network cidr %s", cidr)
  531  
  532  	return subnet
  533  }
  534  
  535  func GetVrtSlotsSize() int {
  536  	ones, _ := getVrtNetworkCIDR().Mask.Size()
  537  
  538  	// total IPs in the CIDR block
  539  	totalIPs := 1 << (32 - ones)
  540  
  541  	// total slots that we can allocate
  542  	// we need to divide total IPs by number of addresses per slot (vpeer and veth)
  543  	// then we subtract the number of addresses so it will not overflow, because we are adding them incrementally by slot index
  544  	totalSlots := (totalIPs / vrtAddressPerSlot) - vrtAddressPerSlot
  545  
  546  	log.Printf("Using network slot size: %d", totalSlots)
  547  
  548  	return totalSlots
  549  }
````

`packages/shared/pkg/artifacts-registry/registry.go` L12-L49:

````go
   12  type RegistryProvider string
   13  
   14  const (
   15  	GCPStorageProvider   RegistryProvider = "GCP_ARTIFACTS"
   16  	AWSStorageProvider   RegistryProvider = "AWS_ECR"
   17  	AzureStorageProvider RegistryProvider = "AZURE_ACR"
   18  	LocalStorageProvider RegistryProvider = "Local"
   19  
   20  	DefaultRegistryProvider RegistryProvider = GCPStorageProvider
   21  
   22  	storageProviderEnv = "ARTIFACTS_REGISTRY_PROVIDER"
   23  )
   24  
   25  var ErrImageNotExists = errors.New("image does not exist")
   26  
   27  type ArtifactsRegistry interface {
   28  	Delete(ctx context.Context, templateId string, buildId string) error
   29  }
   30  
   31  func GetArtifactsRegistryProvider(ctx context.Context) (ArtifactsRegistry, error) {
   32  	provider := RegistryProvider(env.GetEnv(storageProviderEnv, string(DefaultRegistryProvider)))
   33  
   34  	setupCtx, setupCtxCancel := context.WithTimeout(ctx, 10*time.Second)
   35  	defer setupCtxCancel()
   36  
   37  	switch provider {
   38  	case AWSStorageProvider:
   39  		return NewAWSArtifactsRegistry(setupCtx)
   40  	case GCPStorageProvider:
   41  		return NewGCPArtifactsRegistry(setupCtx)
   42  	case AzureStorageProvider:
   43  		return NewAzureArtifactsRegistry(setupCtx)
   44  	case LocalStorageProvider:
   45  		return NewLocalArtifactsRegistry()
   46  	}
   47  
   48  	return nil, fmt.Errorf("unknown artifacts registry provider: %s", provider)
   49  }
````

`packages/shared/pkg/dockerhub/repository.go` L15-L59:

````go
   15  type RemoteRepositoryProvider string
   16  
   17  const (
   18  	GCPStorageProvider   RemoteRepositoryProvider = "GCP_REMOTE_REPOSITORY"
   19  	AWSStorageProvider   RemoteRepositoryProvider = "AWS_ECR"
   20  	AzureStorageProvider RemoteRepositoryProvider = "AZURE_ACR"
   21  	LocalStorageProvider RemoteRepositoryProvider = "Local"
   22  
   23  	DefaultRegistryProvider RemoteRepositoryProvider = GCPStorageProvider
   24  
   25  	storageProviderEnv         = "DOCKERHUB_REMOTE_REPOSITORY_PROVIDER"
   26  	storageRemoteRepositoryURL = "DOCKERHUB_REMOTE_REPOSITORY_URL"
   27  
   28  	setupTimeout = 10 * time.Second
   29  )
   30  
   31  type RemoteRepository interface {
   32  	GetImage(ctx context.Context, tag string, platform containerregistry.Platform) (containerregistry.Image, error)
   33  	Close() error
   34  }
   35  
   36  func GetRemoteRepository(ctx context.Context) (RemoteRepository, error) {
   37  	provider := RemoteRepositoryProvider(env.GetEnv(storageProviderEnv, string(DefaultRegistryProvider)))
   38  
   39  	dockerRemoteRepositoryURL := env.GetEnv(storageRemoteRepositoryURL, "")
   40  	if dockerRemoteRepositoryURL == "" {
   41  		return NewNoopRemoteRepository(), nil
   42  	}
   43  
   44  	setupCtx, setupCtxCancel := context.WithTimeout(ctx, setupTimeout)
   45  	defer setupCtxCancel()
   46  
   47  	switch provider {
   48  	case AWSStorageProvider:
   49  		return NewAWSRemoteRepository(setupCtx, dockerRemoteRepositoryURL)
   50  	case GCPStorageProvider:
   51  		return NewGCPRemoteRepository(setupCtx, dockerRemoteRepositoryURL)
   52  	case AzureStorageProvider:
   53  		return NewAzureRemoteRepository(setupCtx, dockerRemoteRepositoryURL)
   54  	case LocalStorageProvider:
   55  		return NewNoopRemoteRepository(), nil
   56  	}
   57  
   58  	return nil, fmt.Errorf("unknown dockerhub remote repository provider: %s", provider)
   59  }
````

`packages/shared/pkg/featureflags/flags.go` L461-L475:

````go
  461  // envdTimeoutFallbackMs reads ENVD_TIMEOUT (Go duration string, e.g. "10s")
  462  // and returns milliseconds. Falls back to 10 000 ms when unset or unparseable.
  463  func envdTimeoutFallbackMs() int {
  464  	raw := os.Getenv("ENVD_TIMEOUT")
  465  	if raw == "" {
  466  		return 10_000
  467  	}
  468  
  469  	d, err := time.ParseDuration(raw)
  470  	if err != nil {
  471  		return 10_000
  472  	}
  473  
  474  	return int(d.Milliseconds())
  475  }
````

`packages/shared/pkg/consts/sandboxes.go` L17-L17:

````go
   17  var OrchestratorAPIPort = uint16(utils.Must(strconv.ParseUint(env.GetEnv("ORCHESTRATOR_PORT", "5008"), 10, 16)))
````

### A2.5 main.go (no CLI flags; only two test-only env hooks)

`packages/orchestrator/main.go` (full file, 68 lines):

````go
    1  //go:build linux
    2  
    3  package main
    4  
    5  import (
    6  	"context"
    7  	"os"
    8  
    9  	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"
   10  
   11  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/factories"
   12  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/tcpfirewall"
   13  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/version"
   14  	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
   15  )
   16  
   17  var commitSHA string
   18  
   19  func main() {
   20  	applyTestFlagOverrides()
   21  
   22  	factories.Run(factories.Options{
   23  		Version:       version.Version,
   24  		CommitSHA:     commitSHA,
   25  		EgressFactory: defaultEgressFactory,
   26  	})
   27  }
   28  
   29  func applyTestFlagOverrides() {
   30  	// "default" (the harness input default) means the flag's own fallback —
   31  	// dedup disabled — not an override that quietly enables it modeless.
   32  	if mode := os.Getenv("TESTS_MEMFILE_DIFF_DEDUP_MODE"); mode != "" && mode != "default" {
   33  		// direct_io also engages a representative fetch-defrag budget so the
   34  		// promotion/defrag path executes in CI; production tunes the numbers
   35  		// in the flag, the shape is what matters here.
   36  		defrag := 0
   37  		if mode == "direct_io" {
   38  			defrag = 1
   39  		}
   40  		featureflags.OverrideJSONFlag(featureflags.MemfileDiffDedupFlag, ldvalue.FromJSONMarshal(map[string]any{
   41  			"enabled":                        true,
   42  			"bestEffort":                     mode == "best_effort",
   43  			"directIO":                       mode == "direct_io",
   44  			"maxFetchWindowsPerBlock":        2 * defrag,
   45  			"maxPromotedParentPagesPerBlock": 64 * defrag,
   46  			"maxPagesPerPromotedFrame":       8 * defrag,
   47  		}))
   48  	}
   49  	if os.Getenv("TESTS_DISABLE_MEMFD") == "true" {
   50  		featureflags.OverrideBoolFlag(featureflags.UseMemFdFlag, false)
   51  	}
   52  }
   53  
   54  func defaultEgressFactory(_ context.Context, deps *factories.Deps) (*factories.EgressSetup, error) {
   55  	fw := tcpfirewall.New(
   56  		deps.Logger,
   57  		deps.Config.NetworkConfig,
   58  		deps.Sandboxes,
   59  		deps.MeterProvider,
   60  		deps.FeatureFlags,
   61  	)
   62  
   63  	return &factories.EgressSetup{
   64  		Proxy: fw,
   65  		Start: fw.Start,
   66  		Close: fw.Close,
   67  	}, nil
   68  }
````

There are **no command-line flags** on the orchestrator binary (`main.go` has no `flag` usage; `grep
flag\.` in `pkg/factories` finds none). "max-sandboxes-per-node" and "max-starting-instances-per-node"
are **feature flags**, not CLI flags (see P5). CLI flags exist only on the dev tools under `cmd/`
(e.g. `cmd/resume-build`).

### A2.6 Table: environment variables read by the orchestrator process

| Env var | Type | Default | Meaning / where |
|---|---|---|---|
| `NODE_ID` | string | **required** (panics if unset/blank) | Node id; `ServiceInfo.ClientId`, telemetry host id. `env.GetNodeID` -> `utils.RequiredEnv` |
| `ENVIRONMENT` | string | `prod` | `local` skips the 15 s shutdown sleep and template-manager 15 s grace; `local`/`dev` (IsDevelopment) disables the host lock and flips several flag fallbacks (see E) |
| `E2B_DEBUG` | bool-string | `false` | Debug logging |
| `ORCHESTRATOR_SERVICES` | []string (`,`) | `orchestrator` | `orchestrator`, `template-manager` (both for Embed) |
| `GRPC_PORT` | uint16 | `5008` | cmux listener (gRPC + HTTP `/health`, `/upload`) on `:PORT` |
| `PROXY_PORT` | uint16 | `5007` | Sandbox HTTP reverse proxy on `:PORT` |
| `PPROF_PORT` | int | `6060` | pprof on `127.0.0.1:PORT` |
| `NODE_IP` | string | `localhost` | Advertised node address (peer registry needs != localhost); routing records |
| `NODE_LABELS` | []string (`,`) | empty | Labels in ServiceInfo |
| `INSTANCE_GROUP_NAME` | string | empty | LD context only |
| `DOMAIN_NAME` | string | empty | LD deployment context |
| `PROVIDER` | string | `gcp` | BuilderConfig.Provider (template provisioning script branch) |
| `FIRECRACKER_VERSIONS_DIR` | path | `/fc-versions` | FC binaries `<dir>/<ver>/<arch>/firecracker` |
| `HOST_KERNELS_DIR` | path | `/fc-kernels` | `<dir>/<ver>/<arch>/vmlinux.bin` |
| `HOST_BUSYBOX_DIR` | path | `/fc-busybox` | `<dir>/<ver>/<goarch>/busybox` (template builds) |
| `BUSYBOX_VERSION` | string | `1.36.1` | busybox dir version |
| `HOST_ENVD_PATH` | path | `/fc-envd/envd` | envd baked into template builds |
| `ORCHESTRATOR_BASE_PATH` | path | `/orchestrator` | base for the four dirs below |
| `TEMPLATES_DIR` | path | `${ORCHESTRATOR_BASE_PATH}/build-templates` | template build workspace |
| `DEFAULT_CACHE_DIR` | path | `${ORCHESTRATOR_BASE_PATH}/build` | chunk cache; **wiped at startup** (template/cache.go) |
| `SANDBOX_CACHE_DIR` | path | `${ORCHESTRATOR_BASE_PATH}/sandbox` | per-sandbox rootfs overlays `rootfs-*-*.cow/.link` |
| `TEMPLATE_CACHE_DIR` | path | `${ORCHESTRATOR_BASE_PATH}/template` | per-build local cache (`<build>/cache/<uuid>/snapfile|metadata.json`) |
| `SHARED_CHUNK_CACHE_PATH` | path | empty | NFS shared chunk cache (off when empty) |
| `SANDBOX_DIR` | path | `/fc-vm` | tmpfs mount point inside each FC's private mount ns |
| `ORCHESTRATOR_LOCK_PATH` | path | `/orchestrator.lock` | flock single-instance guard (skipped when IsDevelopment) |
| `TEMPLATE_STORAGE_URL` | URL | unset -> legacy | e.g. `file:///var/lib/e2b/storage/templates`; authoritative |
| `BUILD_CACHE_STORAGE_URL` | URL | unset -> legacy | e.g. `file:///var/lib/e2b/storage/build-cache` |
| `STORAGE_PROVIDER` | enum | `storage.DefaultStorageProvider` | legacy; `Local` -> `LOCAL_*_BASE_PATH` |
| `LOCAL_TEMPLATE_STORAGE_BASE_PATH` | path | `/tmp/templates` | legacy local template storage |
| `LOCAL_BUILD_CACHE_STORAGE_BASE_PATH` | path | `/tmp/build-cache` | legacy local build cache |
| `TEMPLATE_BUCKET_NAME`, `BUILD_CACHE_BUCKET_NAME`, `S3_USE_PATH_STYLE` | | | cloud legacy |
| `LOCAL_UPLOAD_BASE_URL` | URL | `http://localhost:<GRPC_PORT>` | signed-URL base for `/upload` when build cache is local |
| `ARTIFACTS_REGISTRY_PROVIDER` | enum | `GCP_ARTIFACTS` | **set `Local`** when template-manager runs |
| `DOCKERHUB_REMOTE_REPOSITORY_PROVIDER` / `_URL` | | `GCP_REMOTE_REPOSITORY` / empty | empty URL -> noop |
| `REDIS_URL` / `REDIS_CLUSTER_URL` | string | empty | both empty -> Redis disabled (ErrRedisDisabled, tolerated) |
| `REDIS_PASSWORD`, `REDIS_TLS_ENABLED`, `REDIS_TLS_CA_BASE64`, `REDIS_POOL_SIZE`(5), `REDIS_MIN_IDLE_CONNS`(2) | | | Redis client |
| `CLICKHOUSE_CONNECTION_STRING` | DSN | empty | empty -> no ClickHouse (P6) |
| `CLICKHOUSE_CONNECTION_STRINGS` | []DSN (`;`) | empty | extra best-effort endpoints |
| `LOGS_COLLECTOR_ADDRESS` | URL | empty | sandbox log shipping (vector) |
| `OTEL_COLLECTOR_GRPC_ENDPOINT` | host:port | empty | empty -> no OTLP export |
| `LAUNCH_DARKLY_API_KEY` | string | empty | empty -> static offline flag store (P5) |
| `NBD_POOL_SIZE` | int | `64` | warm NBD devices; keep < `nbds_max` (compose uses 32 with nbds_max 64) |
| `DISABLE_STARTUP_RECLAIM` | bool | false | skips P1 reclaim entirely (and the v1 purge of the v2 nft table) |
| `FORCE_STOP` | bool | false | shutdown: skip sandbox drain, cancel close ctx (P2) |
| `PERSISTENT_VOLUME_MOUNTS` | map `name:path,...` | empty | enables NFS proxy + portmapper listeners |
| `NFS_PROXY_*` | | | NFS proxy logging/metrics |
| `NETWORK_VERSION` | int | `1` | 1 = iptables+per-netns nftables; 2 = host nftables |
| `SANDBOX_ORCHESTRATOR_IP` | IP | `192.0.2.1` | orchestrator address as seen from guests |
| `SANDBOX_HYPERLOOP_PROXY_PORT` | uint16 | `5010` | hyperloop HTTP server, `0.0.0.0` |
| `SANDBOX_NFS_PROXY_PORT` / `SANDBOX_PORTMAPPER_PORT` | uint16 | `5011` / `5012` | only with PERSISTENT_VOLUME_MOUNTS |
| `SANDBOX_TCP_FIREWALL_HTTP_PORT` / `_TLS_PORT` / `_OTHER_PORT` | uint16 | `5016` / `5017` / `5018` | egress TCP proxy, `0.0.0.0` |
| `ALLOW_SANDBOX_INTERNAL_CIDRS` | []CIDR (`,`) | empty | node-wide exemption from the private-range floor (v1 only, P4) |
| `SANDBOX_EGRESS_DSCP` / `BUILD_SANDBOX_EGRESS_DSCP` | uint8 | 0 / nil | DSCP marking |
| `SANDBOXES_HOST_NETWORK_CIDR` | CIDR | `10.11.0.0/16` | per-slot host IPs (/32) |
| `SANDBOXES_VRT_NETWORK_CIDR` | CIDR | `10.12.0.0/16` | veth/vpeer /31 pairs; also sets slot count |
| `DEFAULT_FIRECRACKER_VERSION` | string | `v1.14-0.2.0` | fallback of flag `build-firecracker-version` |
| `DEFAULT_KERNEL_VERSION` | string | `vmlinux-6.1.158` | fallback of flag `build-kernel-version` |
| `DEFAULT_ENVD_VERSION` | string | `promoted` | fallback of flag `build-envd-version` |
| `ENVD_UPGRADE_TARGET` / `ENVD_OFFLINE_UPGRADE_TARGET` | string | `off` | flag fallbacks |
| `ENVD_BINARY_CACHE` | bool | IsDevelopment | flag fallback |
| `ENVD_TIMEOUT` | Go duration | `10s` | fallback of `envd-timeout-milliseconds` |
| `TARGET_ARCH` | string | runtime.GOARCH | arch path segment |
| `ORCHESTRATOR_PORT` | uint16 | `5008` | shared const (used by peers/clients) |
| `TMPDIR` | path | `/tmp` | `os.TempDir()`: FC/UFFD sockets and metrics FIFOs live here (and are reclaimed there, P1) |
| `COMPRESS_*` | | off | storage compression config |
| `TESTS_MEMFILE_DIFF_DEDUP_MODE`, `TESTS_DISABLE_MEMFD` | | | test hooks in main.go |

---

## A3. Build

### A3.1 Makefile (build-local, STATIC)

`packages/orchestrator/Makefile` L1-L59:

````make
    1  ENV := $(shell cat ../../.last_used_env || echo "not-set")
    2  -include ../../.env.${ENV}
    3  
    4  AWS_BUCKET_PREFIX ?= $(PREFIX)$(AWS_ACCOUNT_ID)-
    5  GCP_BUCKET_PREFIX ?= $(GCP_PROJECT_ID)-
    6  CLEAN_NFS_CACHE_IMAGE_REGISTRY := $(GCP_REGION)-docker.pkg.dev/$(GCP_PROJECT_ID)/$(PREFIX)core/clean-nfs-cache
    7  
    8  # Public bucket holding released, prebuilt orchestrator-package binaries
    9  # (objects are <version>/orchestrator and <version>/clean-nfs-cache; there is
   10  # no separate template-manager object — it is the orchestrator binary
   11  # uploaded under a second name, matching upload/template-manager below).
   12  # Published to by the release-please workflow on tagging a release;
   13  # public-read, so downloads need no credentials.
   14  E2B_ARTIFACT_BINARIES_URL ?= https://storage.googleapis.com/e2b-artifact-binaries/orchestrator
   15  
   16  # Optional version "hook" for the released-artifact flow.
   17  # When empty (default), the build-and-upload/* targets build from source and
   18  # upload the binaries to the client's fc-env-pipeline bucket (the existing
   19  # flow, unchanged). When set (e.g. ORCHESTRATOR_VERSION=v0.1.0), skip
   20  # building: download the released binaries and upload those instead. The env
   21  # pipeline keeps fetching from the same bucket objects either way. One
   22  # version covers orchestrator, template-manager, and clean-nfs-cache — they
   23  # release together.
   24  #
   25  # Set it either on the command line (make ... ORCHESTRATOR_VERSION=v0.1.0) or
   26  # in the active .env.${ENV} file, which is included above. Quotes are
   27  # stripped so both `=v0.1.0` and `="v0.1.0"` work; a command-line value
   28  # always wins over the env file.
   29  ORCHESTRATOR_VERSION := $(strip $(subst ",,$(ORCHESTRATOR_VERSION)))
   30  ifneq ($(ORCHESTRATOR_VERSION),)
   31  ifeq ($(filter v%,$(ORCHESTRATOR_VERSION)),)
   32  $(error ORCHESTRATOR_VERSION must be a v-prefixed version (e.g. v0.15.0), not a commit SHA)
   33  endif
   34  endif
   35  
   36  HOSTNAME := $(shell hostname 2> /dev/null || hostnamectl hostname 2> /dev/null)
   37  $(if $(HOSTNAME),,$(error Failed to determine hostname: both 'hostname' and 'hostnamectl' failed))
   38  
   39  # Architecture for builds. Defaults to amd64; override for ARM64 builds.
   40  # (e.g., BUILD_ARCH=arm64 make build-local)
   41  BUILD_ARCH ?= amd64
   42  # Docker platform string. Override for cross-platform builds:
   43  #   BUILD_PLATFORM=linux/arm64 make build
   44  BUILD_PLATFORM ?= linux/$(BUILD_ARCH)
   45  
   46  # Busybox version — single source of truth for both Docker and local builds.
   47  BUSYBOX_VERSION ?= 1.36.1
   48  BUSYBOX_LOCAL := .busybox/$(BUSYBOX_VERSION)/$(BUILD_ARCH)/busybox
   49  
   50  VERSION_LDFLAGS :=
   51  ifneq ($(strip $(VERSION)),)
   52  VERSION_LDFLAGS := -X=github.com/e2b-dev/infra/packages/orchestrator/pkg/version.Version=$(VERSION)
   53  endif
   54  BUILD_TAGS :=
   55  STATIC_LDFLAGS :=
   56  ifeq ($(STATIC),1)
   57  BUILD_TAGS := -tags netgo,osusergo
   58  STATIC_LDFLAGS := -linkmode external -extldflags -static
   59  endif
````

`packages/orchestrator/Makefile` L79-L90:

````make
   79  # Download busybox from GCS public builds bucket.
   80  # Skips if binary exists and version/arch match the stamp file.
   81  .PHONY: fetch-busybox
   82  fetch-busybox:
   83  	@./scripts/fetch-busybox.sh "$(BUSYBOX_VERSION)" "$(BUILD_ARCH)" "$(BUSYBOX_LOCAL)"
   84  
   85  .PHONY: build-local
   86  build-local:
   87  	# Allow for passing commit sha directly for docker builds
   88  	$(eval COMMIT_SHA ?= $(shell git rev-parse --short HEAD))
   89  	CGO_ENABLED=1 GOOS=linux GOARCH=$(BUILD_ARCH) go build $(BUILD_TAGS) -o bin/orchestrator -ldflags "-X=main.commitSHA=$(COMMIT_SHA) $(VERSION_LDFLAGS) $(STATIC_LDFLAGS)" .
   90  	CGO_ENABLED=1 GOOS=linux GOARCH=$(BUILD_ARCH) go build $(BUILD_TAGS) -o bin/clean-nfs-cache -ldflags "-X=main.commitSHA=$(COMMIT_SHA) $(VERSION_LDFLAGS) $(STATIC_LDFLAGS)" ./cmd/clean-nfs-cache
````

`packages/orchestrator/Makefile` L133-L142:

````make
  133  define setup_local_env
  134  	$(eval include .env.local)
  135  	$(eval export $(shell sed 's/=.*//' .env.local))
  136  endef
  137  
  138  .PHONY: run-local
  139  run-local:
  140  	$(call setup_local_env)
  141  	mkdir -p ./.data/test-volume
  142  	NODE_ID=$(HOSTNAME) ./bin/orchestrator
````

`make build-local` = `CGO_ENABLED=1 GOOS=linux GOARCH=$(BUILD_ARCH) go build $(BUILD_TAGS) -o bin/orchestrator
-ldflags "-X=main.commitSHA=$(COMMIT_SHA) $(VERSION_LDFLAGS) $(STATIC_LDFLAGS)" .` run in
`packages/orchestrator`. `STATIC=1` adds `-tags netgo,osusergo` and `-linkmode external -extldflags -static`.
The Makefile needs a working `hostname` or `hostnamectl` (L36-L37) and optionally includes
`../../.env.${ENV}`. `BUILD_ARCH` defaults to `amd64`. `VERSION` sets `pkg/version.Version`.

### A3.2 go.mod (Go version, local replaces)

`packages/orchestrator/go.mod` L1-L12:

````text
    1  module github.com/e2b-dev/infra/packages/orchestrator
    2  
    3  go 1.26.8
    4  
    5  replace (
    6  	github.com/e2b-dev/infra/packages/clickhouse v0.0.0 => ../clickhouse
    7  	github.com/e2b-dev/infra/packages/shared v0.0.0 => ../shared
    8  )
    9  
   10  replace github.com/willscott/go-nfs v0.0.3 => github.com/e2b-dev/go-nfs v0.0.0-20260911175922-399c077f71ed
   11  
   12  tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
````

Go **1.26.8** (Debian 13 ships older Go; install the upstream toolchain or let `GOTOOLCHAIN=auto` fetch it).
The module needs the sibling directories `packages/shared` and `packages/clickhouse` (replace directives),
so build inside a full checkout.

### A3.3 Why CGO, and the headers it needs

`packages/orchestrator/pkg/sandbox/uffd/userfaultfd/fd.go` L1-L31:

````go
    1  //go:build linux
    2  
    3  package userfaultfd
    4  
    5  // https://docs.kernel.org/admin-guide/mm/userfaultfd.html
    6  // https://man7.org/linux/man-pages/man2/userfaultfd.2.html
    7  // https://github.com/torvalds/linux/blob/master/fs/userfaultfd.c
    8  // https://github.com/loopholelabs/userfaultfd-go/blob/main/pkg/constants/cgo.go
    9  
   10  /*
   11  #include <sys/syscall.h>
   12  #include <fcntl.h>
   13  #include <linux/userfaultfd.h>
   14  #include <sys/ioctl.h>
   15  
   16  struct uffd_pagefault {
   17  	__u64 flags;
   18  	__u64 address;
   19  	__u32 ptid;
   20  };
   21  
   22  #ifndef UFFD_FEATURE_WP_ASYNC
   23  #define UFFD_FEATURE_WP_ASYNC (1 << 15)
   24  #endif
   25  
   26  struct uffd_remove {
   27  	__u64 start;
   28  	__u64 end;
   29  };
   30  */
   31  import "C"
````

This is the only `import "C"` in orchestrator/shared/clickhouse. Build deps on Debian 13:
`gcc`, `libc6-dev`, `linux-libc-dev` (provides `linux/userfaultfd.h`; `UFFD_FEATURE_WP_ASYNC` is
back-filled at L22-L24 if the headers are old). A dynamically linked build on the target host needs no glibc
care; the Dockerfile's bookworm note (below) applies only when building elsewhere.

### A3.4 Dockerfile (base images)

`packages/orchestrator/Dockerfile` (full file, 68 lines):

````dockerfile
    1  ARG GOLANG_VERSION=1.26.8
    2  ARG DEBIAN_RUNTIME_TAG=bookworm-20260824-slim
    3  
    4  # The orchestrator binary is built with CGO and dynamically links against
    5  # glibc, so the build image's glibc must be <= the host's glibc (forward
    6  # compatibility only). The host runs Ubuntu 24.04 (glibc 2.39), so bookworm
    7  # (glibc 2.36) is safe; do NOT bump to trixie (glibc 2.41) without also
    8  # upgrading the host, or the raw binary will fail with "GLIBC_2.4x not found".
    9  ARG DEBIAN_VERSION=bookworm
   10  
   11  FROM golang:${GOLANG_VERSION}-${DEBIAN_VERSION} AS builder
   12  ARG TARGETARCH
   13  
   14  # Cached golang dependencies
   15  WORKDIR /build/shared
   16  COPY ./shared/go.mod ./shared/go.sum ./
   17  RUN --mount=type=cache,target=/go/pkg/mod go mod download
   18  
   19  WORKDIR /build/clickhouse
   20  COPY ./clickhouse/go.mod ./clickhouse/go.sum ./
   21  RUN --mount=type=cache,target=/go/pkg/mod go mod download
   22  
   23  WORKDIR /build/orchestrator
   24  COPY ./orchestrator/go.mod ./orchestrator/go.sum ./
   25  RUN --mount=type=cache,target=/go/pkg/mod go mod download
   26  
   27  # Copy source code
   28  WORKDIR /build
   29  
   30  COPY ./shared/pkg ./shared/pkg
   31  COPY ./clickhouse/pkg ./clickhouse/pkg
   32  
   33  COPY ./orchestrator/pkg ./orchestrator/pkg
   34  COPY ./orchestrator/cmd ./orchestrator/cmd
   35  COPY ./orchestrator/main.go ./orchestrator/main.go
   36  COPY ./orchestrator/Makefile ./orchestrator/Makefile
   37  
   38  WORKDIR /build/orchestrator
   39  
   40  ARG COMMIT_SHA
   41  ARG VERSION
   42  # Use Docker's TARGETARCH (set by --platform) so GOARCH matches the container platform.
   43  RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod make build-local COMMIT_SHA=${COMMIT_SHA} VERSION=${VERSION} BUILD_ARCH=${TARGETARCH}
   44  
   45  FROM scratch AS artifacts
   46  
   47  COPY --from=builder /build/orchestrator/bin/clean-nfs-cache .
   48  COPY --from=builder /build/orchestrator/bin/orchestrator .
   49  
   50  FROM debian:${DEBIAN_RUNTIME_TAG} AS runtime
   51  
   52  ARG COMMIT_SHA
   53  
   54  RUN apt-get update \
   55      && apt-get install -y --no-install-recommends \
   56          ca-certificates \
   57          bash \
   58          util-linux \
   59          iproute2 \
   60          iptables \
   61          rsync \
   62          e2fsprogs \
   63          systemd \
   64      && rm -rf /var/lib/apt/lists/*
   65  
   66  COPY --from=builder /build/orchestrator/bin/orchestrator /usr/local/bin/orchestrator
   67  
   68  ENTRYPOINT ["/usr/local/bin/orchestrator"]
````

Base images: builder `golang:1.26.8-bookworm`; runtime `debian:bookworm-20260824-slim` with
`ca-certificates bash util-linux iproute2 iptables rsync e2fsprogs systemd`. Those runtime packages are
also the host packages the orchestrator execs (it runs in the host namespaces). `util-linux` supplies
`unshare` and `mount` used in the Firecracker start script (A4/P1).

---

## A4. Host directories and on-disk layout

### A4.1 Firecracker and kernel paths (arch-prefixed, legacy fallback)

`packages/orchestrator/pkg/sandbox/fc/config.go` (full file, 85 lines):

````go
    1  //go:build linux
    2  
    3  package fc
    4  
    5  import (
    6  	"errors"
    7  	"os"
    8  	"path/filepath"
    9  
   10  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
   11  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/artifact"
   12  	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
   13  )
   14  
   15  const (
   16  	envsDisk     = "/mnt/disks/fc-envs/v1"
   17  	buildDirName = "builds"
   18  
   19  	rootfsDriveID = "rootfs"
   20  
   21  	entropyBytesSize    int64 = 1024 // 1 KB
   22  	entropyRefillTime   int64 = 100
   23  	entropyOneTimeBurst int64 = 0
   24  )
   25  
   26  type Config struct {
   27  	KernelVersion      string
   28  	FirecrackerVersion string
   29  }
   30  
   31  func (t Config) SandboxKernelDir() string {
   32  	return t.KernelVersion
   33  }
   34  
   35  func (t Config) HostKernelPath(config cfg.BuilderConfig) string {
   36  	// Prefer arch-prefixed path ({version}/{arch}/vmlinux.bin) for multi-arch support.
   37  	// Fall back to legacy flat path ({version}/vmlinux.bin) for existing production nodes.
   38  	archPath := filepath.Join(config.HostKernelsDir, t.KernelVersion, utils.TargetArch(), artifact.KernelFileName)
   39  	if _, err := os.Stat(archPath); err == nil {
   40  		return archPath
   41  	} else if !errors.Is(err, os.ErrNotExist) {
   42  		// Non-existence errors (e.g. permission denied) should not silently fall back
   43  		// to the legacy path, as that could use the wrong binary.
   44  		return archPath
   45  	}
   46  
   47  	return filepath.Join(config.HostKernelsDir, t.KernelVersion, artifact.KernelFileName)
   48  }
   49  
   50  func (t Config) FirecrackerPath(config cfg.BuilderConfig) string {
   51  	// Prefer arch-prefixed path ({version}/{arch}/firecracker) for multi-arch support.
   52  	// Fall back to legacy flat path ({version}/firecracker) for existing production nodes
   53  	// that haven't migrated to the arch-prefixed layout yet.
   54  	archPath, legacyPath := t.firecrackerPaths(config)
   55  	if _, err := os.Stat(archPath); err == nil {
   56  		return archPath
   57  	} else if !errors.Is(err, os.ErrNotExist) {
   58  		// Non-existence errors (e.g. permission denied) should not silently fall back
   59  		// to the legacy path, as that could use the wrong binary.
   60  		return archPath
   61  	}
   62  
   63  	return legacyPath
   64  }
   65  
   66  func (t Config) firecrackerPaths(config cfg.BuilderConfig) (archPath, legacyPath string) {
   67  	return filepath.Join(config.FirecrackerVersionsDir, t.FirecrackerVersion, utils.TargetArch(), artifact.FirecrackerBinaryName),
   68  		filepath.Join(config.FirecrackerVersionsDir, t.FirecrackerVersion, artifact.FirecrackerBinaryName)
   69  }
   70  
   71  type RootfsPaths struct {
   72  	TemplateVersion uint64
   73  	TemplateID      string
   74  	BuildID         string
   75  }
   76  
   77  var ConstantRootfsPaths = RootfsPaths{
   78  	// The version is always 2 for the constant rootfs paths format change.
   79  	TemplateVersion: 2,
   80  }
   81  
   82  // Deprecated: Use static rootfs path instead.
   83  func (t RootfsPaths) DeprecatedSandboxRootfsDir() string {
   84  	return filepath.Join(envsDisk, t.TemplateID, buildDirName, t.BuildID)
   85  }
````

`packages/orchestrator/pkg/sandbox/artifact/names.go` (full file, 7 lines):

````go
    1  package artifact
    2  
    3  const (
    4  	FirecrackerBinaryName = "firecracker"
    5  	KernelFileName        = "vmlinux.bin"
    6  	RootfsFileName        = "rootfs.ext4"
    7  )
````
Busybox (template builds) and envd:

`packages/orchestrator/pkg/template/build/core/rootfs/rootfs.go` L220-L232:

````go
  220  	envdFileData, err := os.ReadFile(buildContext.BuilderConfig.HostEnvdPath)
  221  	if err != nil {
  222  		return nil, fmt.Errorf("error reading envd file: %w", err)
  223  	}
  224  
  225  	busyboxPath := filepath.Join(buildContext.BuilderConfig.HostBusyboxDir, buildContext.BuilderConfig.BusyboxVersion, runtime.GOARCH, "busybox")
  226  	busyboxData, err := os.ReadFile(busyboxPath)
  227  	if err != nil {
  228  		return nil, fmt.Errorf("error reading busybox file %s: %w", busyboxPath, err)
  229  	}
  230  
  231  	filesMap := map[string]oci.File{
  232  		storage.GuestEnvdPath: {Bytes: envdFileData, Mode: 0o777},
````
Directories created at startup (0700, only if missing):

`packages/orchestrator/pkg/factories/run.go` L284-L331:

````go
  284  // Run starts the orchestrator, blocking until shutdown.
  285  // Returns true on clean shutdown.
  286  func Run(opts Options) bool {
  287  	config, err := cfg.Parse()
  288  	if err != nil {
  289  		log.Fatalf("failed to parse config: %v", err)
  290  	}
  291  
  292  	if err = ensureDirs(config); err != nil {
  293  		log.Fatalf("failed to create dirs: %v", err)
  294  	}
  295  
  296  	if opts.EgressFactory == nil {
  297  		log.Fatalf("EgressFactory must be set in Options")
  298  	}
  299  
  300  	success := run(config, opts)
  301  
  302  	log.Println("Stopping orchestrator, success:", success)
  303  
  304  	if !success {
  305  		os.Exit(1)
  306  	}
  307  
  308  	return success
  309  }
  310  
  311  func ensureDirs(c cfg.Config) error {
  312  	for _, dir := range []string{
  313  		c.DefaultCacheDir,
  314  		c.OrchestratorBaseDir,
  315  		c.StorageConfig.SandboxCacheDir,
  316  		c.SandboxDir,
  317  		c.SharedChunkCacheDir,
  318  		c.StorageConfig.TemplateCacheDir,
  319  		c.TemplatesDir,
  320  	} {
  321  		if dir == "" {
  322  			continue
  323  		}
  324  
  325  		if err := os.MkdirAll(dir, 0o700); err != nil {
  326  			return fmt.Errorf("failed to make %q: %w", dir, err)
  327  		}
  328  	}
  329  
  330  	return nil
  331  }
````
Per-sandbox files (sockets/FIFOs in `os.TempDir()`, overlays in `SANDBOX_CACHE_DIR`):

`packages/shared/pkg/storage/sandbox.go` L48-L92:

````go
   48  func (s *SandboxFiles) SandboxCacheRootfsPath(config Config) string {
   49  	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("rootfs-%s-%s.cow", s.SandboxID, s.randomID))
   50  }
   51  
   52  func (s *SandboxFiles) SandboxFirecrackerSocketPath() string {
   53  	return filepath.Join(s.tmpDir, fmt.Sprintf("fc-%s-%s.sock", s.SandboxID, s.randomID))
   54  }
   55  
   56  func (s *SandboxFiles) SandboxUffdSocketPath() string {
   57  	return filepath.Join(s.tmpDir, fmt.Sprintf("uffd-%s-%s.sock", s.SandboxID, s.randomID))
   58  }
   59  
   60  func (s *SandboxFiles) SandboxCacheRootfsLinkPath(config Config) string {
   61  	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("rootfs-%s-%s.link", s.SandboxID, s.randomID))
   62  }
   63  
   64  func (s *SandboxFiles) SandboxMetricsFifoPath() string {
   65  	return filepath.Join(s.tmpDir, fmt.Sprintf("fc-metrics-%s-%s.fifo", s.SandboxID, s.randomID))
   66  }
   67  
   68  func (s *SandboxFiles) SandboxCgroupName() string {
   69  	return fmt.Sprintf("sbx-%s-%s", s.SandboxID, s.randomID)
   70  }
   71  
   72  // SandboxFileGlobs returns glob patterns matching the on-disk files a sandbox
   73  // creates: firecracker/uffd sockets and the metrics fifo under tempDir, plus
   74  // the rootfs overlay and link files under sandboxCacheDir. The patterns mirror
   75  // the path builders above and are the single source of truth used by startup
   76  // reclaim. sandboxCacheDir may be empty, in which case the cache patterns are
   77  // omitted.
   78  func SandboxFileGlobs(tempDir, sandboxCacheDir string) []string {
   79  	patterns := []string{
   80  		filepath.Join(tempDir, "fc-*-*.sock"),
   81  		filepath.Join(tempDir, "uffd-*-*.sock"),
   82  		filepath.Join(tempDir, "fc-metrics-*-*.fifo"),
   83  	}
   84  	if sandboxCacheDir != "" {
   85  		patterns = append(patterns,
   86  			filepath.Join(sandboxCacheDir, "rootfs-*-*.cow"),
   87  			filepath.Join(sandboxCacheDir, "rootfs-*-*.link"),
   88  		)
   89  	}
   90  
   91  	return patterns
   92  }
````
Per-build template cache (under `TEMPLATE_CACHE_DIR`):

`packages/shared/pkg/storage/paths_cache.go` L42-L56:

````go
   42  func (c CachePaths) CacheSnapfile() string {
   43  	return filepath.Join(c.cacheDir(), SnapfileName)
   44  }
   45  
   46  func (c CachePaths) CacheMetadata() string {
   47  	return filepath.Join(c.cacheDir(), MetadataName)
   48  }
   49  
   50  func (c CachePaths) cacheDir() string {
   51  	return filepath.Join(c.config.TemplateCacheDir, c.BuildID, "cache", c.CacheIdentifier)
   52  }
   53  
   54  func (c CachePaths) Close() error {
   55  	return os.RemoveAll(c.cacheDir())
   56  }
````
Fixed paths: `NetNamespacesDir = "/var/run/netns"` (network/storage_local.go L41), cgroup root
`RootCgroupPath = "/sys/fs/cgroup/e2b"` (cgroup/manager.go L26):

`packages/orchestrator/pkg/sandbox/cgroup/manager.go` L21-L34:

````go
   21  const (
   22  	// standard kernel mount point for cgroups v2
   23  	cgroupV2MountPoint = "/sys/fs/cgroup"
   24  
   25  	// RootCgroupPath is the base path for all E2B sandbox cgroups
   26  	RootCgroupPath = cgroupV2MountPoint + "/e2b"
   27  
   28  	// NoCgroupFD is a sentinel value indicating that no cgroup file descriptor
   29  	// is available (e.g. cgroup accounting is disabled or the FD has been released).
   30  	NoCgroupFD = -1
   31  
   32  	cgroupKillTimeout      = 2 * time.Second
   33  	cgroupKillPollInterval = 100 * time.Millisecond
   34  )
````

### A4.2 Resulting layout (with the Embed values; the defaults are identical except storage)

| Path | Content | Source |
|---|---|---|
| `/var/lib/e2b/bin/orchestrator` | binary (Embed choice; anywhere is fine) | fetch-artifacts L128 |
| `/fc-versions/<fcver>/<arch>/firecracker` (legacy `/fc-versions/<fcver>/firecracker`) | Firecracker | fc/config.go L50-L69 |
| `/fc-kernels/<kver>/<arch>/vmlinux.bin` (legacy `/fc-kernels/<kver>/vmlinux.bin`) | guest kernel | fc/config.go L35-L48 |
| `/fc-busybox/<bbver>/<goarch>/busybox` | busybox for template builds | rootfs.go L225 |
| `/fc-envd/envd` | promoted envd (also `envd.<ver>` / `<ver>/envd` siblings for upgrades) | cfg HOST_ENVD_PATH; flags.go L1001-L1017 |
| `/fc-vm` | tmpfs mount point inside each FC mount ns (created with X-mount.mkdir) | script_builder.go L55-L63 |
| `/orchestrator/{build,build-templates,sandbox,template}` | caches/workspaces | cfg defaults |
| `/orchestrator.lock` | flock + PID | run.go L333-L386 |
| `/var/lib/e2b/storage/templates/<buildID>/...` | template + snapshot storage (`TEMPLATE_STORAGE_URL`) | C |
| `/var/lib/e2b/storage/build-cache/...` | build layer cache (`BUILD_CACHE_STORAGE_URL`) | |
| `/var/run/netns/ns-<idx>` | per-slot netns (v1) | slot.go L199-L201 |
| `/sys/fs/cgroup/e2b/sbx-<sandboxID>-<rand>` | per-sandbox cgroup | storage/sandbox.go L68-L70 |
| `$TMPDIR/fc-<sbx>-<rand>.sock`, `uffd-*.sock`, `fc-metrics-*.fifo` | per-sandbox control files | storage/sandbox.go L52-L66 |

`<arch>`/`<goarch>` is `amd64` or `arm64` (`utils.TargetArch()`), not `x86_64`.

---

## A5. Listening ports

| Listener | Bind | Port (env) | Code |
|---|---|---|---|
| cmux: gRPC (SandboxService, VolumeService, ChunkService, TemplateService if template-manager, InfoService, grpc.health) + HTTP/1 (`/health`, `/upload`) | `:5008` all interfaces | `GRPC_PORT` | run.go L984-L1095, cmux.go L21 |
| Sandbox reverse proxy (HTTP, host-header or `E2b-Sandbox-Id`/`E2b-Sandbox-Port` routing) | `:5007` all interfaces | `PROXY_PORT` | run.go L739; shared/pkg/proxy/proxy.go L66 |
| pprof | `127.0.0.1:6060` | `PPROF_PORT` | telemetry/pprof.go L50-L56 |
| Hyperloop (guest -> `192.0.2.1:80` is REDIRECTed here) | `0.0.0.0:5010` | `SANDBOX_HYPERLOOP_PROXY_PORT` | hyperloopserver/server.go L43 |
| TCP egress firewall proxy (HTTP / TLS / other) | `0.0.0.0:5016/5017/5018` | `SANDBOX_TCP_FIREWALL_*_PORT` | tcpfirewall/proxy.go L102-L104 |
| NFS proxy / portmapper | `:5011` / `:5012` | `SANDBOX_NFS_PROXY_PORT` / `SANDBOX_PORTMAPPER_PORT` | only when `PERSISTENT_VOLUME_MOUNTS` set (run.go L961-L967, L1158-L1211) |

All non-pprof listeners bind all interfaces; a host firewall should restrict 5007/5008 to trusted callers
and 5010-5018 to the sandbox veths (the REDIRECT targets are reached via the INPUT path from `veth-*`).

`packages/orchestrator/pkg/factories/cmux.go` L19-L39:

````go
   19  func NewCMUXServer(ctx context.Context, port uint16, meterProvider metric.MeterProvider) (cmux.CMux, error) {
   20  	var lisCfg net.ListenConfig
   21  	lis, err := lisCfg.Listen(ctx, "tcp", fmt.Sprintf(":%d", port))
   22  	if err != nil {
   23  		return nil, fmt.Errorf("failed to listen on port %d: %w", port, err)
   24  	}
   25  
   26  	m := cmux.New(lis)
   27  
   28  	meter := meterProvider.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/factories")
   29  	errCounter := utils.Must(telemetry.GetCounter(meter, telemetry.CmuxErrorsTotal))
   30  
   31  	m.HandleError(func(err error) bool {
   32  		logger.L().Warn(ctx, "cmux connection error", zap.Error(err))
   33  		errCounter.Add(ctx, 1)
   34  
   35  		return true // keep serving
   36  	})
   37  
   38  	return m, nil
   39  }
````

`packages/orchestrator/pkg/factories/run.go` L544-L557:

````go
  544  	var closers []closer
  545  
  546  	pprofServer := telemetry.NewPprofServer()
  547  	go func() {
  548  		logger.L().Info(ctx, "pprof server starting", zap.Int("port", telemetry.PprofPort()))
  549  
  550  		if err := pprofServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
  551  			logger.L().Error(ctx, "pprof server encountered error", zap.Error(err))
  552  		}
  553  	}()
  554  	// Closers run in reverse order; retain diagnostics through service teardown.
  555  	closers = append(closers, closer{"pprof server", func(ctx context.Context) error {
  556  		return closePprofServer(ctx, pprofServer)
  557  	}})
````

`packages/orchestrator/pkg/factories/run.go` L738-L751:

````go
  738  	// sandbox proxy
  739  	sandboxProxy, err := proxy.NewSandboxProxy(tel.MeterProvider, config.ProxyPort, sandboxes, featureFlags)
  740  	if err != nil {
  741  		logger.L().Fatal(ctx, "failed to create sandbox proxy", zap.Error(err))
  742  	}
  743  	startService("sandbox proxy", func() error {
  744  		err := sandboxProxy.Start(ctx)
  745  		if errors.Is(err, http.ErrServerClosed) {
  746  			return nil
  747  		}
  748  
  749  		return err
  750  	})
  751  	closers = append(closers, closer{"sandbox proxy", sandboxProxy.Close})
````

`packages/orchestrator/pkg/factories/run.go` L960-L1095:

````go
  960  	// nfs proxy server
  961  	if len(config.PersistentVolumeMounts) > 0 {
  962  		nfsClosers, err := startNFSProxy(ctx, config, builder, startService, sandboxes)
  963  		if err != nil {
  964  			logger.L().Fatal(ctx, "failed to start nfs proxy", zap.Error(err))
  965  		}
  966  		closers = append(closers, nfsClosers...)
  967  	}
  968  
  969  	// hyperloop server
  970  	hyperloopSrv, err := hyperloopserver.NewHyperloopServer(ctx, config.NetworkConfig.HyperloopProxyPort, globalLogger, sandboxes, featureFlags)
  971  	if err != nil {
  972  		logger.L().Fatal(ctx, "failed to create hyperloop server", zap.Error(err))
  973  	}
  974  	startService("hyperloop server", func() error {
  975  		err := hyperloopSrv.ListenAndServe()
  976  		if errors.Is(err, http.ErrServerClosed) {
  977  			return nil
  978  		}
  979  
  980  		return err
  981  	})
  982  	closers = append(closers, closer{"hyperloop server", hyperloopSrv.Shutdown})
  983  
  984  	grpcServer := e2bgrpc.NewGRPCServer(tel, e2bgrpc.WithSandboxResumeMetrics())
  985  	orchestrator.RegisterSandboxServiceServer(grpcServer, orchestratorService)
  986  	orchestrator.RegisterVolumeServiceServer(grpcServer, volumeService)
  987  	orchestrator.RegisterChunkServiceServer(grpcServer, orchestratorService)
  988  
  989  	// template manager
  990  	var tmpl *tmplserver.ServerStore
  991  	var localUploadHandler *localupload.Handler
  992  	if services.RunsTemplateManager() {
  993  		buildPersistence, uploadHandler, err := setupBuildStorage(ctx, limiter, config)
  994  		if err != nil {
  995  			logger.L().Fatal(ctx, "failed to setup build storage", zap.Error(err))
  996  		}
  997  
  998  		localUploadHandler = uploadHandler
  999  
 1000  		tmpl, err = tmplserver.New(
 1001  			ctx,
 1002  			config,
 1003  			serviceInfo,
 1004  			featureFlags,
 1005  			tel.MeterProvider,
 1006  			globalLogger,
 1007  			tmplSbxLoggerExternal,
 1008  			sandboxFactory,
 1009  			sandboxProxy,
 1010  			templateCache,
 1011  			persistence,
 1012  			buildPersistence,
 1013  			uploads,
 1014  		)
 1015  		if err != nil {
 1016  			logger.L().Fatal(ctx, "failed to create template manager", zap.Error(err))
 1017  		}
 1018  
 1019  		templatemanager.RegisterTemplateServiceServer(grpcServer, tmpl)
 1020  
 1021  		closers = append(closers, closer{"template server", tmpl.Close})
 1022  	}
 1023  
 1024  	infoService := service.NewInfoService(serviceInfo, sandboxes, hostMetrics)
 1025  	orchestratorinfo.RegisterInfoServiceServer(grpcServer, infoService)
 1026  
 1027  	grpcHealth := health.NewServer()
 1028  	grpc_health_v1.RegisterHealthServer(grpcServer, grpcHealth)
 1029  
 1030  	// cmux server, allows us to reuse the same TCP port between grpc and HTTP requests
 1031  	cmuxServer, err := NewCMUXServer(ctx, config.GRPCPort, tel.MeterProvider)
 1032  	if err != nil {
 1033  		logger.L().Fatal(ctx, "failed to create cmux server", zap.Error(err))
 1034  	}
 1035  
 1036  	// Create all matchers BEFORE starting Serve() to avoid data race.
 1037  	// cmux.Match() modifies internal state that Serve() reads from.
 1038  	httpListener := cmuxServer.Match(cmux.HTTP1Fast())
 1039  	grpcListener := cmuxServer.Match(cmux.Any()) // the rest are GRPC requests
 1040  
 1041  	startService("cmux server", func() error {
 1042  		logger.L().Info(ctx, "Starting network server", zap.Uint16("port", config.GRPCPort))
 1043  		err := cmuxServer.Serve()
 1044  		if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
 1045  			return nil
 1046  		}
 1047  
 1048  		return err
 1049  	})
 1050  	closers = append(closers, closer{"cmux server", func(context.Context) error {
 1051  		logger.L().Info(ctx, "Shutting down cmux server")
 1052  		cmuxServer.Close()
 1053  
 1054  		return nil
 1055  	}})
 1056  
 1057  	// http server
 1058  	healthcheck, err := e2bhealthcheck.NewHealthcheck(serviceInfo)
 1059  	if err != nil {
 1060  		logger.L().Fatal(ctx, "failed to create healthcheck", zap.Error(err))
 1061  	}
 1062  
 1063  	httpMux := http.NewServeMux()
 1064  	httpMux.Handle("/health", healthcheck.CreateHandler())
 1065  
 1066  	if localUploadHandler != nil {
 1067  		httpMux.Handle("/upload", localUploadHandler)
 1068  	}
 1069  
 1070  	httpServer := NewHTTPServer()
 1071  	httpServer.Handler = httpMux
 1072  
 1073  	startService("http server", func() error {
 1074  		err := httpServer.Serve(httpListener)
 1075  		switch {
 1076  		case errors.Is(err, cmux.ErrServerClosed):
 1077  			return nil
 1078  		case errors.Is(err, http.ErrServerClosed):
 1079  			return nil
 1080  		default:
 1081  			return err
 1082  		}
 1083  	})
 1084  	closers = append(closers, closer{"http server", httpServer.Shutdown})
 1085  
 1086  	// grpc server
 1087  	startService("grpc server", func() error {
 1088  		return grpcServer.Serve(grpcListener)
 1089  	})
 1090  	closers = append(closers, closer{"grpc server", func(context.Context) error {
 1091  		logger.L().Info(ctx, "Shutting down grpc server")
 1092  		grpcServer.GracefulStop()
 1093  
 1094  		return nil
 1095  	}})
````

`packages/orchestrator/pkg/hyperloopserver/server.go` L41-L46:

````go
   41  	server := &http.Server{
   42  		Handler: engine,
   43  		Addr:    fmt.Sprintf("0.0.0.0:%d", port),
   44  
   45  		BaseContext: func(net.Listener) context.Context { return ctx },
   46  	}
````

`packages/orchestrator/pkg/tcpfirewall/proxy.go` L97-L125:

````go
   97  	// Three separate addresses for different traffic types.
   98  	// iptables redirects traffic based on original destination port:
   99  	// - dport 80 → httpAddr (HTTP Host header inspection)
  100  	// - dport 443 → tlsAddr (TLS SNI inspection)
  101  	// - other dports → otherAddr (CIDR-only, no protocol inspection)
  102  	httpAddr := fmt.Sprintf("0.0.0.0:%d", p.httpPort)
  103  	tlsAddr := fmt.Sprintf("0.0.0.0:%d", p.tlsPort)
  104  	otherAddr := fmt.Sprintf("0.0.0.0:%d", p.otherPort)
  105  
  106  	deps := proxyDeps{
  107  		metrics:      p.metrics,
  108  		limiter:      p.limiter,
  109  		logger:       p.logger,
  110  		sandboxes:    p.sandboxes,
  111  		featureFlags: p.featureFlags,
  112  		egressTOS:    p.egressTOS,
  113  	}
  114  
  115  	// HTTP listener (port 80 traffic): inspect Host header for domain allowlist
  116  	p.proxy.AddHTTPHostMatchRoute(httpAddr, func(_ context.Context, _ string) bool { return true }, newConnectionHandler(ctx, domainHandler, ProtocolHTTP, deps))
  117  	p.proxy.AddRoute(httpAddr, newConnectionHandler(ctx, cidrOnlyHandler, ProtocolHTTP, deps))
  118  
  119  	// TLS listener (port 443 traffic): inspect SNI for domain allowlist
  120  	p.proxy.AddSNIMatchRoute(tlsAddr, func(_ context.Context, _ string) bool { return true }, newConnectionHandler(ctx, domainHandler, ProtocolTLS, deps))
  121  	p.proxy.AddRoute(tlsAddr, newConnectionHandler(ctx, cidrOnlyHandler, ProtocolTLS, deps))
  122  
  123  	// Other listener (all other ports): CIDR-only check, no protocol inspection
  124  	// This prevents blocking on server-first protocols like SSH
  125  	p.proxy.AddRoute(otherAddr, newConnectionHandler(ctx, cidrOnlyHandler, ProtocolOther, deps))
````

`packages/shared/pkg/proxy/host.go` L14-L120:

````go
   14  const sandboxSharedHostSubdomain = "sandbox."
   15  
   16  func GetTargetFromRequest() func(r *http.Request) (sandboxId string, port uint64, err error) {
   17  	return func(r *http.Request) (sandboxId string, port uint64, err error) {
   18  		if shouldParseHeaders(r.Host) && hasRoutingHeaders(r.Header) {
   19  			var ok bool
   20  			sandboxId, port, ok, err = parseHeaders(r.Header)
   21  			if err != nil {
   22  				return "", 0, err
   23  			} else if ok {
   24  				if err := id.ValidateSandboxID(sandboxId); err != nil {
   25  					return "", 0, ErrInvalidSandboxID
   26  				}
   27  
   28  				return sandboxId, port, nil
   29  			}
   30  		}
   31  
   32  		sandboxId, port, err = parseHost(r.Host)
   33  		if err != nil {
   34  			return "", 0, err
   35  		}
   36  
   37  		if err := id.ValidateSandboxID(sandboxId); err != nil {
   38  			return "", 0, ErrInvalidSandboxID
   39  		}
   40  
   41  		return sandboxId, port, nil
   42  	}
   43  }
   44  
   45  func shouldParseHeaders(host string) bool {
   46  	_, sharedHost := SandboxSharedHostDomain(host)
   47  
   48  	return isLocalRequestHost(host) || sharedHost
   49  }
   50  
   51  func requestHostname(host string) string {
   52  	return (&url.URL{Host: host}).Hostname()
   53  }
   54  
   55  func isLocalRequestHost(host string) bool {
   56  	host = requestHostname(host)
   57  	ip := net.ParseIP(host)
   58  
   59  	// An IP address cannot encode sandbox routing info (like {port}-{sandboxId}.{domain}),
   60  	// so header-based routing is the only mechanism that can work for IP hosts.
   61  	return host == "localhost" || ip != nil
   62  }
   63  
   64  func SandboxSharedHostDomain(host string) (string, bool) {
   65  	domain, ok := strings.CutPrefix(requestHostname(host), sandboxSharedHostSubdomain)
   66  
   67  	return domain, ok && domain != ""
   68  }
   69  
   70  func hasRoutingHeaders(h http.Header) bool {
   71  	return h.Get(headerSandboxID) != "" || h.Get(headerSandboxPort) != ""
   72  }
   73  
   74  func parseHost(host string) (sandboxID string, port uint64, err error) {
   75  	dot := strings.Index(host, ".")
   76  
   77  	// There must be always domain part used
   78  	if dot == -1 {
   79  		return "", 0, ErrInvalidHost
   80  	}
   81  
   82  	// Keep only the left-most subdomain part, i.e. everything before the
   83  	host = host[:dot]
   84  
   85  	hostParts := strings.Split(host, "-")
   86  	if len(hostParts) < 2 {
   87  		return "", 0, ErrInvalidHost
   88  	}
   89  
   90  	sandboxPortString := hostParts[0]
   91  	sandboxID = hostParts[1]
   92  
   93  	sandboxPort, err := strconv.ParseUint(sandboxPortString, 10, 64)
   94  	if err != nil {
   95  		return "", 0, InvalidSandboxPortError{sandboxPortString, err}
   96  	}
   97  
   98  	return sandboxID, sandboxPort, nil
   99  }
  100  
  101  type MissingHeaderError struct {
  102  	Header string
  103  }
  104  
  105  func (e MissingHeaderError) Error() string {
  106  	return fmt.Sprintf("Missing header: %s", e.Header)
  107  }
  108  
  109  const (
  110  	headerSandboxID   = "E2b-Sandbox-Id"
  111  	headerSandboxPort = "E2b-Sandbox-Port"
  112  )
  113  
  114  func parseHeaders(h http.Header) (sandboxID string, port uint64, ok bool, err error) {
  115  	sandboxID = h.Get(headerSandboxID)
  116  	portString := h.Get(headerSandboxPort)
  117  
  118  	if sandboxID == "" && portString == "" {
  119  		return "", 0, false, nil
  120  	}
````

Sandbox proxy routing: `Host: <port>-<sandboxID>.<anydomain>` (parseHost), or for an IP/`localhost` host
the headers `E2b-Sandbox-Id` + `E2b-Sandbox-Port`. The proxy dials `http://<slot HostIP>:<port>`
(orchestrator/pkg/proxy/proxy.go L123-L126). envd listens in the guest on `49983`
(`packages/shared/pkg/consts/envd.go` L4 `DefaultEnvdServerPort int64 = 49983`).

---

## A6. Health endpoint and InfoService

HTTP `GET /health` on the cmux port:

`packages/orchestrator/pkg/healthcheck/healthcheck.go` (full file, 68 lines):

````go
    1  //go:build linux
    2  
    3  package healthcheck
    4  
    5  import (
    6  	"encoding/json"
    7  	"net/http"
    8  	"sync"
    9  	"time"
   10  
   11  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/service"
   12  	e2borchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
   13  	e2bHealth "github.com/e2b-dev/infra/packages/shared/pkg/health"
   14  )
   15  
   16  type Healthcheck struct {
   17  	info *service.ServiceInfo
   18  
   19  	lastRun time.Time
   20  	mu      sync.RWMutex
   21  }
   22  
   23  func NewHealthcheck(info *service.ServiceInfo) (*Healthcheck, error) {
   24  	return &Healthcheck{
   25  		info: info,
   26  
   27  		lastRun: time.Now(),
   28  		mu:      sync.RWMutex{},
   29  	}, nil
   30  }
   31  
   32  func (h *Healthcheck) CreateHandler() http.Handler {
   33  	// Start /health HTTP server
   34  	routeMux := http.NewServeMux()
   35  	routeMux.HandleFunc("/health", h.healthHandler)
   36  
   37  	return routeMux
   38  }
   39  
   40  func (h *Healthcheck) getStatus() e2bHealth.Status {
   41  	switch h.info.GetStatus().Status {
   42  	case e2borchestratorinfo.ServiceInfoStatus_Healthy:
   43  		return e2bHealth.Healthy
   44  	case e2borchestratorinfo.ServiceInfoStatus_Draining, e2borchestratorinfo.ServiceInfoStatus_Standby, e2borchestratorinfo.ServiceInfoStatus_ShuttingDown:
   45  		return e2bHealth.Draining
   46  	}
   47  
   48  	return e2bHealth.Unhealthy
   49  }
   50  
   51  func (h *Healthcheck) healthHandler(w http.ResponseWriter, _ *http.Request) {
   52  	h.mu.RLock()
   53  	defer h.mu.RUnlock()
   54  
   55  	status := h.getStatus()
   56  	response := e2bHealth.Response{Status: status, Version: h.info.SourceCommit}
   57  
   58  	w.Header().Set("Content-Type", "application/json")
   59  	if status == e2bHealth.Unhealthy {
   60  		w.WriteHeader(http.StatusServiceUnavailable)
   61  	} else {
   62  		w.WriteHeader(http.StatusOK)
   63  	}
   64  
   65  	if err := json.NewEncoder(w).Encode(response); err != nil {
   66  		http.Error(w, err.Error(), http.StatusInternalServerError)
   67  	}
   68  }
````

`packages/shared/pkg/health/main.go` L1-L10:

````go
    1  package health
    2  
    3  type Status string
    4  
    5  const (
    6  	Healthy   Status = "healthy"
    7  	Unhealthy Status = "unhealthy"
    8  	Draining  Status = "draining"
    9  )
   10  
````
Response: `{"status":"healthy"|"draining"|"unhealthy","version":"<commit>"}`; HTTP 503 only for
`unhealthy`; `draining` (Draining/Standby/ShuttingDown) still returns 200. The initial status is Healthy
(service/info.go L129) and only `SetStatus(ShuttingDown)` at shutdown (run.go L1112) or an explicit
`ServiceStatusOverride` RPC changes it. **The orchestrator itself never refuses Create based on status** —
status is advisory for the caller (API); only `max_sandboxes` and the starting semaphore gate Create.

`packages/orchestrator/info.proto` (full file, 95 lines):

````proto
    1  syntax = "proto3";
    2  
    3  import "google/protobuf/empty.proto";
    4  import "google/protobuf/timestamp.proto";
    5  
    6  option go_package = "https://github.com/e2b-dev/infra/orchestrator";
    7  
    8  // needs to be different from the enumeration in the template manager
    9  enum ServiceInfoStatus {
   10    Healthy = 0;
   11    // Draining excludes new work while existing work finishes; it can return to Healthy.
   12    Draining = 1;
   13    Unhealthy = 2;
   14    // Standby means the node is not actively used, but it can return to Healthy and continues serving traffic.
   15    Standby = 3;
   16    // ShuttingDown drains existing work before process exit and cannot be reversed.
   17    ShuttingDown = 4;
   18  }
   19  
   20  enum ServiceInfoRole {
   21    TemplateBuilder = 0;
   22    Orchestrator = 1;
   23  }
   24  
   25  message DiskMetrics {
   26    string mount_point = 1;
   27    string device = 2;
   28    string filesystem_type = 3;
   29    uint64 used_bytes = 4;
   30    uint64 total_bytes = 5;
   31  }
   32  
   33  message MachineInfo {
   34    string cpu_architecture = 1;
   35    string cpu_family = 2;
   36    string cpu_model = 3;
   37    string cpu_model_name = 4;
   38    repeated string cpu_flags = 5;
   39  }
   40  
   41  message ServiceInfoResponse {
   42    string node_id = 1;
   43    string service_id = 2;
   44    string service_version = 3;
   45    string service_commit = 4;
   46  
   47    ServiceInfoStatus service_status = 51;
   48    repeated ServiceInfoRole service_roles = 52;
   49    google.protobuf.Timestamp service_startup = 53;
   50    MachineInfo machine_info = 54;
   51    repeated string labels = 55;
   52    google.protobuf.Timestamp service_status_changed_at = 56;
   53  
   54    // Overlapping work holds, not distinct sandboxes or builds. Zero is idle.
   55    uint64 outstanding_work = 58;
   56  
   57    // Nonpositive values reject sandbox creation.
   58    int64 max_sandboxes = 59;
   59  
   60    int64 metric_vcpu_used = 101 [deprecated = true];
   61    int64 metric_memory_used_mb = 102 [deprecated = true];
   62    int64 metric_disk_mb = 103 [deprecated = true];
   63    uint32 metric_sandboxes_running = 104;
   64  
   65    // Host system usage metrics
   66    uint32 metric_cpu_percent = 105;
   67    uint64 metric_memory_used_bytes = 106;
   68  
   69    // Host system total resources
   70    uint32 metric_cpu_count = 108;
   71    uint64 metric_memory_total_bytes = 109;
   72  
   73    // Allocated resources to sandboxes
   74    uint32 metric_cpu_allocated = 110;
   75    uint64 metric_memory_allocated_bytes = 111;
   76    uint64 metric_disk_allocated_bytes = 112;
   77  
   78    // Detailed disk metrics for each mount point
   79    repeated DiskMetrics metric_disks = 113;
   80  
   81    // Hugepage pool metrics (page counts from /proc/meminfo)
   82    uint64 metric_hugepages_total = 114;
   83    uint64 metric_hugepages_used = 115;
   84    uint64 metric_hugepages_reserved = 116;
   85    uint64 metric_hugepage_size_bytes = 117;
   86  }
   87  
   88  message ServiceStatusChangeRequest {
   89    ServiceInfoStatus service_status = 2;
   90  }
   91  
   92  service InfoService {
   93    rpc ServiceInfo(google.protobuf.Empty) returns (ServiceInfoResponse);
   94    rpc ServiceStatusOverride(ServiceStatusChangeRequest) returns (google.protobuf.Empty);
   95  }
````

`packages/orchestrator/pkg/service/info.go` (full file, 141 lines):

````go
    1  //go:build linux
    2  
    3  package service
    4  
    5  import (
    6  	"context"
    7  	"sync"
    8  	"sync/atomic"
    9  	"time"
   10  
   11  	"go.uber.org/zap"
   12  
   13  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
   14  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/service/machineinfo"
   15  	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
   16  	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
   17  )
   18  
   19  // ServiceStatus bundles the service status with the time of its last change.
   20  type ServiceStatus struct {
   21  	Status    orchestratorinfo.ServiceInfoStatus
   22  	ChangedAt time.Time
   23  }
   24  
   25  type ServiceInfo struct {
   26  	ClientId  string
   27  	ServiceId string
   28  
   29  	SourceVersion string
   30  	SourceCommit  string
   31  
   32  	Startup      time.Time
   33  	Roles        []orchestratorinfo.ServiceInfoRole
   34  	Labels       []string
   35  	MachineInfo  machineinfo.MachineInfo
   36  	MaxSandboxes atomic.Int64
   37  
   38  	status          ServiceStatus
   39  	statusMu        sync.RWMutex
   40  	outstandingWork int64
   41  }
   42  
   43  var serviceRolesMapper = map[cfg.ServiceType]orchestratorinfo.ServiceInfoRole{
   44  	cfg.Orchestrator:    orchestratorinfo.ServiceInfoRole_Orchestrator,
   45  	cfg.TemplateManager: orchestratorinfo.ServiceInfoRole_TemplateBuilder,
   46  }
   47  
   48  func (s *ServiceInfo) GetStatus() ServiceStatus {
   49  	s.statusMu.RLock()
   50  	defer s.statusMu.RUnlock()
   51  
   52  	return s.status
   53  }
   54  
   55  // Child work must be registered before its parent releases ownership.
   56  func (s *ServiceInfo) TrackWork() func() {
   57  	s.statusMu.Lock()
   58  	s.outstandingWork++
   59  	s.statusMu.Unlock()
   60  
   61  	return s.finishWork
   62  }
   63  
   64  func (s *ServiceInfo) finishWork() {
   65  	s.statusMu.Lock()
   66  	defer s.statusMu.Unlock()
   67  
   68  	s.outstandingWork--
   69  }
   70  
   71  func (s *ServiceInfo) OutstandingWork() int64 {
   72  	s.statusMu.RLock()
   73  	defer s.statusMu.RUnlock()
   74  
   75  	return s.outstandingWork
   76  }
   77  
   78  func (s *ServiceInfo) SetStatus(ctx context.Context, status orchestratorinfo.ServiceInfoStatus) {
   79  	s.statusMu.Lock()
   80  	defer s.statusMu.Unlock()
   81  
   82  	s.setStatus(ctx, status)
   83  }
   84  
   85  func (s *ServiceInfo) OverrideStatus(ctx context.Context, status orchestratorinfo.ServiceInfoStatus) bool {
   86  	s.statusMu.Lock()
   87  	defer s.statusMu.Unlock()
   88  
   89  	// Only process shutdown may enter ShuttingDown.
   90  	if status == orchestratorinfo.ServiceInfoStatus_ShuttingDown {
   91  		return false
   92  	}
   93  
   94  	if s.status.Status == orchestratorinfo.ServiceInfoStatus_Draining && status == orchestratorinfo.ServiceInfoStatus_Standby {
   95  		return false
   96  	}
   97  
   98  	return s.setStatus(ctx, status)
   99  }
  100  
  101  func (s *ServiceInfo) setStatus(ctx context.Context, status orchestratorinfo.ServiceInfoStatus) bool {
  102  	if s.status.Status == orchestratorinfo.ServiceInfoStatus_ShuttingDown && status != s.status.Status {
  103  		return false
  104  	}
  105  
  106  	if s.status.Status != status {
  107  		logger.L().Info(ctx, "Service status changed", zap.String("status", status.String()))
  108  		s.status = ServiceStatus{Status: status, ChangedAt: time.Now()}
  109  	}
  110  
  111  	return true
  112  }
  113  
  114  func NewInfoContainer(clientId string, version string, commit string, instanceID string, machineInfo machineinfo.MachineInfo, config cfg.Config) *ServiceInfo {
  115  	services := cfg.GetServices(config)
  116  	serviceRoles := make([]orchestratorinfo.ServiceInfoRole, 0)
  117  
  118  	for _, service := range services {
  119  		if role, ok := serviceRolesMapper[service]; ok {
  120  			serviceRoles = append(serviceRoles, role)
  121  		}
  122  	}
  123  
  124  	startup := time.Now()
  125  	serviceInfo := &ServiceInfo{
  126  		ClientId:  clientId,
  127  		ServiceId: instanceID,
  128  
  129  		status: ServiceStatus{Status: orchestratorinfo.ServiceInfoStatus_Healthy, ChangedAt: startup},
  130  
  131  		Startup:     startup,
  132  		Roles:       serviceRoles,
  133  		Labels:      config.NodeLabels,
  134  		MachineInfo: machineInfo,
  135  
  136  		SourceVersion: version,
  137  		SourceCommit:  commit,
  138  	}
  139  
  140  	return serviceInfo
  141  }
````

`packages/orchestrator/pkg/service/service_info.go` (full file, 155 lines):

````go
    1  //go:build linux
    2  
    3  package service
    4  
    5  import (
    6  	"context"
    7  
    8  	"go.uber.org/zap"
    9  	"google.golang.org/grpc/codes"
   10  	"google.golang.org/grpc/status"
   11  	"google.golang.org/protobuf/types/known/emptypb"
   12  	"google.golang.org/protobuf/types/known/timestamppb"
   13  
   14  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/metrics"
   15  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
   16  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/service/machineinfo"
   17  	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
   18  	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
   19  )
   20  
   21  type Server struct {
   22  	orchestratorinfo.UnimplementedInfoServiceServer
   23  
   24  	info        *ServiceInfo
   25  	sandboxes   *sandbox.Map
   26  	hostMetrics *metrics.HostMetrics
   27  }
   28  
   29  func NewInfoService(info *ServiceInfo, sandboxes *sandbox.Map, hostMetrics *metrics.HostMetrics) *Server {
   30  	return &Server{
   31  		info:        info,
   32  		sandboxes:   sandboxes,
   33  		hostMetrics: hostMetrics,
   34  	}
   35  }
   36  
   37  func (s *Server) ServiceInfo(ctx context.Context, _ *emptypb.Empty) (*orchestratorinfo.ServiceInfoResponse, error) {
   38  	info := s.info
   39  
   40  	// Get host metrics for the orchestrator
   41  	cpuMetrics, err := s.hostMetrics.GetCPUMetrics()
   42  	if err != nil {
   43  		logger.L().Warn(ctx, "Failed to get host metrics", zap.Error(err))
   44  		cpuMetrics = &metrics.CPUMetrics{}
   45  	}
   46  
   47  	memoryMetrics, err := s.hostMetrics.GetMemoryMetrics()
   48  	if err != nil {
   49  		logger.L().Warn(ctx, "Failed to get host metrics", zap.Error(err))
   50  		memoryMetrics = &metrics.MemoryMetrics{}
   51  	}
   52  
   53  	diskMetrics, err := s.hostMetrics.GetDiskMetrics()
   54  	if err != nil {
   55  		logger.L().Warn(ctx, "Failed to get host metrics", zap.Error(err))
   56  		diskMetrics = []metrics.DiskInfo{}
   57  	}
   58  
   59  	// Calculate sandbox resource allocation
   60  	sandboxVCpuAllocated := uint32(0)
   61  	sandboxMemoryAllocated := uint64(0)
   62  	sandboxDiskAllocated := uint64(0)
   63  
   64  	for _, item := range s.sandboxes.Items() {
   65  		sandboxVCpuAllocated += uint32(item.Config.Vcpu)
   66  		sandboxMemoryAllocated += uint64(item.Config.RamMB) * 1024 * 1024
   67  		sandboxDiskAllocated += uint64(item.Config.TotalDiskSizeMB) * 1024 * 1024
   68  	}
   69  
   70  	info.statusMu.RLock()
   71  	serviceStatus := info.status
   72  	outstandingWork := uint64(info.outstandingWork)
   73  	info.statusMu.RUnlock()
   74  
   75  	return &orchestratorinfo.ServiceInfoResponse{
   76  		NodeId:                 info.ClientId,
   77  		ServiceId:              info.ServiceId,
   78  		ServiceStatus:          serviceStatus.Status,
   79  		ServiceStatusChangedAt: timestamppb.New(serviceStatus.ChangedAt),
   80  		OutstandingWork:        outstandingWork,
   81  		MaxSandboxes:           info.MaxSandboxes.Load(),
   82  
   83  		ServiceVersion: info.SourceVersion,
   84  		ServiceCommit:  info.SourceCommit,
   85  
   86  		ServiceStartup: timestamppb.New(info.Startup),
   87  		ServiceRoles:   info.Roles,
   88  		MachineInfo:    convertMachineInfo(info.MachineInfo),
   89  		Labels:         info.Labels,
   90  
   91  		// Allocated resources to sandboxes
   92  		MetricCpuAllocated:         sandboxVCpuAllocated,
   93  		MetricMemoryAllocatedBytes: sandboxMemoryAllocated,
   94  		MetricDiskAllocatedBytes:   sandboxDiskAllocated,
   95  		MetricSandboxesRunning:     uint32(s.sandboxes.Count()),
   96  
   97  		// Host system usage metrics
   98  		MetricCpuPercent:      uint32(cpuMetrics.UsedPercent),
   99  		MetricMemoryUsedBytes: memoryMetrics.UsedBytes,
  100  
  101  		// Host system total resources
  102  		MetricCpuCount:         cpuMetrics.Count,
  103  		MetricMemoryTotalBytes: memoryMetrics.TotalBytes,
  104  
  105  		// Hugepage pool metrics (page counts)
  106  		MetricHugepagesTotal:    memoryMetrics.HugePagesTotal,
  107  		MetricHugepagesUsed:     memoryMetrics.HugePagesUsed,
  108  		MetricHugepagesReserved: memoryMetrics.HugePagesReserved,
  109  		MetricHugepageSizeBytes: memoryMetrics.HugePageSizeBytes,
  110  
  111  		// Detailed disk metrics
  112  		MetricDisks: convertDiskMetrics(diskMetrics),
  113  
  114  		// TODO: Remove when migrated
  115  		MetricVcpuUsed:     int64(sandboxVCpuAllocated),
  116  		MetricMemoryUsedMb: int64(sandboxMemoryAllocated / (1024 * 1024)),
  117  		MetricDiskMb:       int64(sandboxDiskAllocated / (1024 * 1024)),
  118  	}, nil
  119  }
  120  
  121  // convertDiskMetrics converts internal DiskInfo to protobuf DiskMetrics
  122  func convertDiskMetrics(disks []metrics.DiskInfo) []*orchestratorinfo.DiskMetrics {
  123  	result := make([]*orchestratorinfo.DiskMetrics, len(disks))
  124  	for i, disk := range disks {
  125  		result[i] = &orchestratorinfo.DiskMetrics{
  126  			MountPoint:     disk.MountPoint,
  127  			Device:         disk.Device,
  128  			FilesystemType: disk.FilesystemType,
  129  			UsedBytes:      disk.UsedBytes,
  130  			TotalBytes:     disk.TotalBytes,
  131  		}
  132  	}
  133  
  134  	return result
  135  }
  136  
  137  // convertDiskMetrics converts internal DiskInfo to protobuf DiskMetrics
  138  func convertMachineInfo(machineInfo machineinfo.MachineInfo) *orchestratorinfo.MachineInfo {
  139  	return &orchestratorinfo.MachineInfo{
  140  		CpuArchitecture: machineInfo.Arch,
  141  		CpuFamily:       machineInfo.Family,
  142  		CpuModel:        machineInfo.Model,
  143  		CpuModelName:    machineInfo.ModelName,
  144  		CpuFlags:        machineInfo.Flags,
  145  	}
  146  }
  147  
  148  func (s *Server) ServiceStatusOverride(ctx context.Context, req *orchestratorinfo.ServiceStatusChangeRequest) (*emptypb.Empty, error) {
  149  	logger.L().Info(ctx, "service status override request received", zap.String("status", req.GetServiceStatus().String()))
  150  	if !s.info.OverrideStatus(ctx, req.GetServiceStatus()) {
  151  		return nil, status.Errorf(codes.FailedPrecondition, "cannot override node status to %s", req.GetServiceStatus())
  152  	}
  153  
  154  	return &emptypb.Empty{}, nil
  155  }
````

`outstanding_work` = count of in-flight tracked operations (`TrackWork()` is called by Create, Update,
Delete, Pause, Checkpoint and further handlers in server/sandboxes.go L1864-L2044, the prefetch harvest
(prefetch_harvest.go L215), TemplateCreate and each running build (create_template.go L50, L234) and
TemplateBuildDelete); it counts overlapping holds, not sandboxes.
`max_sandboxes` = feature flag `max-sandboxes-per-node`, refreshed every 30 s (server/main.go L42, L461-L463).

---

# B. Patch sites

## P1. Startup reclaim scoping

Called from run.go before the network storage is created, only when the sandbox runtime is used and
`DISABLE_STARTUP_RECLAIM` is false:

`packages/orchestrator/pkg/factories/run.go` L780-L805:

````go
  780  	// Sandbox-runtime reclaim must run before NewStorageLocal below: reclaim
  781  	// deletes leaked ns-* from /run/netns, and NewStorageLocal snapshots the
  782  	// remaining namespaces as foreign at construction.
  783  	// reclaimClean is true only when reclaim ran AND tore every slot down; a
  784  	// partial failure leaves anchor namespaces that must keep their v2 set entries.
  785  	reclaimClean := false
  786  	reclaimRan := false
  787  	if usesSandboxRuntime && !config.DisableStartupReclaim {
  788  		reclaimRan = true
  789  		summary := startupreclaim.Run(ctx, startupreclaim.Config{
  790  			NetworkConfig: config.NetworkConfig,
  791  			EgressProxy:   egressSetup.Proxy,
  792  			CgroupManager: cgroupManager,
  793  			StorageConfig: config.StorageConfig,
  794  		})
  795  		reclaimClean = !summary.HasFailures()
  796  	}
  797  
  798  	if usesSandboxRuntime {
  799  		tracker, err := metrics.NewFirecrackerTracker(tel.MeterProvider, sandboxes)
  800  		if err != nil {
  801  			logger.L().Fatal(ctx, "failed to create Firecracker process tracker", zap.Error(err))
  802  		}
  803  		startService("Firecracker process tracker", func() error { return tracker.Start(ctx) })
  804  		closers = append(closers, closer{"Firecracker process tracker", tracker.Close})
  805  	}
````

`packages/orchestrator/pkg/startupreclaim/reclaim.go` (full file, 170 lines):

````go
    1  //go:build linux
    2  
    3  package startupreclaim
    4  
    5  import (
    6  	"context"
    7  	"os"
    8  
    9  	"go.opentelemetry.io/otel"
   10  	"go.opentelemetry.io/otel/attribute"
   11  	"go.opentelemetry.io/otel/metric"
   12  	"go.uber.org/zap"
   13  
   14  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/cgroup"
   15  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd"
   16  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
   17  	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
   18  	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
   19  	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
   20  )
   21  
   22  const (
   23  	resourceFirecracker = "firecracker"
   24  	resourceNBD         = "nbd"
   25  	resourceNetwork     = "network"
   26  	resourceCgroup      = "cgroup"
   27  	resourceFile        = "file"
   28  
   29  	procDir = "/proc"
   30  )
   31  
   32  var (
   33  	meter = otel.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/startupreclaim")
   34  
   35  	reclaimedCounter = utils.Must(meter.Int64Counter("orchestrator.startup_reclaim.reclaimed",
   36  		metric.WithDescription("Startup reclaim resources successfully reclaimed."),
   37  		metric.WithUnit("{resource}"),
   38  	))
   39  	failedCounter = utils.Must(meter.Int64Counter("orchestrator.startup_reclaim.failed",
   40  		metric.WithDescription("Startup reclaim resource cleanup failures."),
   41  		metric.WithUnit("{resource}"),
   42  	))
   43  )
   44  
   45  type Config struct {
   46  	NetworkConfig network.Config
   47  	EgressProxy   network.EgressProxy
   48  	CgroupManager cgroup.Manager
   49  	StorageConfig storage.Config
   50  	TempDir       string
   51  	ProcDir       string
   52  	NetnsDir      string
   53  	CgroupRoot    string
   54  }
   55  
   56  type Summary struct {
   57  	Reclaimed map[string]int
   58  	Failed    map[string]int
   59  }
   60  
   61  func (s Summary) totalReclaimed() int {
   62  	return total(s.Reclaimed)
   63  }
   64  
   65  func (s Summary) totalFailed() int {
   66  	return total(s.Failed)
   67  }
   68  
   69  // HasFailures reports whether any resource failed to reclaim.
   70  func (s Summary) HasFailures() bool {
   71  	return s.totalFailed() > 0
   72  }
   73  
   74  func total(values map[string]int) int {
   75  	total := 0
   76  	for _, value := range values {
   77  		total += value
   78  	}
   79  
   80  	return total
   81  }
   82  
   83  // reclaimer reclaims leaked resources of a single kind. reclaim is best-effort
   84  // and never fatal, returning the count reclaimed and any failures.
   85  type reclaimer struct {
   86  	resource string
   87  	reclaim  func(ctx context.Context) (int, []error)
   88  }
   89  
   90  func Run(ctx context.Context, config Config) Summary {
   91  	config = config.withDefaults()
   92  
   93  	// No egress proxy wired: slots are still torn down, but the egress firewall
   94  	// cleanup (OnSlotDelete) is skipped and those iptables rules may leak. Log it
   95  	// instead of substituting silently; callers with no egress firewall can pass
   96  	// network.NewNoopEgressProxy() to opt in quietly.
   97  	if config.EgressProxy == nil {
   98  		logger.L().Error(ctx, "startup reclaim: no egress proxy provided; egress firewall cleanup will be skipped for reclaimed slots")
   99  		config.EgressProxy = network.NewNoopEgressProxy()
  100  	}
  101  
  102  	summary := Summary{Reclaimed: map[string]int{}, Failed: map[string]int{}}
  103  
  104  	// Order matters: firecracker runs first so the VMMs are killed before the
  105  	// network reclaim tears down the slots they used.
  106  	reclaimers := []reclaimer{
  107  		{resourceFirecracker, func(ctx context.Context) (int, []error) {
  108  			return reclaimFirecrackers(ctx, config.ProcDir)
  109  		}},
  110  		{resourceNBD, nbd.ReclaimLeaked},
  111  		{resourceNetwork, func(context.Context) (int, []error) {
  112  			return network.ReclaimLeakedSlots(config.NetnsDir, config.NetworkConfig, config.EgressProxy)
  113  		}},
  114  		{resourceCgroup, func(ctx context.Context) (int, []error) {
  115  			return cgroup.ReclaimLeaked(ctx, config.CgroupManager, config.CgroupRoot)
  116  		}},
  117  		{resourceFile, func(context.Context) (int, []error) {
  118  			return storage.ReclaimSandboxFiles(config.TempDir, config.StorageConfig.SandboxCacheDir)
  119  		}},
  120  	}
  121  
  122  	for _, r := range reclaimers {
  123  		reclaimed, failures := r.reclaim(ctx)
  124  		record(ctx, &summary, r.resource, reclaimed, failures)
  125  	}
  126  
  127  	fields := []zap.Field{
  128  		zap.Any("reclaimed", summary.Reclaimed),
  129  		zap.Any("failed", summary.Failed),
  130  		zap.Int("total_reclaimed", summary.totalReclaimed()),
  131  		zap.Int("total_failed", summary.totalFailed()),
  132  	}
  133  	if summary.totalReclaimed() > 0 || summary.totalFailed() > 0 {
  134  		logger.L().Warn(ctx, "startup resource reclaim completed with leftover resources", fields...)
  135  	} else {
  136  		logger.L().Info(ctx, "startup resource reclaim completed cleanly", fields...)
  137  	}
  138  
  139  	return summary
  140  }
  141  
  142  func (c Config) withDefaults() Config {
  143  	if c.TempDir == "" {
  144  		c.TempDir = os.TempDir()
  145  	}
  146  	if c.ProcDir == "" {
  147  		c.ProcDir = procDir
  148  	}
  149  	if c.NetnsDir == "" {
  150  		c.NetnsDir = network.NetNamespacesDir
  151  	}
  152  	if c.CgroupRoot == "" {
  153  		c.CgroupRoot = cgroup.RootCgroupPath
  154  	}
  155  
  156  	return c
  157  }
  158  
  159  func record(ctx context.Context, summary *Summary, resource string, reclaimed int, failures []error) {
  160  	if reclaimed > 0 {
  161  		summary.Reclaimed[resource] += reclaimed
  162  		reclaimedCounter.Add(ctx, int64(reclaimed), metric.WithAttributes(attribute.String("resource_type", resource)))
  163  	}
  164  
  165  	for _, err := range failures {
  166  		summary.Failed[resource]++
  167  		failedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("resource_type", resource)))
  168  		logger.L().Warn(ctx, "startup resource reclaim failed", zap.String("resource_type", resource), zap.Error(err))
  169  	}
  170  }
````

`packages/orchestrator/pkg/startupreclaim/firecracker.go` (full file, 112 lines):

````go
    1  //go:build linux
    2  
    3  package startupreclaim
    4  
    5  import (
    6  	"bytes"
    7  	"context"
    8  	"errors"
    9  	"fmt"
   10  	"os"
   11  	"path/filepath"
   12  	"strconv"
   13  	"syscall"
   14  
   15  	"go.uber.org/zap"
   16  
   17  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/artifact"
   18  	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
   19  )
   20  
   21  // reclaimFirecrackers kills orphaned firecracker process groups by scanning the
   22  // host process table. The network slots they used are reclaimed separately by
   23  // the network reclaim.
   24  func reclaimFirecrackers(ctx context.Context, procDir string) (int, []error) {
   25  	pids, err := discoverFirecrackerPIDs(procDir)
   26  	if err != nil {
   27  		return 0, []error{err}
   28  	}
   29  
   30  	reclaimed := 0
   31  	var failures []error
   32  	for _, pid := range pids {
   33  		pgid, err := syscall.Getpgid(pid)
   34  		if err != nil {
   35  			failures = append(failures, fmt.Errorf("failed to get firecracker pgid for pid %d: %w", pid, err))
   36  
   37  			continue
   38  		}
   39  
   40  		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
   41  			failures = append(failures, fmt.Errorf("failed to kill firecracker process group %d for pid %d: %w", pgid, pid, err))
   42  
   43  			continue
   44  		}
   45  
   46  		reclaimed++
   47  		logger.L().Warn(ctx, "killed orphaned firecracker process group",
   48  			zap.Int("pid", pid),
   49  			zap.Int("pgid", pgid))
   50  	}
   51  
   52  	return reclaimed, failures
   53  }
   54  
   55  // discoverFirecrackerPIDs scans procDir for leftover firecracker processes.
   56  func discoverFirecrackerPIDs(procDir string) ([]int, error) {
   57  	entries, err := os.ReadDir(procDir)
   58  	if err != nil {
   59  		return nil, fmt.Errorf("failed to read proc directory: %w", err)
   60  	}
   61  
   62  	pids := make([]int, 0)
   63  	for _, entry := range entries {
   64  		if !entry.IsDir() {
   65  			continue
   66  		}
   67  
   68  		pid, err := strconv.Atoi(entry.Name())
   69  		if err != nil {
   70  			continue
   71  		}
   72  
   73  		cmdline, err := processCmdline(procDir, pid)
   74  		if err != nil || !isFirecrackerCmdline(cmdline) {
   75  			continue
   76  		}
   77  
   78  		pids = append(pids, pid)
   79  	}
   80  
   81  	return pids, nil
   82  }
   83  
   84  func processCmdline(procDir string, pid int) ([]string, error) {
   85  	data, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "cmdline"))
   86  	if err != nil {
   87  		return nil, err
   88  	}
   89  
   90  	data = bytes.TrimRight(data, "\x00")
   91  	if len(data) == 0 {
   92  		return nil, nil
   93  	}
   94  
   95  	parts := bytes.Split(data, []byte{0})
   96  	cmdline := make([]string, 0, len(parts))
   97  	for _, part := range parts {
   98  		cmdline = append(cmdline, string(part))
   99  	}
  100  
  101  	return cmdline, nil
  102  }
  103  
  104  func isFirecrackerCmdline(cmdline []string) bool {
  105  	if len(cmdline) == 0 {
  106  		return false
  107  	}
  108  
  109  	// Exact basename match so versioned paths match but "firecracker-monitor"
  110  	// and similar do not.
  111  	return filepath.Base(cmdline[0]) == artifact.FirecrackerBinaryName
  112  }
````

**Current scope is host-wide**: `discoverFirecrackerPIDs` walks all of `/proc` and matches any process
whose `argv[0]` basename is exactly `firecracker` (L104-L112), then SIGKILLs its **whole process group**
(L33-L44). Any other Firecracker on the host (e.g. a second VMM manager) is killed at every orchestrator
start.

How the orchestrator launches Firecracker (this is what the scoping criterion must match):

`packages/orchestrator/pkg/sandbox/fc/process.go` L192-L273:

````go
  192  func validateFirecrackerBinary(versions Config, config cfg.BuilderConfig) error {
  193  	firecrackerPath := versions.FirecrackerPath(config)
  194  	_, err := os.Stat(firecrackerPath)
  195  	if err == nil {
  196  		return nil
  197  	}
  198  
  199  	if errors.Is(err, os.ErrNotExist) {
  200  		archPath, legacyPath := versions.firecrackerPaths(config)
  201  
  202  		return fmt.Errorf("firecracker binary not found; checked architecture-specific path %q and legacy path %q: %w", archPath, legacyPath, err)
  203  	}
  204  
  205  	return fmt.Errorf("error stating firecracker binary %q: %w", firecrackerPath, err)
  206  }
  207  
  208  func NewProcess(
  209  	ctx context.Context,
  210  	execCtx context.Context,
  211  	config cfg.BuilderConfig,
  212  	slot *network.Slot,
  213  	files *storage.SandboxFiles,
  214  	versions Config,
  215  	rootfsProvider rootfs.Provider,
  216  	rootfsPaths RootfsPaths,
  217  ) (*Process, error) {
  218  	ctx, childSpan := tracer.Start(ctx, "initialize-fc", trace.WithAttributes(
  219  		attribute.Int("sandbox.slot.index", slot.Idx),
  220  	))
  221  	defer childSpan.End()
  222  
  223  	// Build the firecracker start script and get computed paths
  224  	startBuilder := NewStartScriptBuilder(config)
  225  	startScript, err := startBuilder.Build(versions, files, rootfsPaths, slot.NamespaceID())
  226  	if err != nil {
  227  		return nil, err
  228  	}
  229  
  230  	telemetry.SetAttributes(ctx,
  231  		attribute.String("sandbox.cmd", startScript.Value),
  232  	)
  233  
  234  	if err = validateFirecrackerBinary(versions, config); err != nil {
  235  		return nil, err
  236  	}
  237  
  238  	_, err = os.Stat(versions.HostKernelPath(config))
  239  	if err != nil {
  240  		return nil, fmt.Errorf("error stating kernel file: %w", err)
  241  	}
  242  
  243  	cmd := exec.CommandContext(execCtx,
  244  		"unshare",
  245  		"-m",
  246  		"--",
  247  		"bash",
  248  		"-c",
  249  		startScript.Value,
  250  	)
  251  
  252  	p := &Process{
  253  		Versions:              versions,
  254  		Exit:                  utils.NewErrorOnce(),
  255  		cmd:                   cmd,
  256  		firecrackerSocketPath: files.SandboxFirecrackerSocketPath(),
  257  		metricsPath:           files.SandboxMetricsFifoPath(),
  258  		config:                config,
  259  		client:                newApiClient(files.SandboxFirecrackerSocketPath()),
  260  		rootfsProvider:        rootfsProvider,
  261  		files:                 files,
  262  		slot:                  slot,
  263  
  264  		kernelPath: startScript.KernelPath,
  265  		rootfsPath: startScript.RootfsPath,
  266  	}
  267  
  268  	cmd.SysProcAttr = &syscall.SysProcAttr{
  269  		Setsid: true, // Create a new session
  270  	}
  271  
  272  	return p, nil
  273  }
````

`packages/orchestrator/pkg/sandbox/fc/process.go` L298-L320:

````go
  298  
  299  	// Set up cgroup FD for atomic placement via CLONE_INTO_CGROUP.
  300  	// The cgroup is created and owned by the caller (Sandbox); Process only
  301  	// uses the FD during clone.
  302  	if cgroupFD != cgroup.NoCgroupFD {
  303  		p.cmd.SysProcAttr.UseCgroupFD = true
  304  		p.cmd.SysProcAttr.CgroupFD = cgroupFD
  305  	}
  306  
  307  	// Create the metrics FIFO before Firecracker starts.
  308  	// Firecracker will open the write end once PUT /metrics is called.
  309  	if err := syscall.Mkfifo(p.metricsPath, 0o600); err != nil {
  310  		return fmt.Errorf("error creating fc metrics FIFO: %w", err)
  311  	}
  312  
  313  	err := p.cmd.Start()
  314  	if err != nil {
  315  		// cmd.Process is nil when Start fails, so Stop() won't reach the FIFO cleanup.
  316  		// Remove the FIFO here to avoid leaving it behind.
  317  		_ = os.Remove(p.metricsPath)
  318  
  319  		return fmt.Errorf("error starting fc process: %w", err)
  320  	}
````

`packages/orchestrator/pkg/sandbox/fc/script_builder.go` L45-L110:

````go
   45  const startScriptV1 = `mount --make-rprivate / &&
   46  
   47  mount -t tmpfs tmpfs {{ .DeprecatedSandboxRootfsDir }} -o X-mount.mkdir &&
   48  ln -s {{ .HostRootfsPath }} {{ .DeprecatedSandboxRootfsDir }}/{{ .SandboxRootfsFile }} &&
   49  
   50  mount -t tmpfs tmpfs {{ .SandboxDir }}/{{ .SandboxKernelDir }} -o X-mount.mkdir &&
   51  ln -s {{ .HostKernelPath }} {{ .SandboxDir }}/{{ .SandboxKernelDir }}/{{ .SandboxKernelFile }} &&
   52  
   53  ip netns exec {{ .NamespaceID }} {{ .FirecrackerPath }} --api-sock {{ .FirecrackerSocket }}`
   54  
   55  const startScriptV2 = `mount --make-rprivate / &&
   56  mount -t tmpfs tmpfs {{ .SandboxDir }} -o X-mount.mkdir &&
   57  
   58  ln -s {{ .HostRootfsPath }} {{ .SandboxDir }}/{{ .SandboxRootfsFile }} &&
   59  
   60  mkdir -p {{ .SandboxDir }}/{{ .SandboxKernelDir }} &&
   61  ln -s {{ .HostKernelPath }} {{ .SandboxDir }}/{{ .SandboxKernelDir }}/{{ .SandboxKernelFile }} &&
   62  
   63  ip netns exec {{ .NamespaceID }} {{ .FirecrackerPath }} --api-sock {{ .FirecrackerSocket }}`
   64  
   65  // StartScriptBuilder handles the creation and execution of firecracker start scripts
   66  type StartScriptBuilder struct {
   67  	builderConfig cfg.BuilderConfig
   68  	templateV1    *txtTemplate.Template
   69  	templateV2    *txtTemplate.Template
   70  }
   71  
   72  // NewStartScriptBuilder creates a new StartScriptBuilder instance
   73  func NewStartScriptBuilder(builderConfig cfg.BuilderConfig) *StartScriptBuilder {
   74  	templateV1 := txtTemplate.Must(txtTemplate.New("fc-start-v1").Parse(startScriptV1))
   75  	templateV2 := txtTemplate.Must(txtTemplate.New("fc-start-v2").Parse(startScriptV2))
   76  
   77  	return &StartScriptBuilder{
   78  		builderConfig: builderConfig,
   79  		templateV1:    templateV1,
   80  		templateV2:    templateV2,
   81  	}
   82  }
   83  
   84  // buildArgs prepares the arguments for the start script template
   85  func (sb *StartScriptBuilder) buildArgs(
   86  	versions Config,
   87  	files *storage.SandboxFiles,
   88  	rootfsPaths RootfsPaths,
   89  	namespaceID string,
   90  ) startScriptArgs {
   91  	return startScriptArgs{
   92  		// General
   93  		SandboxDir: sb.builderConfig.SandboxDir,
   94  
   95  		// Kernel
   96  		HostKernelPath:    versions.HostKernelPath(sb.builderConfig),
   97  		SandboxKernelDir:  versions.SandboxKernelDir(),
   98  		SandboxKernelFile: artifact.KernelFileName,
   99  
  100  		// Rootfs
  101  		HostRootfsPath:             files.SandboxCacheRootfsLinkPath(sb.builderConfig.StorageConfig),
  102  		DeprecatedSandboxRootfsDir: rootfsPaths.DeprecatedSandboxRootfsDir(),
  103  		SandboxRootfsFile:          artifact.RootfsFileName,
  104  
  105  		// FC
  106  		NamespaceID:       namespaceID,
  107  		FirecrackerPath:   versions.FirecrackerPath(sb.builderConfig),
  108  		FirecrackerSocket: files.SandboxFirecrackerSocketPath(),
  109  	}
  110  }
````

Concrete process tree per sandbox: `unshare -m -- bash -c '<script>'` started with `Setsid: true` (new
session, so pgid == pid of `unshare`, which execs `bash`), placed atomically into the sandbox cgroup via
`CLONE_INTO_CGROUP` when `cgroupFD != NoCgroupFD` (L302-L305), whose last command is
`ip netns exec ns-<idx> <FirecrackerPath> --api-sock <TMPDIR>/fc-<sbx>-<rand>.sock`. `ip netns exec`
execvp's the given path, so the Firecracker process's `/proc/<pid>/cmdline` argv[0] is the **absolute
path** `FIRECRACKER_VERSIONS_DIR/<ver>/<arch>/firecracker` (or the legacy flat path), argv[1..2] =
`--api-sock`, `<TMPDIR>/fc-...sock`, and `/proc/<pid>/cgroup` is `0::/e2b/sbx-<sandboxID>-<rand>`.

**Minimal change points (P1-FC):**
1. Add `FirecrackerVersionsDir string` to `startupreclaim.Config` (reclaim.go L45-L54) and set it at
   run.go L789-L794 (`FirecrackerVersionsDir: config.FirecrackerVersionsDir`).
2. Pass it to `reclaimFirecrackers(ctx, config.ProcDir)` (reclaim.go L107-L109) -> `discoverFirecrackerPIDs`
   -> `isFirecrackerCmdline(cmdline, versionsDir)` and require
   `strings.HasPrefix(filepath.Clean(cmdline[0]), filepath.Clean(versionsDir)+"/")` in addition to the
   basename check (firecracker.go L104-L112). Optionally also accept/require that
   `/proc/<pid>/cgroup` contains `/e2b/` (the cgroup root is a fixed const, cgroup/manager.go L26), which
   additionally excludes a foreign FC that happens to live under the same versions dir. Use a dedicated
   `FIRECRACKER_VERSIONS_DIR` (not shared with the other VMM manager) so the prefix is unambiguous.
3. The process-group kill (L33-L40) is then safe: the pgid is the `unshare` session leader created by
   this orchestrator.

Tests: `packages/orchestrator/pkg/startupreclaim/firecracker_test.go` exercises `isFirecrackerCmdline`
and `discoverFirecrackerPIDs` with a fake proc dir; update their signatures.

### P1-NET: network slot reclaim (ns-* netns)

`packages/orchestrator/pkg/sandbox/network/reclaim.go` (full file, 42 lines):

````go
    1  //go:build linux
    2  
    3  package network
    4  
    5  import (
    6  	"fmt"
    7  )
    8  
    9  // ReclaimLeakedSlots tears down network namespaces and slots left over from a
   10  // previous orchestrator run. It discovers leaked slots by scanning netnsDir:
   11  // the netns entry is a reliable record of every leaked slot, because it is
   12  // created before any host-side networking in CreateNetwork and removed last in
   13  // RemoveNetwork (which preserves it as a rediscovery anchor if host-side
   14  // teardown fails). It returns the number of slots removed and any failures.
   15  func ReclaimLeakedSlots(netnsDir string, config Config, egressProxy EgressProxy) (int, []error) {
   16  	var failures []error
   17  
   18  	slots, err := ListSlotNamespaces(netnsDir)
   19  	if err != nil {
   20  		failures = append(failures, err)
   21  	}
   22  
   23  	reclaimed := 0
   24  	for _, idx := range slots {
   25  		slot, err := NewSlot(fmt.Sprintf("startup-reclaim-%d", idx), idx, config, egressProxy)
   26  		if err != nil {
   27  			failures = append(failures, err)
   28  
   29  			continue
   30  		}
   31  
   32  		if err := slot.RemoveNetwork(); err != nil {
   33  			failures = append(failures, fmt.Errorf("failed to remove network slot %d: %w", idx, err))
   34  
   35  			continue
   36  		}
   37  
   38  		reclaimed++
   39  	}
   40  
   41  	return reclaimed, failures
   42  }
````

`packages/orchestrator/pkg/sandbox/network/storage_local.go` L22-L66:

````go
   22  type StorageLocal struct {
   23  	config    Config
   24  	slotsSize int
   25  	netnsDir  string
   26  	// foreignNs holds the namespace names found in netnsDir at construction
   27  	// (after startup reclaim); this storage never allocates them. The
   28  	// snapshot is immutable — anything appearing later is handled by the
   29  	// per-scan availability check in Acquire.
   30  	foreignNs map[string]struct{}
   31  	// leakedNs holds indexes Release freed while their namespace still
   32  	// existed — failed teardowns whose namespace RemoveNetwork kept as the
   33  	// reclaim anchor. Acquire falls back to them when no clean index is
   34  	// left, and CreateNetwork finishes the teardown before reuse.
   35  	leakedNs     map[string]struct{}
   36  	acquiredNs   map[string]struct{}
   37  	acquiredNsMu sync.Mutex
   38  	egressProxy  EgressProxy
   39  }
   40  
   41  const NetNamespacesDir = "/var/run/netns"
   42  
   43  func NewStorageLocal(ctx context.Context, config Config, egressProxy EgressProxy) (*StorageLocal, error) {
   44  	// get namespaces that we want to always skip
   45  	foreignNs, err := getForeignNamespaces(NetNamespacesDir)
   46  	if err != nil {
   47  		return nil, fmt.Errorf("error getting already used namespaces: %w", err)
   48  	}
   49  
   50  	foreignNsMap := make(map[string]struct{})
   51  	for _, ns := range foreignNs {
   52  		foreignNsMap[ns] = struct{}{}
   53  		logger.L().Info(ctx, fmt.Sprintf("Found foreign namespace: %s", ns))
   54  	}
   55  
   56  	return &StorageLocal{
   57  		config:       config,
   58  		netnsDir:     NetNamespacesDir,
   59  		foreignNs:    foreignNsMap,
   60  		leakedNs:     make(map[string]struct{}),
   61  		slotsSize:    vrtSlotsSize,
   62  		acquiredNs:   make(map[string]struct{}, vrtSlotsSize),
   63  		acquiredNsMu: sync.Mutex{},
   64  		egressProxy:  egressProxy,
   65  	}, nil
   66  }
````

`packages/orchestrator/pkg/sandbox/network/storage_local.go` L228-L275:

````go
  228  func getSlotName(slotIdx int) string {
  229  	slotIdxStr := strconv.Itoa(slotIdx)
  230  
  231  	return fmt.Sprintf("ns-%s", slotIdxStr)
  232  }
  233  
  234  func SlotIndexFromNamespace(name string) (int, bool) {
  235  	idxStr, ok := strings.CutPrefix(name, "ns-")
  236  	if !ok || idxStr == "" {
  237  		return 0, false
  238  	}
  239  
  240  	idx, err := strconv.Atoi(idxStr)
  241  	if err != nil || idx < 1 || idx > vrtSlotsSize {
  242  		return 0, false
  243  	}
  244  
  245  	return idx, true
  246  }
  247  
  248  func ListSlotNamespaces(dir string) ([]int, error) {
  249  	files, err := os.ReadDir(dir)
  250  	if err != nil {
  251  		if os.IsNotExist(err) {
  252  			return nil, nil
  253  		}
  254  
  255  		return nil, fmt.Errorf("error reading netns directory: %w", err)
  256  	}
  257  
  258  	indices := make([]int, 0, len(files))
  259  	for _, file := range files {
  260  		if file.IsDir() {
  261  			continue
  262  		}
  263  
  264  		idx, ok := SlotIndexFromNamespace(file.Name())
  265  		if !ok {
  266  			continue
  267  		}
  268  
  269  		indices = append(indices, idx)
  270  	}
  271  
  272  	slices.Sort(indices)
  273  
  274  	return indices, nil
  275  }
````

Scope: every file `/var/run/netns/ns-<N>` with `1 <= N <= vrtSlotsSize` (32766 with the default /16) is
treated as this orchestrator's leaked slot and torn down with `RemoveNetwork` (network.go L363-L452:
deletes FORWARD/nat rules keyed by `veth-<N>`, the host route to `10.11.x.y/32`, the veth and finally the
netns). Namespaces with other names are untouched and are also skipped at allocation (`foreignNs`, L43-L66,
snapshot taken *after* reclaim). The name prefix `ns-` is hard-coded in `Slot.NamespaceID()` (slot.go
L199-L201), `getSlotName` and `SlotIndexFromNamespace` (L234-L246).
**Change point if another tool on the host uses `ns-<int>` names:** make the prefix configurable (e.g.
`e2b-ns-`) in these three places; reclaim and allocation then follow automatically. If no other tool uses
`ns-<int>`, no change is needed. The veth names `veth-<N>` (slot.go L171-L173) and host routes in
`10.11.0.0/16` must also not collide with other software.

### P1-NBD: NBD reclaim

`packages/orchestrator/pkg/sandbox/nbd/reclaim.go` (full file, 32 lines):

````go
    1  //go:build linux
    2  
    3  package nbd
    4  
    5  import (
    6  	"context"
    7  	"fmt"
    8  )
    9  
   10  // ReclaimLeaked disconnects every currently-connected NBD device left over from
   11  // a previous orchestrator run. It returns the number of devices disconnected and
   12  // any per-device failures.
   13  func ReclaimLeaked(ctx context.Context) (int, []error) {
   14  	devices, err := ConnectedDevices()
   15  	if err != nil {
   16  		return 0, []error{err}
   17  	}
   18  
   19  	reclaimed := 0
   20  	var failures []error
   21  	for _, device := range devices {
   22  		if err := DisconnectDevice(ctx, device); err != nil {
   23  			failures = append(failures, fmt.Errorf("failed to disconnect nbd%d: %w", device, err))
   24  
   25  			continue
   26  		}
   27  
   28  		reclaimed++
   29  	}
   30  
   31  	return reclaimed, failures
   32  }
````

`packages/orchestrator/pkg/sandbox/nbd/pool.go` L138-L169:

````go
  138  func ConnectedDevices() ([]DeviceSlot, error) {
  139  	maxDevices, err := getMaxDevices()
  140  	if err != nil {
  141  		return nil, err
  142  	}
  143  
  144  	devices := make([]DeviceSlot, 0)
  145  	for slot := DeviceSlot(0); slot < DeviceSlot(maxDevices); slot++ {
  146  		connected, err := isDeviceConnectedIn(sysBlockDir, slot)
  147  		if err != nil {
  148  			return nil, err
  149  		}
  150  		if connected {
  151  			devices = append(devices, slot)
  152  		}
  153  	}
  154  
  155  	return devices, nil
  156  }
  157  
  158  func isDeviceConnectedIn(blockDir string, slot DeviceSlot) (bool, error) {
  159  	pidFile := fmt.Sprintf("%s/nbd%d/pid", blockDir, slot)
  160  	_, err := os.Stat(pidFile)
  161  	if err == nil {
  162  		return true, nil
  163  	}
  164  	if !os.IsNotExist(err) {
  165  		return false, fmt.Errorf("failed to stat pid file: %w", err)
  166  	}
  167  
  168  	return false, nil
  169  }
````

Scope: **every** connected `/dev/nbdN` on the host (`/sys/block/nbdN/pid` exists) is disconnected. The
orchestrator connects devices itself via netlink (`nbdnl.Connect`, nbd/path_direct.go L257), so the
kernel's `pid` file holds the orchestrator's PID. **Change point:** in `ReclaimLeaked` (or
`ConnectedDevices`) read `/sys/block/nbd<N>/pid` and only disconnect when that PID no longer exists or
`/proc/<pid>/exe` resolves to this orchestrator binary (`os.Executable()`); leave devices owned by live
foreign processes alone. If nothing else on the host uses NBD, no change is needed.

### P1-CG and P1-FILE: cgroup and file reclaim

`packages/orchestrator/pkg/sandbox/cgroup/reclaim.go` (full file, 35 lines):

````go
    1  package cgroup
    2  
    3  import (
    4  	"context"
    5  	"errors"
    6  	"fmt"
    7  )
    8  
    9  // ReclaimLeaked destroys every sandbox cgroup under root left over from a
   10  // previous orchestrator run, killing any remaining processes in them. It returns
   11  // the number of cgroups destroyed and any failures.
   12  func ReclaimLeaked(ctx context.Context, manager Manager, root string) (int, []error) {
   13  	if manager == nil {
   14  		return 0, []error{errors.New("cgroup manager is nil")}
   15  	}
   16  
   17  	names, err := ListSandboxCgroups(root)
   18  	if err != nil {
   19  		return 0, []error{err}
   20  	}
   21  
   22  	reclaimed := 0
   23  	var failures []error
   24  	for _, name := range names {
   25  		if err := manager.Destroy(ctx, name); err != nil {
   26  			failures = append(failures, fmt.Errorf("failed to remove cgroup %s: %w", name, err))
   27  
   28  			continue
   29  		}
   30  
   31  		reclaimed++
   32  	}
   33  
   34  	return reclaimed, failures
   35  }
````

`packages/orchestrator/pkg/sandbox/cgroup/manager.go` L547-L569:

````go
  547  func ListSandboxCgroups(root string) ([]string, error) {
  548  	entries, err := os.ReadDir(root)
  549  	if err != nil {
  550  		if os.IsNotExist(err) {
  551  			return nil, nil
  552  		}
  553  
  554  		return nil, fmt.Errorf("failed to read cgroup root: %w", err)
  555  	}
  556  
  557  	names := make([]string, 0, len(entries))
  558  	for _, entry := range entries {
  559  		if !entry.IsDir() || !IsSandboxCgroupName(entry.Name()) {
  560  			continue
  561  		}
  562  
  563  		names = append(names, entry.Name())
  564  	}
  565  
  566  	slices.Sort(names)
  567  
  568  	return names, nil
  569  }
````

`packages/shared/pkg/storage/sandbox.go` L94-L133:

````go
   94  // ReclaimSandboxFiles removes leaked sandbox files matching SandboxFileGlobs,
   95  // left over from sandboxes that did not shut down cleanly. It returns the number
   96  // of files removed and any per-file removal failures. Files that no longer exist
   97  // are treated as already reclaimed.
   98  func ReclaimSandboxFiles(tempDir, sandboxCacheDir string) (int, []error) {
   99  	paths, err := matchingSandboxFiles(tempDir, sandboxCacheDir)
  100  	if err != nil {
  101  		return 0, []error{err}
  102  	}
  103  
  104  	reclaimed := 0
  105  	var failures []error
  106  	for _, path := range paths {
  107  		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
  108  			failures = append(failures, fmt.Errorf("failed to remove %s: %w", path, err))
  109  
  110  			continue
  111  		}
  112  
  113  		reclaimed++
  114  	}
  115  
  116  	return reclaimed, failures
  117  }
  118  
  119  func matchingSandboxFiles(tempDir, sandboxCacheDir string) ([]string, error) {
  120  	paths := make([]string, 0)
  121  	for _, pattern := range SandboxFileGlobs(tempDir, sandboxCacheDir) {
  122  		matches, err := filepath.Glob(pattern)
  123  		if err != nil {
  124  			return nil, fmt.Errorf("failed to glob %s: %w", pattern, err)
  125  		}
  126  
  127  		paths = append(paths, matches...)
  128  	}
  129  
  130  	slices.Sort(paths)
  131  
  132  	return paths, nil
  133  }
````

Cgroup reclaim is already scoped to `/sys/fs/cgroup/e2b/` and `IsSandboxCgroupName` (`sbx-*`). File
reclaim globs `os.TempDir()` for `fc-*-*.sock`, `uffd-*-*.sock`, `fc-metrics-*-*.fifo` — patterns another
Firecracker manager may also use in `/tmp`. **Zero-code mitigation:** run the orchestrator with a
dedicated short `TMPDIR` (e.g. `TMPDIR=/run/e2b`, keep it short: UNIX socket paths are limited to 108
bytes and the name already carries a sandbox id plus a random id).

## P2. Shutdown: 15 s admission sleep and sandbox drain

Full SIGTERM handling (signal context at L424, wait at L1097-L1103):

`packages/orchestrator/pkg/factories/run.go` L421-L426:

````go
  421  	ctx, cancel := context.WithCancel(context.Background())
  422  	defer cancel()
  423  
  424  	sig, sigCancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
  425  	defer sigCancel()
  426  
````

`packages/orchestrator/pkg/factories/run.go` L1097-L1156:

````go
 1097  	// Wait for the shutdown signal or if some service fails
 1098  	select {
 1099  	case <-sig.Done():
 1100  		logger.L().Info(ctx, "Shutdown signal received")
 1101  	case serviceErr := <-serviceError:
 1102  		logger.L().Error(ctx, "Service error", zap.Error(serviceErr))
 1103  	}
 1104  
 1105  	closeCtx, cancelCloseCtx := context.WithCancel(context.Background())
 1106  	defer cancelCloseCtx()
 1107  	if config.ForceStop {
 1108  		cancelCloseCtx()
 1109  	}
 1110  
 1111  	logger.L().Info(ctx, "Starting drain phase", zap.Int("sandbox_count", sandboxes.Count()))
 1112  	serviceInfo.SetStatus(ctx, orchestratorinfo.ServiceInfoStatus_ShuttingDown)
 1113  
 1114  	// Consumers must stop assigning work before services drain.
 1115  	if !env.IsLocal() {
 1116  		time.Sleep(15 * time.Second)
 1117  	}
 1118  
 1119  	// Wait for services to be drained before closing them
 1120  	if tmpl != nil {
 1121  		err := tmpl.Wait(closeCtx)
 1122  		if err != nil {
 1123  			logger.L().Error(ctx, "error while waiting for template manager to drain", zap.Error(err))
 1124  			success = false
 1125  		}
 1126  	}
 1127  
 1128  	// Gracefully wait for live sandboxes to exit before closing the services they
 1129  	// depend on. The forced-stop path skips this and tears sandboxes down later.
 1130  	if !config.ForceStop {
 1131  		logger.L().Info(ctx, "Starting sandbox drain phase", zap.Int("sandbox_count", sandboxes.Count()))
 1132  		if err := orchestratorService.DrainSandboxes(closeCtx); err != nil {
 1133  			logger.L().Error(ctx, "error while draining sandboxes", zap.Error(err))
 1134  			success = false
 1135  		}
 1136  	}
 1137  
 1138  	slices.Reverse(closers)
 1139  	for _, closer := range closers {
 1140  		clog := globalLogger.With(zap.String("service", closer.name), zap.Bool("forced", config.ForceStop))
 1141  		clog.Info(ctx, "closing")
 1142  		if err := closer.close(closeCtx); err != nil {
 1143  			clog.Error(ctx, "error during shutdown", zap.Error(err))
 1144  			success = false
 1145  		}
 1146  	}
 1147  
 1148  	logger.L().Info(ctx, "Waiting for services to finish")
 1149  	var sde serviceDoneError
 1150  	if err := g.Wait(); err != nil && !errors.As(err, &sde) {
 1151  		logger.L().Error(ctx, "service group error", zap.Error(err))
 1152  		success = false
 1153  	}
 1154  
 1155  	return success
 1156  }
````

`packages/orchestrator/pkg/server/main.go` L44-L63:

````go
   44  // uploadDrainLogInterval is how often Close logs progress while waiting for
   45  // in-flight snapshot uploads to finish during shutdown.
   46  const uploadDrainLogInterval = 10 * time.Second
   47  
   48  // sandboxDrainPollInterval is how often the graceful sandbox drain re-checks
   49  // the live sandbox count during shutdown.
   50  const sandboxDrainPollInterval = 5 * time.Second
   51  
   52  // sandboxDrainLogInterval backs off how often the graceful sandbox drain logs
   53  // progress so a long-lived node draining for hours does not spam the logs.
   54  func sandboxDrainLogInterval(elapsed time.Duration) time.Duration {
   55  	switch {
   56  	case elapsed < time.Minute:
   57  		return 5 * time.Second
   58  	case elapsed < time.Hour:
   59  		return time.Minute
   60  	default:
   61  		return 15 * time.Minute
   62  	}
   63  }
````

`packages/orchestrator/pkg/server/main.go` L336-L437:

````go
  336  func (s *Server) Close(ctx context.Context) error {
  337  	s.closeOnce.Do(func() {
  338  		close(s.done)
  339  	})
  340  
  341  	// Wait for in-flight snapshot uploads to finish so a graceful shutdown
  342  	// doesn't drop a snapshot that is still uploading. ctx is cancelled on a
  343  	// forced stop, in which case we stop waiting and let the process exit.
  344  	uploadsDone := make(chan struct{})
  345  	go func() {
  346  		s.uploadsWG.Wait()
  347  		close(uploadsDone)
  348  	}()
  349  
  350  	s.drainUploads(ctx, uploadsDone)
  351  
  352  	s.uploadedBuilds.Stop()
  353  
  354  	return nil
  355  }
  356  
  357  // drainUploads waits for in-flight snapshot uploads to finish, logging progress
  358  // periodically, until they complete or ctx is cancelled (forced stop).
  359  func (s *Server) drainUploads(ctx context.Context, uploadsDone <-chan struct{}) {
  360  	inFlight := s.uploadsInFlight.Load()
  361  	if inFlight == 0 {
  362  		return
  363  	}
  364  
  365  	logger.L().Info(ctx, "waiting for in-flight snapshot uploads to finish", zap.Int64("uploads", inFlight))
  366  
  367  	ticker := time.NewTicker(uploadDrainLogInterval)
  368  	defer ticker.Stop()
  369  
  370  	for {
  371  		select {
  372  		case <-uploadsDone:
  373  			logger.L().Info(ctx, "all in-flight snapshot uploads finished")
  374  
  375  			return
  376  		case <-ctx.Done():
  377  			logger.L().Warn(ctx, "shutting down with snapshot uploads still in flight",
  378  				zap.Int64("uploads", s.uploadsInFlight.Load()),
  379  				zap.Error(context.Cause(ctx)),
  380  			)
  381  
  382  			return
  383  		case <-ticker.C:
  384  			logger.L().Info(ctx, "still waiting for in-flight snapshot uploads",
  385  				zap.Int64("uploads", s.uploadsInFlight.Load()),
  386  			)
  387  		}
  388  	}
  389  }
  390  
  391  // DrainSandboxes waits for the live sandboxes on this node to exit on their own
  392  // during a graceful shutdown, then waits for their lifecycle cleanup to finish.
  393  // It does not reject new sandbox starts; that admission gating is layered in
  394  // separately. It returns ctx.Err() if ctx is cancelled before the node empties.
  395  func (s *Server) DrainSandboxes(ctx context.Context) error {
  396  	live := s.sandboxFactory.Sandboxes.Count()
  397  	logger.L().Info(ctx, "starting graceful sandbox drain", zap.Int("live_sandboxes", live))
  398  
  399  	ticker := time.NewTicker(sandboxDrainPollInterval)
  400  	defer ticker.Stop()
  401  	startedAt := time.Now()
  402  	lastLoggedAt := startedAt
  403  
  404  	for {
  405  		remaining := s.sandboxFactory.Sandboxes.Count()
  406  		if remaining == 0 {
  407  			logger.L().Info(ctx, "graceful sandbox drain complete", zap.Int("live_sandboxes", remaining))
  408  
  409  			return s.waitSandboxLifecycles(ctx)
  410  		}
  411  
  412  		select {
  413  		case <-ctx.Done():
  414  			logger.L().Warn(ctx, "graceful sandbox drain timed out",
  415  				zap.Int("remaining_sandboxes", remaining),
  416  				zap.Error(ctx.Err()),
  417  			)
  418  
  419  			return ctx.Err()
  420  		case <-ticker.C:
  421  			now := time.Now()
  422  			remaining = s.sandboxFactory.Sandboxes.Count()
  423  			elapsed := now.Sub(startedAt)
  424  			if remaining > 0 && now.Sub(lastLoggedAt) >= sandboxDrainLogInterval(elapsed) {
  425  				logger.L().Info(ctx, "waiting for sandbox drain",
  426  					zap.Int("remaining_sandboxes", remaining),
  427  					zap.Duration("elapsed", elapsed),
  428  				)
  429  				lastLoggedAt = now
  430  			}
  431  		}
  432  	}
  433  }
  434  
  435  func (s *Server) waitSandboxLifecycles(ctx context.Context) error {
  436  	return s.sandboxFactory.Sandboxes.WaitLifecycles(ctx)
  437  }
````

`packages/orchestrator/pkg/template/server/main.go` L196-L218:

````go
  196  // Wait gracefully drains in-flight template builds during shutdown. It waits
  197  // for running builds to finish, bounded by ctx, then gives consumers a grace
  198  // period to read the final build status. It returns ctx.Err() if ctx is
  199  // cancelled before the drain completes.
  200  func (s *ServerStore) Wait(ctx context.Context) error {
  201  	s.logger.Info(ctx, "Waiting for all build jobs to finish", zap.Int64("active_builds", s.activeBuilds.Load()))
  202  	if err := utils.WaitGroupWait(ctx, s.wg); err != nil {
  203  		return fmt.Errorf("waiting for template builds: %w", err)
  204  	}
  205  
  206  	if !env.IsLocal() {
  207  		s.logger.Info(ctx, "Waiting for consumers to check build status")
  208  		select {
  209  		case <-time.After(consumerStatusCheckGracePeriod):
  210  		case <-ctx.Done():
  211  			return ctx.Err()
  212  		}
  213  	}
  214  
  215  	s.logger.Info(ctx, "Template build queue cleaned")
  216  
  217  	return nil
  218  }
````

`packages/orchestrator/pkg/template/server/main.go` L43-L43:

````go
   43  const consumerStatusCheckGracePeriod = 15 * time.Second
````

Behaviour:
- SIGINT/SIGTERM/SIGUSR1 -> status ShuttingDown -> **15 s sleep unless `ENVIRONMENT=local`**
  (L1115-L1117) -> template-manager `Wait` (plus another 15 s `consumerStatusCheckGracePeriod` unless
  local) -> `DrainSandboxes(closeCtx)` which polls every 5 s **with no deadline** until every live sandbox
  has ended on its own (end_time) -> closers in reverse order (uploads drain in `Server.Close`, network pool
  cleanup, ...).
- `FORCE_STOP=true` cancels `closeCtx` up front (L1107-L1109) and skips the drain (L1130). Nothing in the
  closers kills live Firecracker processes (network `Pool.Close` only cleans *unused* pooled slots), so
  those VMs keep running after exit and are reclaimed by P1 on the next start.

**Minimal change point:** replace L1114-L1117

```go
	// Consumers must stop assigning work before services drain.
	if !env.IsLocal() {
		time.Sleep(15 * time.Second)
	}
```

with a configurable duration, e.g. a new field
`` ShutdownAdmissionGrace time.Duration `env:"SHUTDOWN_ADMISSION_GRACE" envDefault:"15s"` `` in `cfg.Config` (model.go L78-L108) and
`time.Sleep(config.ShutdownAdmissionGrace)` (0 = no sleep). Do the same for
`consumerStatusCheckGracePeriod` in template/server/main.go L206-L213 if the template-manager service is
enabled. Setting `ENVIRONMENT=local` avoids both sleeps without code but has side effects (E1). For a
bounded drain, either wrap `closeCtx` for the `DrainSandboxes` call in `context.WithTimeout` from a new
env (e.g. `SANDBOX_DRAIN_TIMEOUT`), or keep the Embed approach of killing `/sys/fs/cgroup/e2b/*/cgroup.kill`
before sending SIGTERM (orchestrator-launch.sh L20-L32).

## P3. Exposing the slot HostIP in List and Create

List (the only place `RunningSandbox` is built; build sandboxes are filtered out at L667-L672):

`packages/orchestrator/pkg/server/sandboxes.go` L654-L691:

````go
  654  func (s *Server) List(ctx context.Context, _ *emptypb.Empty) (*orchestrator.SandboxListResponse, error) {
  655  	_, childSpan := tracer.Start(ctx, "sandbox-list")
  656  	defer childSpan.End()
  657  
  658  	items := s.sandboxFactory.Sandboxes.Items()
  659  
  660  	sandboxes := make([]*orchestrator.RunningSandbox, 0, len(items))
  661  
  662  	for _, sbx := range items {
  663  		if sbx == nil {
  664  			continue
  665  		}
  666  
  667  		// Build sandboxes are not owned by the API and must never show up here,
  668  		// or the API would treat them as orphans and kill them. They are the only
  669  		// sandboxes created without an APIStoredConfig.
  670  		if sbx.APIStoredConfig == nil {
  671  			continue
  672  		}
  673  
  674  		startedAt := sbx.GetStartedAt()
  675  		sandboxes = append(sandboxes, &orchestrator.RunningSandbox{
  676  			Config:      sbx.APIStoredConfig,
  677  			ClientId:    s.info.ClientId,
  678  			StartTime:   timestamppb.New(startedAt),
  679  			EndTime:     timestamppb.New(sbx.GetEndAt()),
  680  			SandboxId:   sbx.Runtime.SandboxID,
  681  			TeamId:      sbx.Runtime.TeamID,
  682  			ExecutionId: sbx.Runtime.ExecutionID,
  683  			Vcpu:        sbx.Config.Vcpu,
  684  			RamMb:       sbx.Config.RamMB,
  685  		})
  686  	}
  687  
  688  	return &orchestrator.SandboxListResponse{
  689  		Sandboxes: sandboxes,
  690  	}, nil
  691  }
````
Create (response built at L443-L452; `sbx` is in scope):

`packages/orchestrator/pkg/server/sandboxes.go` L127-L130:

````go
  127  func (s *Server) Create(ctx context.Context, req *orchestrator.SandboxCreateRequest) (_ *orchestrator.SandboxCreateResponse, createErr error) {
  128  	releaseWork := s.info.TrackWork()
  129  	defer releaseWork()
  130  
````

`packages/orchestrator/pkg/server/sandboxes.go` L214-L246:

````go
  214  	reservation, err := s.sandboxFactory.Sandboxes.Reserve(req.GetSandbox().GetSandboxId())
  215  	if err != nil {
  216  		return nil, s.sandboxAlreadyRunning(ctx, req.GetSandbox().GetSandboxId(), req.GetSandbox().GetExecutionId(), err)
  217  	}
  218  	var rollback *sandbox.Cleanup
  219  	defer func() {
  220  		s.finishSandboxStart(ctx, reservation, rollback, createErr)
  221  	}()
  222  
  223  	maxRunningSandboxesPerNode := s.info.MaxSandboxes.Load()
  224  
  225  	runningSandboxes := int64(s.sandboxFactory.Sandboxes.Count())
  226  	if runningSandboxes >= maxRunningSandboxesPerNode {
  227  		telemetry.ReportEvent(ctx, "max number of running sandboxes reached")
  228  
  229  		return nil, status.Errorf(codes.ResourceExhausted, "max number of running sandboxes on node reached (%d), please retry", maxRunningSandboxesPerNode)
  230  	}
  231  
  232  	// Check if we've reached the max number of starting instances on this node
  233  	if req.GetSandbox().GetSnapshot() {
  234  		err := s.waitForAcquire(ctx)
  235  		if err != nil {
  236  			return nil, err
  237  		}
  238  	} else {
  239  		acquired := s.startingSandboxes.TryAcquire(1)
  240  		if !acquired {
  241  			telemetry.ReportEvent(ctx, "too many starting sandboxes on node")
  242  
  243  			return nil, status.Errorf(codes.ResourceExhausted, "too many sandboxes starting on this node, please retry")
  244  		}
  245  	}
  246  	defer s.startingSandboxes.Release(1)
````

`packages/orchestrator/pkg/server/sandboxes.go` L327-L453:

````go
  327  	var sbx *sandbox.Sandbox
  328  	if filesystemBooted {
  329  		sbx, err = s.sandboxFactory.RebootSandbox(
  330  			ctx,
  331  			template,
  332  			config,
  333  			runtime,
  334  			req.GetEndTime().AsTime(),
  335  			req.GetSandbox(),
  336  			// Defer routing until after the resume-time envd upgrade's
  337  			// post-/init, so the sandbox isn't reachable during its pre-init
  338  			// auth window. Promoted below via markSandboxLive.
  339  			true,
  340  			req.GetFilesystemBoot(),
  341  			func(o rootfs.RecoverOutcome) { fsRecovery = o },
  342  		)
  343  	} else {
  344  		sbx, err = s.sandboxFactory.ResumeSandbox(
  345  			ctx,
  346  			template,
  347  			config,
  348  			runtime,
  349  			req.GetStartTime().AsTime(),
  350  			req.GetEndTime().AsTime(),
  351  			req.GetSandbox(),
  352  			// Defer routing until after the resume-time envd upgrade's
  353  			// post-/init (see markSandboxLive below).
  354  			sandbox.WithDeferredLiveRegistration(),
  355  		)
  356  	}
  357  	if err != nil {
  358  		if errors.Is(err, storage.ErrObjectNotExist) {
  359  			// Snapshot data not found, let the API know the data aren't probably upload yet
  360  			telemetry.ReportError(ctx, "sandbox files not found", err, telemetry.WithSandboxID(req.GetSandbox().GetSandboxId()))
  361  
  362  			return nil, status.Errorf(codes.FailedPrecondition, "sandbox files for '%s' not found", req.GetSandbox().GetSandboxId())
  363  		}
  364  
  365  		err = errors.Join(err, context.Cause(ctx))
  366  		telemetry.ReportCriticalError(ctx, "failed to create sandbox", err)
  367  		logger.L().Error(ctx, "failed to create sandbox", zap.Error(err),
  368  			zap.Bool("filesystem_boot_requested", req.GetFilesystemBoot()),
  369  			logger.WithSandboxID(runtime.SandboxID),
  370  			logger.WithBuildID(runtime.BuildID),
  371  			logger.WithTemplateID(runtime.TemplateID),
  372  			logger.WithEnvdVersion(config.Envd.Version),
  373  			logger.WithKernelVersion(config.FirecrackerConfig.KernelVersion),
  374  			logger.WithFirecrackerVersion(config.FirecrackerConfig.FirecrackerVersion),
  375  		)
  376  
  377  		return nil, status.Errorf(codes.Internal, "failed to create sandbox: %s", err)
  378  	}
  379  
  380  	rollback.Add(ctx, func(ctx context.Context) error { return stopAndCloseSandbox(ctx, sbx) })
  381  	s.setupSandboxLifecycle(ctx, sbx, releaseTemplate)
  382  
  383  	// Resume-time envd live-upgrade. The API /resume maps to Create
  384  	// with snapshot=true, so this is the real resume path. Flag-driven,
  385  	// best-effort + recover-wrapped (see maybeUpgradeEnvd) so it can't disrupt
  386  	// resume. ctx already carries the LD context (envd-version/team/template).
  387  	if req.GetSandbox().GetSnapshot() {
  388  		var upErr error
  389  		envdUpgraded, upErr = s.maybeUpgradeEnvd(ctx, sbx)
  390  		if upErr != nil {
  391  			sbx.SetStopReason(sandbox.StopReasonKilled)
  392  
  393  			return nil, upErr
  394  		}
  395  	}
  396  
  397  	// Promote to the live registry only now — after any resume-time envd upgrade
  398  	// has run its post-/init and restored the access token — so the sandbox is
  399  	// never routable during the upgrade's sub-second pre-init auth window. Both
  400  	// the resume and reboot paths above defer this.
  401  	if err := s.markSandboxLive(ctx, sbx, reservation); err != nil {
  402  		return nil, status.Errorf(codes.Internal, "failed to register sandbox: %s", err)
  403  	}
  404  	// Read off the start path; unknown here means the read has not landed yet.
  405  	childSpan.SetAttributes(attribute.String("balloon_mode", sbx.BalloonMode()))
  406  
  407  	// Read scheduling metadata after the sandbox resumed so the template's
  408  	// memfile/rootfs devices (and their headers) are resolved.
  409  	var schedulingMetadata *orchestrator.SchedulingMetadata
  410  	if provider, ok := template.(interface {
  411  		SchedulingMetadata(ctx context.Context) *orchestrator.SchedulingMetadata
  412  	}); ok {
  413  		schedulingMetadata = provider.SchedulingMetadata(ctx)
  414  	}
  415  
  416  	eventType := events.SandboxCreatedEventPair
  417  	if req.GetSandbox().GetSnapshot() {
  418  		eventType = events.SandboxResumedEventPair
  419  	}
  420  
  421  	teamID, buildId, eventsTTLDays, eventData := s.prepareSandboxEventData(ctx, sbx)
  422  	s.publishEventAsync(
  423  		ctx,
  424  		teamID,
  425  		events.SandboxEvent{
  426  			Version:   events.StructureVersionV2,
  427  			ID:        uuid.New(),
  428  			Type:      eventType.Type,
  429  			Timestamp: time.Now().UTC(),
  430  
  431  			EventData:          eventData,
  432  			SandboxID:          sbx.Runtime.SandboxID,
  433  			SandboxExecutionID: sbx.Runtime.ExecutionID,
  434  			SandboxTemplateID:  sbx.Config.BaseTemplateID,
  435  			SandboxBuildID:     buildId,
  436  			SandboxTeamID:      teamID,
  437  			EventsTTLDays:      eventsTTLDays,
  438  		},
  439  	)
  440  
  441  	sbx.SetExecutionStartedAt(time.Now())
  442  
  443  	return &orchestrator.SandboxCreateResponse{
  444  		ClientId:              s.info.ClientId,
  445  		SchedulingMetadata:    schedulingMetadata,
  446  		FilesystemBootApplied: filesystemBooted,
  447  		// The resolved Firecracker version the sandbox actually runs, frozen
  448  		// for its lifetime. The API stores it so a version-gated feature can
  449  		// key off the running binary exactly instead of re-resolving the
  450  		// flag, which drifts from this frozen value whenever the flag moves.
  451  		ResolvedFirecrackerVersion: resolvedFCVersion,
  452  	}, nil
  453  }
````
Where the slot lives on the sandbox and how the HostIP is obtained:

`packages/orchestrator/pkg/sandbox/sandbox.go` L245-L249:

````go
  245  type Resources struct {
  246  	Slot   *network.Slot
  247  	rootfs rootfs.Provider
  248  	memory uffd.MemoryBackend
  249  }
````

`packages/orchestrator/pkg/sandbox/sandbox.go` L290-L292:

````go
  290  type Sandbox struct {
  291  	*Resources
  292  	*Metadata
````

`packages/orchestrator/pkg/sandbox/network/slot.go` L50-L95:

````go
   50  // Slot network allocation
   51  //
   52  // For each slot, we allocate three IP addresses:
   53  // Host IP - used to access the sandbox from the host machine
   54  // Vpeer and Veth IPs - used by the sandbox to communicate with the host
   55  //
   56  // Host default namespace creates a /16 CIDR block for the host IPs.
   57  // Slot with Idx 1 will receive 10.11.0.1 and so on. Its allocated incrementally by slot Idx.
   58  // Host mask is /32 because we only use one IP per slot.
   59  //
   60  // Vrt addresses (vpeer and veth) are allocated from a /31 CIDR block so we can use CIDR for network link routing.
   61  // By default, they are using 10.12.0.0/16 CIDR block, that can be configured via environment variable.
   62  // Vpeer receives the first IP in the block, and Veth receives the second IP. Block is calculated as (slot index * addresses per slot allocation).
   63  // Vrt address per slot is always 2, so we can allocate /31 CIDR block for each slot.
   64  type Slot struct {
   65  	Key string
   66  	Idx int
   67  
   68  	Firewall *Firewall
   69  
   70  	// firewallCustomRules is used to track if custom firewall rules are set for the slot and need a cleanup.
   71  	firewallCustomRules atomic.Bool
   72  
   73  	// egressDSCP is the class the slot's rule currently stamps (0 = no rule).
   74  	// Not a synchronization point: callers need exclusive slot ownership,
   75  	// which the pool guarantees between Get and the recycle give-back.
   76  	egressDSCP atomic.Uint32
   77  
   78  	vPeerIp net.IP
   79  	vEthIp  net.IP
   80  	vrtMask net.IPMask
   81  
   82  	tapIp   net.IP
   83  	tapMask net.IPMask
   84  
   85  	// HostIP is IP address for the sandbox from the host machine.
   86  	// You can use it to make requests to the sandbox.
   87  	HostIP   net.IP
   88  	hostNet  *net.IPNet
   89  	hostCIDR string
   90  
   91  	hyperloopPort string
   92  
   93  	egressProxy EgressProxy
   94  	config      Config
   95  }
````

`packages/orchestrator/pkg/sandbox/network/slot.go` L179-L201:

````go
  179  func (s *Slot) HostIPString() string {
  180  	return s.HostIP.String()
  181  }
  182  
  183  func (s *Slot) HostMask() net.IPMask {
  184  	return s.hostNet.Mask
  185  }
  186  
  187  func (s *Slot) HostNet() *net.IPNet {
  188  	return s.hostNet
  189  }
  190  
  191  func (s *Slot) HostCIDR() string {
  192  	return s.hostCIDR
  193  }
  194  
  195  func (s *Slot) NamespaceIP() string {
  196  	return "169.254.0.21"
  197  }
  198  
  199  func (s *Slot) NamespaceID() string {
  200  	return fmt.Sprintf("ns-%d", s.Idx)
  201  }
````

`packages/orchestrator/pkg/sandbox/map.go` L135-L159:

````go
  135  // GetByHostPort looks up a sandbox by its host IP address parsed from hostPort.
  136  func (m *Map) GetByHostPort(hostPort string) (*Sandbox, error) {
  137  	reqIP, _, err := net.SplitHostPort(hostPort)
  138  	if err != nil {
  139  		return nil, fmt.Errorf("error parsing remote address %s: %w", hostPort, err)
  140  	}
  141  
  142  	sbx, ok := m.network.Get(reqIP)
  143  	if !ok {
  144  		return nil, errors.New("sandbox not found")
  145  	}
  146  
  147  	return sbx, nil
  148  }
  149  
  150  // AssignNetwork registers a sandbox's IP so it is findable by GetByHostPort.
  151  func (m *Map) AssignNetwork(ctx context.Context, sbx *Sandbox) {
  152  	ip := sbx.Slot.HostIPString()
  153  	m.network.Insert(ip, sbx)
  154  
  155  	sbx.log().Info(ctx, "sandbox network map entry added",
  156  		logger.WithLifecycleID(sbx.LifecycleID),
  157  		logger.WithSandboxIP(ip),
  158  	)
  159  }
````

`sbx.Slot.HostIPString()` returns `10.11.0.<idx>`-style addresses (host CIDR indexed by slot idx; slot.go
L118-L127). From the host, `HostIP:<port>` reaches the guest (route `HostIP/32 via vpeer` + DNAT
`HostIP -> 169.254.0.21` inside the netns, network.go L270, L298-L305); envd is `HostIP:49983`.

**Minimal change:** in `packages/orchestrator/orchestrator.proto` add
`string host_ip = 10;` to `RunningSandbox` (next free tag after `ram_mb = 9`) and
`string host_ip = 5;` to `SandboxCreateResponse` (next free after `resolved_firecracker_version = 4`);
regenerate `packages/shared/pkg/grpc/orchestrator/orchestrator.pb.go` (+ `_grpc.pb.go`) with the repo's
generator (`make generate` / `go generate ./...` in packages/orchestrator); set
`HostIp: sbx.Slot.HostIPString()` at sandboxes.go L675-L685 and L443-L452. `sbx.Slot` is non-nil for
every sandbox in `Items()` that reached AssignNetwork; guard with `if sbx.Slot != nil` for safety.

Full current proto for reference:

`packages/orchestrator/orchestrator.proto` (full file, 265 lines):

````proto
    1  syntax = "proto3";
    2  
    3  import "google/protobuf/empty.proto";
    4  import "google/protobuf/timestamp.proto";
    5  
    6  option go_package = "https://github.com/e2b-dev/infra/orchestrator";
    7  
    8  message SandboxConfig {
    9    // Data required for creating a new sandbox.
   10    string template_id = 1;
   11    string build_id = 2;
   12  
   13    string kernel_version = 3;
   14    string firecracker_version = 4;
   15  
   16    bool huge_pages = 5;
   17  
   18    string sandbox_id = 6;
   19    map<string, string> env_vars = 7;
   20  
   21    // Metadata about the sandbox.
   22    map<string, string> metadata = 8;
   23    optional string alias = 9;
   24    string envd_version = 10;
   25  
   26    int64 vcpu = 11;
   27    int64 ram_mb = 12;
   28  
   29    string team_id = 13;
   30    // Maximum length of the sandbox in Hours.
   31    int64 max_sandbox_length = 14;
   32  
   33    int64 total_disk_size_mb = 15;
   34  
   35    bool snapshot = 16;
   36    string base_template_id = 17;
   37  
   38    bool auto_pause = 18;
   39  
   40    optional string envd_access_token = 19;
   41    string execution_id = 20;
   42  
   43    // Whether the sandbox should have access to the internet.
   44    // This is optional only for backwards compatibility.
   45    // After migration, the optional keyword can be removed.
   46    optional bool allow_internet_access = 21;
   47  
   48    optional SandboxNetworkConfig network = 22;
   49  
   50    repeated SandboxVolumeMount volumeMounts = 23;
   51  
   52    // Auto-resume policy for paused sandboxes.
   53    optional SandboxAutoResumeConfig auto_resume = 24;
   54  
   55    // When true, a timeout auto-pause takes a filesystem-only snapshot (no
   56    // memory) instead of a full memory snapshot. Only meaningful when auto_pause
   57    // is set. Passed through so the policy survives an API re-sync from the
   58    // orchestrator's sandbox list (mirrors auto_pause); the orchestrator itself
   59    // does not act on it — the API evictor does.
   60    bool auto_pause_filesystem_only = 25;
   61  
   62    // Retention of sandbox events in days
   63    int64 events_ttl_days = 26;
   64  
   65    // Sandbox workload identity configuration requested at create time. Absent on
   66    // older serialized configs, which decode as no workload identity.
   67    optional SandboxIam iam = 27;
   68  }
   69  
   70  // Sandbox workload identity configuration. A non-empty tokens map defines the
   71  // sandbox workload identity, which the orchestrator derives from the existing
   72  // trusted team_id, sandbox_id, execution_id and template_id fields. No
   73  // credential is minted, signed, or delivered here.
   74  message SandboxIam {
   75    // Named workload-token definitions, keyed by a caller-chosen token name.
   76    map<string, SandboxIamToken> tokens = 1;
   77  }
   78  
   79  // A named workload-token definition. file_path is intentionally omitted: only
   80  // absent/null is accepted at admission, so file delivery is not represented.
   81  message SandboxIamToken {
   82    string audience = 1;
   83    string token_type = 2;
   84  }
   85  
   86  message SandboxAutoResumeConfig {
   87    // Policy values are owned by the API layer today (e.g. "off", "any").
   88    string policy = 1;
   89    // Timeout requested on initial sandbox create (seconds).
   90    uint64 timeout_seconds = 2;
   91  }
   92  
   93  message SandboxVolumeMount {
   94    string id = 1;
   95    string path = 2;
   96    string type = 3;
   97    string name = 4;
   98  }
   99  
  100  message SandboxNetworkConfig {
  101    optional SandboxNetworkEgressConfig egress = 1;
  102    optional SandboxNetworkIngressConfig ingress = 2;
  103  }
  104  
  105  message SandboxNetworkTransform {
  106    map<string, string> headers = 1;
  107  }
  108  
  109  message SandboxNetworkRule {
  110    optional SandboxNetworkTransform transform = 1;
  111  }
  112  
  113  message SandboxNetworkDomainRules {
  114    repeated SandboxNetworkRule rules = 1;
  115  }
  116  
  117  message SandboxNetworkEgressConfig {
  118    repeated string allowed_cidrs = 1;
  119    repeated string denied_cidrs = 2;
  120    repeated string allowed_domains = 3;
  121    map<string, SandboxNetworkDomainRules> rules = 4;
  122  
  123    // BYOP SOCKS5 egress proxy.
  124    string egress_proxy_address = 5;
  125    string egress_proxy_username = 6;
  126    string egress_proxy_password = 7;
  127  }
  128  
  129  message SandboxNetworkIngressConfig {
  130    optional string traffic_access_token = 1;
  131    optional string mask_request_host = 2;
  132    repeated uint32 https_ports = 3;
  133  }
  134  
  135  message SandboxCreateRequest {
  136    SandboxConfig sandbox = 1;
  137  
  138    google.protobuf.Timestamp start_time = 2;
  139    google.protobuf.Timestamp end_time = 3;
  140  
  141    // When true, resume by cold-booting from the snapshot's rootfs even when it
  142    // includes a memory snapshot. The request can only widen toward the no-memory
  143    // path — it can never force a memory restore of a snapshot that has none.
  144    // Absent = the snapshot's own metadata alone selects the boot path, so
  145    // existing callers are unaffected. Request-scoped: nothing durable records it.
  146    optional bool filesystem_boot = 4;
  147  }
  148  
  149  message SandboxCreateResponse {
  150    string client_id = 1;
  151    SchedulingMetadata scheduling_metadata = 2;
  152  
  153    // Echoes whether the sandbox cold-booted from its rootfs (vs a memory
  154    // restore). Absent from orchestrators that predate filesystem_boot, so a
  155    // caller that demanded a filesystem boot can detect an unhonored demand
  156    // instead of trusting deploy ordering.
  157    bool filesystem_boot_applied = 3;
  158  
  159    // The Firecracker version the sandbox actually RUNS: the request's
  160    // declared version resolved through the firecracker-versions flag at
  161    // start, frozen for the sandbox's lifetime. Callers gating features on the
  162    // FC version must read this rather than re-deriving it — a re-resolution
  163    // can disagree with the frozen value whenever the flag changes or the
  164    // evaluation contexts differ. Empty from orchestrators that predate the
  165    // field.
  166    string resolved_firecracker_version = 4;
  167  }
  168  
  169  message SandboxUpdateRequest {
  170    string sandbox_id = 1;
  171  
  172    // All fields are optional — only set fields are applied.
  173    optional google.protobuf.Timestamp end_time = 2;
  174    optional SandboxNetworkEgressConfig egress = 3;
  175  }
  176  
  177  message SandboxDeleteRequest {
  178    string sandbox_id = 1;
  179    optional string kill_reason = 2;
  180  }
  181  
  182  message SandboxPauseRequest {
  183    string sandbox_id = 1;
  184    string template_id = 2;
  185    string build_id = 3;
  186  
  187    // When true, persist only the filesystem (no memory snapshot); resuming such
  188    // a snapshot cold-boots (reboots) from the rootfs. Default false = full memory
  189    // snapshot, so existing callers are unaffected.
  190    bool filesystem_only = 4;
  191  }
  192  
  193  message SchedulingMetadata {
  194    // memfile_base_build_id / rootfs_base_build_id are each artifact's root layer
  195    // (shared across the template's sandboxes); they can differ. build_id is the
  196    // final/current layer. All also appear in the lists below.
  197    string memfile_base_build_id = 1;
  198    string build_id = 2;
  199    // Deduplicated build IDs whose data each artifact references (all ancestor
  200    // layers plus the build itself). Sorted; order is not significant. When a
  201    // list exceeds the cap, the lightest layers are dropped first and the count
  202    // of dropped layers is reported.
  203    repeated string memfile_build_ids = 3;
  204    repeated string rootfs_build_ids = 4;
  205    uint32 memfile_dropped_builds = 5;
  206    uint32 rootfs_dropped_builds = 6;
  207    // Referenced bytes per build, aligned with the *_build_ids lists. On save the
  208    // new memfile layer's bytes are a pre-dedup, block-granular upper bound.
  209    repeated uint64 memfile_build_bytes = 7;
  210    repeated uint64 rootfs_build_bytes = 8;
  211    string rootfs_base_build_id = 9;
  212  }
  213  
  214  message SandboxPauseResponse {
  215    SchedulingMetadata scheduling_metadata = 1;
  216  }
  217  
  218  message SandboxCheckpointRequest {
  219    string sandbox_id = 1;
  220    string build_id = 3;
  221    // Provenance stamped onto the snapshot's storage objects for the storage
  222    // index (e.g. template_id). Opaque to the orchestrator, which just forwards
  223    // it to object metadata.
  224    map<string, string> metadata = 4;
  225  }
  226  
  227  message SandboxCheckpointResponse {
  228    SchedulingMetadata scheduling_metadata = 1;
  229  }
  230  
  231  message RunningSandbox {
  232    // Deprecated: the API no longer rebuilds sandbox state from this list. Redis
  233    // is the source of truth; List is only used to detect sandboxes running on a
  234    // node that the store does not know about, so they can be killed. The fields
  235    // below carry everything that decision needs. Still populated so API
  236    // instances predating those fields keep working during a rollout.
  237    SandboxConfig config = 1 [deprecated = true];
  238    string client_id = 2;
  239  
  240    google.protobuf.Timestamp start_time = 3;
  241    google.protobuf.Timestamp end_time = 4;
  242  
  243    // Minimal set required to detect an orphaned sandbox and kill it.
  244    // sandbox_id + team_id form the store key. execution_id is carried in the
  245    // edge sandbox-catalog delete event, so a cluster node's routing entry is
  246    // evicted too. vcpu/ram_mb feed the node's optimistic resource accounting.
  247    string sandbox_id = 5;
  248    string team_id = 6;
  249    string execution_id = 7;
  250    int64 vcpu = 8;
  251    int64 ram_mb = 9;
  252  }
  253  
  254  message SandboxListResponse {
  255    repeated RunningSandbox sandboxes = 1;
  256  }
  257  
  258  service SandboxService {
  259    rpc Create(SandboxCreateRequest) returns (SandboxCreateResponse);
  260    rpc Update(SandboxUpdateRequest) returns (google.protobuf.Empty);
  261    rpc List(google.protobuf.Empty) returns (SandboxListResponse);
  262    rpc Delete(SandboxDeleteRequest) returns (google.protobuf.Empty);
  263    rpc Pause(SandboxPauseRequest) returns (SandboxPauseResponse);
  264    rpc Checkpoint(SandboxCheckpointRequest) returns (SandboxCheckpointResponse);
  265  }
````

## P4. Private-range floor and per-sandbox allowed private CIDRs

### P4.1 The always-deny list (shared)

`packages/shared/pkg/sandbox-network/firewall.go` L16-L51:

````go
   16  const (
   17  	AllInternetTrafficCIDR = "0.0.0.0/0"
   18  
   19  	DefaultNameserver = "8.8.8.8"
   20  )
   21  
   22  var DeniedSandboxCIDRs = []string{
   23  	// IPv4 private/local ranges
   24  	"10.0.0.0/8",     // RFC 1918 private.
   25  	"100.64.0.0/10",  // RFC 6598 CGNAT / shared address space; used by some cloud providers for internal services.
   26  	"127.0.0.0/8",    // RFC 1122 loopback.
   27  	"169.254.0.0/16", // RFC 3927 link-local (incl. cloud metadata 169.254.169.254).
   28  	"172.16.0.0/12",  // RFC 1918 private.
   29  	"192.168.0.0/16", // RFC 1918 private.
   30  	// IPv6 local ranges
   31  	"::1/128",   // RFC 4291 loopback.
   32  	"fc00::/7",  // RFC 4193 unique local.
   33  	"fe80::/10", // RFC 4291 link-local.
   34  }
   35  
   36  var DeniedSandboxSetData = utils.Must(set.AddressStringsToSetData(DeniedSandboxCIDRs))
   37  
   38  // parsedDeniedSandboxCIDRs is DeniedSandboxCIDRs pre-parsed for
   39  // IsIPInDeniedSandboxCIDRs.
   40  var parsedDeniedSandboxCIDRs = func() []*net.IPNet {
   41  	out := make([]*net.IPNet, 0, len(DeniedSandboxCIDRs))
   42  	for _, c := range DeniedSandboxCIDRs {
   43  		_, ipNet, err := net.ParseCIDR(c)
   44  		if err != nil {
   45  			panic(fmt.Sprintf("sandbox_network: invalid CIDR in DeniedSandboxCIDRs: %q: %v", c, err))
   46  		}
   47  		out = append(out, ipNet)
   48  	}
   49  
   50  	return out
   51  }()
````

### P4.2 Layer 1 — nftables inside each sandbox netns (v1)

`packages/orchestrator/pkg/sandbox/network/firewall.go` (full file, 519 lines):

````go
    1  //go:build linux
    2  
    3  package network
    4  
    5  import (
    6  	"context"
    7  	"errors"
    8  	"fmt"
    9  	"net/netip"
   10  	"sync"
   11  
   12  	"github.com/gaissmai/extnetip"
   13  	"github.com/google/nftables"
   14  	"github.com/google/nftables/binaryutil"
   15  	"github.com/google/nftables/expr"
   16  	"github.com/ngrok/firewall_toolkit/pkg/expressions"
   17  	"github.com/ngrok/firewall_toolkit/pkg/set"
   18  	"go.uber.org/zap"
   19  	"golang.org/x/sys/unix"
   20  
   21  	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
   22  	sandbox_network "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-network"
   23  )
   24  
   25  const tableName = "slot-firewall"
   26  
   27  type Firewall struct {
   28  	// mu serializes the shared conn buffer, which is committed on Flush().
   29  	mu sync.Mutex
   30  
   31  	conn  *nftables.Conn
   32  	table *nftables.Table
   33  
   34  	// Filter chain in PREROUTING
   35  	filterChain *nftables.Chain
   36  
   37  	predefinedDenySet  set.Set
   38  	predefinedAllowSet set.Set
   39  
   40  	userDenySet  set.Set
   41  	userAllowSet set.Set
   42  
   43  	tapInterface string
   44  
   45  	allowedRanges []string
   46  }
   47  
   48  func NewFirewall(tapIf string, orchestratorInternalIP string, extraAllowedCIDRs []string) (_ *Firewall, err error) {
   49  	conn, err := nftables.New(nftables.AsLasting())
   50  	if err != nil {
   51  		return nil, fmt.Errorf("new nftables conn: %w", err)
   52  	}
   53  
   54  	defer func() {
   55  		if err != nil {
   56  			err = errors.Join(err, conn.CloseLasting())
   57  		}
   58  	}()
   59  
   60  	table := conn.AddTable(&nftables.Table{
   61  		Name:   tableName,
   62  		Family: nftables.TableFamilyINet,
   63  	})
   64  
   65  	// Filter chain in PREROUTING
   66  	// This handles: allow/deny decisions for traffic from the tap interface
   67  	policy := nftables.ChainPolicyAccept
   68  	filterChain := conn.AddChain(&nftables.Chain{
   69  		Name:     "PREROUTE_FILTER",
   70  		Table:    table,
   71  		Type:     nftables.ChainTypeFilter,
   72  		Hooknum:  nftables.ChainHookPrerouting,
   73  		Priority: nftables.ChainPriorityRef(-150),
   74  		Policy:   &policy,
   75  	})
   76  
   77  	// Create deny-set and allow-set
   78  	alwaysDenySet, err := set.New(conn, table, "filtered_always_denylist", nftables.TypeIPAddr)
   79  	if err != nil {
   80  		return nil, fmt.Errorf("new deny set: %w", err)
   81  	}
   82  	alwaysAllowSet, err := set.New(conn, table, "filtered_always_allowlist", nftables.TypeIPAddr)
   83  	if err != nil {
   84  		return nil, fmt.Errorf("new allow set: %w", err)
   85  	}
   86  
   87  	denySet, err := set.New(conn, table, "filtered_denylist", nftables.TypeIPAddr)
   88  	if err != nil {
   89  		return nil, fmt.Errorf("new deny set: %w", err)
   90  	}
   91  	allowSet, err := set.New(conn, table, "filtered_allowlist", nftables.TypeIPAddr)
   92  	if err != nil {
   93  		return nil, fmt.Errorf("new allow set: %w", err)
   94  	}
   95  
   96  	controlIP := fmt.Sprintf("%s/32", orchestratorInternalIP)
   97  
   98  	fw := &Firewall{
   99  		conn:               conn,
  100  		table:              table,
  101  		predefinedDenySet:  alwaysDenySet,
  102  		predefinedAllowSet: alwaysAllowSet,
  103  		userDenySet:        denySet,
  104  		userAllowSet:       allowSet,
  105  		tapInterface:       tapIf,
  106  		allowedRanges: append(
  107  			[]string{controlIP},
  108  			extraAllowedCIDRs...,
  109  		),
  110  		filterChain: filterChain,
  111  	}
  112  
  113  	// Install default rules and initial set data in a single flush.
  114  	fw.installRules(false)
  115  	if err := fw.bufferUserRules(nil, nil); err != nil {
  116  		return nil, fmt.Errorf("error while configuring initial data: %w", err)
  117  	}
  118  	if err := fw.conn.Flush(); err != nil {
  119  		return nil, fmt.Errorf("flush initial firewall rules: %w", err)
  120  	}
  121  
  122  	return fw, nil
  123  }
  124  
  125  func (fw *Firewall) Close() error {
  126  	fw.mu.Lock()
  127  	defer fw.mu.Unlock()
  128  
  129  	fw.conn.DelTable(&nftables.Table{
  130  		Name:   tableName,
  131  		Family: nftables.TableFamilyINet,
  132  	})
  133  	deleteErr := fw.conn.Flush()
  134  	if errors.Is(deleteErr, unix.ENOENT) {
  135  		deleteErr = nil
  136  	}
  137  
  138  	return errors.Join(deleteErr, fw.conn.CloseLasting())
  139  }
  140  
  141  // resetConn replaces fw.conn with a fresh netlink connection, discarding
  142  // buffered messages and any sticky serialization error — nftables.Conn has no
  143  // API for that (see https://github.com/google/nftables/pull/324).
  144  func (fw *Firewall) resetConn(ctx context.Context) error {
  145  	closeErr := fw.conn.CloseLasting()
  146  
  147  	conn, err := nftables.New(nftables.AsLasting())
  148  	if err != nil {
  149  		err = fmt.Errorf("open new lasting nftables conn: %w", err)
  150  
  151  		// Fall back to a transient conn.
  152  		var transientErr error
  153  		conn, transientErr = nftables.New()
  154  		if transientErr != nil {
  155  			err = errors.Join(err, fmt.Errorf("open transient nftables conn: %w", transientErr))
  156  		}
  157  	}
  158  
  159  	resetErr := errors.Join(closeErr, err)
  160  	switch {
  161  	case conn == nil:
  162  		// Both the lasting and transient constructors failed. Keep the old
  163  		// (already closed, possibly poisoned) conn rather than storing nil and
  164  		// panicking on next use; the firewall is left in a degraded state.
  165  		logger.L().Error(ctx, "firewall nftables conn reset failed; reusing the old conn",
  166  			zap.String("tap_interface", fw.tapInterface), zap.Error(resetErr))
  167  	case resetErr != nil:
  168  		fw.conn = conn
  169  		logger.L().Error(ctx, "firewall nftables conn reset after apply failure encountered errors",
  170  			zap.String("tap_interface", fw.tapInterface), zap.Error(resetErr))
  171  	default:
  172  		fw.conn = conn
  173  		logger.L().Warn(ctx, "firewall nftables conn reset after apply failure",
  174  			zap.String("tap_interface", fw.tapInterface))
  175  	}
  176  
  177  	return resetErr
  178  }
  179  
  180  // tapIfaceMatch returns expressions that match packets from the tap interface.
  181  func (fw *Firewall) tapIfaceMatch() []expr.Any {
  182  	return []expr.Any{
  183  		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
  184  		&expr.Cmp{
  185  			Register: 1,
  186  			Op:       expr.CmpOpEq,
  187  			Data:     append([]byte(fw.tapInterface), 0), // null-terminated interface name
  188  		},
  189  	}
  190  }
  191  
  192  // accept returns an expression that accepts the packet.
  193  func accept() []expr.Any {
  194  	return []expr.Any{
  195  		&expr.Verdict{Kind: expr.VerdictAccept},
  196  	}
  197  }
  198  
  199  // addSetFilterRule adds a filter rule that matches destination IPs in a set.
  200  // If drop is true, packets are dropped. Otherwise, they are accepted.
  201  // This applies to ALL protocols.
  202  func (fw *Firewall) addSetFilterRule(ipSet *nftables.Set, drop bool) {
  203  	var verdict []expr.Any
  204  	if drop {
  205  		verdict = []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
  206  	} else {
  207  		verdict = accept()
  208  	}
  209  
  210  	fw.conn.AddRule(&nftables.Rule{
  211  		Table: fw.table,
  212  		Chain: fw.filterChain,
  213  		Exprs: append(append(fw.tapIfaceMatch(),
  214  			expressions.IPv4DestinationAddress(1),
  215  			expressions.IPSetLookUp(ipSet, 1)),
  216  			verdict...,
  217  		),
  218  	})
  219  }
  220  
  221  // addNonTCPSetFilterRule adds a filter rule that matches ONLY non-TCP traffic to destinations in a set.
  222  // If drop is true, packets are dropped. Otherwise, they are accepted.
  223  // TCP traffic is NOT affected by this rule (iptables REDIRECT handles TCP traffic).
  224  func (fw *Firewall) addNonTCPSetFilterRule(ipSet *nftables.Set, drop bool) {
  225  	var verdict []expr.Any
  226  	if drop {
  227  		verdict = []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
  228  	} else {
  229  		verdict = accept()
  230  	}
  231  
  232  	fw.conn.AddRule(&nftables.Rule{
  233  		Table: fw.table,
  234  		Chain: fw.filterChain,
  235  		Exprs: append(append(fw.tapIfaceMatch(),
  236  			// Match non-TCP protocol (protocol != TCP)
  237  			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
  238  			&expr.Cmp{
  239  				Op:       expr.CmpOpNeq,
  240  				Register: 1,
  241  				Data:     []byte{unix.IPPROTO_TCP},
  242  			},
  243  			// Check dest in set
  244  			expressions.IPv4DestinationAddress(1),
  245  			expressions.IPSetLookUp(ipSet, 1)),
  246  			verdict...,
  247  		),
  248  	})
  249  }
  250  
  251  // addEstablishedAcceptRule buffers a rule that accepts ESTABLISHED/RELATED
  252  // return traffic from the tap interface, so response packets are allowed even
  253  // when the source sits in a deny set. Buffer-only; the caller flushes.
  254  func (fw *Firewall) addEstablishedAcceptRule() {
  255  	fw.conn.AddRule(&nftables.Rule{
  256  		Table: fw.table,
  257  		Chain: fw.filterChain,
  258  		Exprs: append(append(fw.tapIfaceMatch(),
  259  			// Load CT state
  260  			&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
  261  			// Check ESTABLISHED or RELATED
  262  			&expr.Bitwise{
  263  				SourceRegister: 1,
  264  				DestRegister:   1,
  265  				Len:            4,
  266  				Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
  267  				Xor:            binaryutil.NativeEndian.PutUint32(0),
  268  			},
  269  			&expr.Cmp{
  270  				Op:       expr.CmpOpNeq,
  271  				Register: 1,
  272  				Data:     binaryutil.NativeEndian.PutUint32(0),
  273  			}),
  274  			accept()...,
  275  		),
  276  	})
  277  }
  278  
  279  // addTapDropRule buffers a rule that drops every packet from the tap interface,
  280  // regardless of protocol or destination. Buffer-only; the caller flushes.
  281  func (fw *Firewall) addTapDropRule() {
  282  	fw.conn.AddRule(&nftables.Rule{
  283  		Table: fw.table,
  284  		Chain: fw.filterChain,
  285  		Exprs: append(fw.tapIfaceMatch(),
  286  			&expr.Verdict{Kind: expr.VerdictDrop},
  287  		),
  288  	})
  289  }
  290  
  291  // installRules buffers the filter chain rules. When byop is true, Rule 3 drops
  292  // non-TCP only (TCP shifts to the userspace SOCKS5 proxy); otherwise it drops
  293  // all protocols. Buffer-only; the caller must Flush.
  294  func (fw *Firewall) installRules(byop bool) {
  295  	// FILTER CHAIN (PREROUTING, priority -150)
  296  	//   1. ESTABLISHED/RELATED → accept
  297  	//   2. predefinedAllowSet → accept (all protocols)
  298  	//   3. predefinedDenySet → DROP (all protocols, or non-TCP only when byop)
  299  	//   4. Non-TCP: userAllowSet → accept
  300  	//   5. Non-TCP: userDenySet → DROP
  301  	//   6. Default: ACCEPT (TCP handled by iptables REDIRECT in host netns)
  302  
  303  	// Rule 1: Allow ESTABLISHED/RELATED connections - all protocols
  304  	// This ensures response packets are allowed even if the source is in predefinedDenySet
  305  	fw.addEstablishedAcceptRule()
  306  
  307  	// Rule 2: predefinedAllowSet → accept (all protocols)
  308  	fw.addSetFilterRule(fw.predefinedAllowSet.Set(), false)
  309  
  310  	// Rule 3: predefinedDenySet → DROP (all protocols, or non-TCP only in BYOP).
  311  	if byop {
  312  		fw.addNonTCPSetFilterRule(fw.predefinedDenySet.Set(), true)
  313  	} else {
  314  		fw.addSetFilterRule(fw.predefinedDenySet.Set(), true)
  315  	}
  316  
  317  	// Rule 4: Non-TCP + userAllowSet → accept
  318  	// Only non-TCP traffic is affected; TCP goes to proxy
  319  	fw.addNonTCPSetFilterRule(fw.userAllowSet.Set(), false)
  320  
  321  	// Rule 5: Non-TCP + userDenySet → DROP
  322  	// Only non-TCP traffic is affected; TCP goes to proxy
  323  	fw.addNonTCPSetFilterRule(fw.userDenySet.Set(), true)
  324  
  325  	// Default policy: ACCEPT
  326  	// - Non-TCP not in user sets: allowed (default policy)
  327  	// - TCP: iptables REDIRECT handles TCP traffic to proxy
  328  }
  329  
  330  // bufferUserRules buffers a full replacement of every firewall set.
  331  // Buffer-only; the caller flushes.
  332  func (fw *Firewall) bufferUserRules(allowedCIDRs, deniedCIDRs []string) error {
  333  	// 1. Reset predefined deny set to default blocked ranges (buffered, no flush).
  334  	if err := fw.predefinedDenySet.ClearAndAddElements(fw.conn, sandbox_network.DeniedSandboxSetData); err != nil {
  335  		return fmt.Errorf("reset predefined deny set: %w", err)
  336  	}
  337  
  338  	// 2. Reset predefined allow set to allowedRanges (buffered, no flush).
  339  	allowedSetData, err := set.AddressStringsToSetData(fw.allowedRanges)
  340  	if err != nil {
  341  		return fmt.Errorf("parse initial allowed CIDRs: %w", err)
  342  	}
  343  	if err := fw.predefinedAllowSet.ClearAndAddElements(fw.conn, allowedSetData); err != nil {
  344  		return fmt.Errorf("reset predefined allow set: %w", err)
  345  	}
  346  
  347  	// 3. Replace user deny set with new denied CIDRs (buffered, no flush).
  348  	if err := clearAndReplaceCIDRs(fw.conn, fw.userDenySet, deniedCIDRs); err != nil {
  349  		return fmt.Errorf("replace user deny set: %w", err)
  350  	}
  351  
  352  	// 4. Replace user allow set with new allowed CIDRs (buffered, no flush).
  353  	if err := clearAndReplaceCIDRs(fw.conn, fw.userAllowSet, allowedCIDRs); err != nil {
  354  		return fmt.Errorf("replace user allow set: %w", err)
  355  	}
  356  
  357  	return nil
  358  }
  359  
  360  // ApplyRules reinstalls the filter chain in the given BYOP mode and replaces
  361  // all firewall sets, committed in a single atomic flush so the kernel never
  362  // holds the new Rule 3 mode with stale user sets. The chain is always rebuilt
  363  // from scratch; on flush failure the kernel keeps the previous ruleset and no
  364  // in-memory state can desync from it.
  365  //
  366  // On any failure the conn is replaced via resetConn, so a poisoned batch can
  367  // never leak into a later flush.
  368  func (fw *Firewall) ApplyRules(ctx context.Context, byop bool, allowedCIDRs, deniedCIDRs []string) (err error) {
  369  	fw.mu.Lock()
  370  	defer fw.mu.Unlock()
  371  
  372  	defer func() {
  373  		if err != nil {
  374  			err = errors.Join(err, fw.resetConn(ctx))
  375  		}
  376  	}()
  377  
  378  	fw.conn.FlushChain(fw.filterChain)
  379  	fw.installRules(byop)
  380  	if err := fw.bufferUserRules(allowedCIDRs, deniedCIDRs); err != nil {
  381  		return err
  382  	}
  383  
  384  	if err := fw.conn.Flush(); err != nil {
  385  		return fmt.Errorf("flush atomic rule application: %w", err)
  386  	}
  387  
  388  	return nil
  389  }
  390  
  391  // DenyEgress rebuilds the filter chain so that every packet originating from
  392  // the guest (the tap interface) is dropped, except ESTABLISHED/RELATED return
  393  // traffic. Unlike the user deny set — which drops only non-TCP traffic and
  394  // leaves TCP to the egress proxy — this drops ALL protocols and allows NO
  395  // guest-initiated destination, so a sandbox isolated this way cannot reach the
  396  // network at all, including the orchestrator's own in-sandbox IP (which also
  397  // fronts the NFS proxy, portmapper and hyperloop). The orchestrator still
  398  // drives the resume because it connects INTO the guest (envd /init, health
  399  // probes) and those replies match ESTABLISHED/RELATED; nothing about the resume
  400  // needs the guest to open a connection outward. It backs the throwaway
  401  // pause-resume prefetch harvest sandbox, whose envd init must not egress — the
  402  // throwaway is also resumed with its volume mounts suppressed so /init does not
  403  // attempt the (now-blocked) NFS mount.
  404  //
  405  // Like ApplyRules the chain is rebuilt from scratch and committed in a single
  406  // flush; on failure the conn is reset so a poisoned batch cannot leak into a
  407  // later flush.
  408  func (fw *Firewall) DenyEgress(ctx context.Context) (err error) {
  409  	fw.mu.Lock()
  410  	defer fw.mu.Unlock()
  411  
  412  	defer func() {
  413  		if err != nil {
  414  			err = errors.Join(err, fw.resetConn(ctx))
  415  		}
  416  	}()
  417  
  418  	fw.conn.FlushChain(fw.filterChain)
  419  
  420  	// Rule 1: ESTABLISHED/RELATED → accept (lets the orchestrator-driven control
  421  	// path, which connects into the guest, get its replies back).
  422  	fw.addEstablishedAcceptRule()
  423  	// Rule 2: everything else from the tap → drop, all protocols. No
  424  	// guest-initiated egress is allowed, not even to the orchestrator IP.
  425  	fw.addTapDropRule()
  426  
  427  	if err := fw.conn.Flush(); err != nil {
  428  		return fmt.Errorf("flush deny-egress rules: %w", err)
  429  	}
  430  
  431  	return nil
  432  }
  433  
  434  var maxIPv4 = netip.MustParseAddr("255.255.255.255")
  435  
  436  // clearAndReplaceCIDRs clears a set and repopulates it with the given CIDRs.
  437  // Buffered — nothing commits until conn.Flush() in ReplaceUserRules.
  438  func clearAndReplaceCIDRs(conn *nftables.Conn, s set.Set, cidrs []string) error {
  439  	toolkitCIDRs, boundaryStart := splitEgressCIDRs(cidrs)
  440  
  441  	if len(toolkitCIDRs) == 0 {
  442  		conn.FlushSet(s.Set())
  443  	} else {
  444  		data, err := set.AddressStringsToSetData(toolkitCIDRs)
  445  		if err != nil {
  446  			return err
  447  		}
  448  
  449  		if err := s.ClearAndAddElements(conn, data); err != nil {
  450  			return err
  451  		}
  452  	}
  453  
  454  	if !boundaryStart.IsValid() {
  455  		return nil
  456  	}
  457  
  458  	// A lone start element (no interval end) runs to the top — nft's own encoding;
  459  	// an explicit 255.255.255.255 end is exclusive and would drop the broadcast.
  460  	if err := conn.SetAddElements(s.Set(), []nftables.SetElement{
  461  		{Key: boundaryStart.AsSlice()},
  462  	}); err != nil {
  463  		return fmt.Errorf("add max-boundary CIDR element: %w", err)
  464  	}
  465  
  466  	return nil
  467  }
  468  
  469  // splitEgressCIDRs separates toolkit-encodable CIDRs from max-ending ranges (the
  470  // toolkit's end.Next() overflows there). Those collapse to one [boundaryStart,
  471  // maxIPv4] interval; ordinary CIDRs it subsumes are dropped, since the non-merge
  472  // set silently corrupts overlapping elements in a single flush.
  473  func splitEgressCIDRs(cidrs []string) (toolkit []string, boundaryStart netip.Addr) {
  474  	toolkit = make([]string, 0, len(cidrs))
  475  	for _, cidr := range cidrs {
  476  		start, end, ok := ipv4Range(cidr)
  477  		if ok && end == maxIPv4 {
  478  			if !boundaryStart.IsValid() || start.Less(boundaryStart) {
  479  				boundaryStart = start
  480  			}
  481  
  482  			continue
  483  		}
  484  
  485  		toolkit = append(toolkit, cidr)
  486  	}
  487  
  488  	if !boundaryStart.IsValid() {
  489  		return toolkit, boundaryStart
  490  	}
  491  
  492  	kept := toolkit[:0]
  493  	for _, cidr := range toolkit {
  494  		if start, _, ok := ipv4Range(cidr); ok && !start.Less(boundaryStart) {
  495  			continue
  496  		}
  497  		kept = append(kept, cidr)
  498  	}
  499  
  500  	return kept, boundaryStart
  501  }
  502  
  503  // ipv4Range returns the [start, end] of an IPv4 CIDR or bare address; ok is
  504  // false for non-IPv4 or unparseable input.
  505  func ipv4Range(cidr string) (start, end netip.Addr, ok bool) {
  506  	if p, err := netip.ParsePrefix(cidr); err == nil {
  507  		if !p.Addr().Is4() {
  508  			return netip.Addr{}, netip.Addr{}, false
  509  		}
  510  		start, end = extnetip.Range(p.Masked())
  511  
  512  		return start, end, true
  513  	}
  514  	if a, err := netip.ParseAddr(cidr); err == nil && a.Is4() {
  515  		return a, a, true
  516  	}
  517  
  518  	return netip.Addr{}, netip.Addr{}, false
  519  }
````
Slot wiring (where the firewall is created and rules applied):

`packages/orchestrator/pkg/sandbox/network/slot.go` L233-L258:

````go
  233  func (s *Slot) InitializeFirewall() error {
  234  	if s.Firewall != nil {
  235  		return fmt.Errorf("firewall is already initialized for slot %s", s.Key)
  236  	}
  237  
  238  	fw, err := NewFirewall(s.TapName(), s.config.OrchestratorInSandboxIPAddress, s.config.AllowSandboxInternalCIDRs)
  239  	if err != nil {
  240  		return fmt.Errorf("error initializing firewall: %w", err)
  241  	}
  242  	s.Firewall = fw
  243  
  244  	return nil
  245  }
  246  
  247  func (s *Slot) CloseFirewall() error {
  248  	if s.Firewall == nil {
  249  		return nil
  250  	}
  251  
  252  	if err := s.Firewall.Close(); err != nil {
  253  		return fmt.Errorf("error closing firewall: %w", err)
  254  	}
  255  	s.Firewall = nil
  256  
  257  	return nil
  258  }
````

`packages/orchestrator/pkg/sandbox/network/slot.go` L354-L490:

````go
  354  func (s *Slot) ConfigureInternet(ctx context.Context, network *orchestrator.SandboxNetworkConfig) (e error) {
  355  	ctx, span := tracer.Start(ctx, "slot-internet-configure", trace.WithAttributes(
  356  		attribute.String("namespace_id", s.NamespaceID()),
  357  	))
  358  	defer span.End()
  359  
  360  	egress := network.GetEgress()
  361  	hasUserRules := len(egress.GetAllowedCidrs()) != 0 ||
  362  		len(egress.GetDeniedCidrs()) != 0 ||
  363  		len(egress.GetAllowedDomains()) != 0
  364  	hasBYOP := egress.GetEgressProxyAddress() != ""
  365  
  366  	if !hasUserRules && !hasBYOP {
  367  		// Internet access is allowed by default.
  368  		return nil
  369  	}
  370  
  371  	s.firewallCustomRules.Store(true)
  372  
  373  	n, err := ns.GetNS(filepath.Join(NetNamespacesDir, s.NamespaceID()))
  374  	if err != nil {
  375  		return fmt.Errorf("failed to get slot network namespace '%s': %w", s.NamespaceID(), err)
  376  	}
  377  	defer n.Close()
  378  
  379  	err = n.Do(func(_ ns.NetNS) error {
  380  		return s.Firewall.ApplyRules(ctx, hasBYOP, egress.GetAllowedCidrs(), egress.GetDeniedCidrs())
  381  	})
  382  	if err != nil {
  383  		return fmt.Errorf("failed execution in network namespace '%s': %w", s.NamespaceID(), err)
  384  	}
  385  
  386  	return nil
  387  }
  388  
  389  // UpdateInternet replaces all user firewall rules atomically in a single nftables flush.
  390  func (s *Slot) UpdateInternet(ctx context.Context, egress *orchestrator.SandboxNetworkEgressConfig) error {
  391  	ctx, span := tracer.Start(ctx, "slot-internet-update", trace.WithAttributes(
  392  		attribute.String("namespace_id", s.NamespaceID()),
  393  	))
  394  	defer span.End()
  395  
  396  	// A slot without a firewall (NewSlot does not attach one) must fail
  397  	// cleanly, not nil-panic inside the netns closure. This is also load-
  398  	// bearing for tests: namespace names are ns-<idx> only, so a fixture
  399  	// slot's GetNS can unexpectedly SUCCEED when a parallel test binary has
  400  	// a real netns with the same index open.
  401  	if s.Firewall == nil {
  402  		return fmt.Errorf("no firewall attached to slot namespace '%s'", s.NamespaceID())
  403  	}
  404  
  405  	allowedCIDRs := egress.GetAllowedCidrs()
  406  	deniedCIDRs := egress.GetDeniedCidrs()
  407  	hasBYOP := egress.GetEgressProxyAddress() != ""
  408  
  409  	n, err := ns.GetNS(filepath.Join(NetNamespacesDir, s.NamespaceID()))
  410  	if err != nil {
  411  		return fmt.Errorf("failed to get slot network namespace '%s': %w", s.NamespaceID(), err)
  412  	}
  413  	defer n.Close()
  414  
  415  	// Set before mutating: a partial failure must still trigger cleanup.
  416  	s.firewallCustomRules.Store(true)
  417  
  418  	err = n.Do(func(_ ns.NetNS) error {
  419  		return s.Firewall.ApplyRules(ctx, hasBYOP, allowedCIDRs, deniedCIDRs)
  420  	})
  421  	if err != nil {
  422  		return fmt.Errorf("failed execution in network namespace '%s': %w", s.NamespaceID(), err)
  423  	}
  424  
  425  	return nil
  426  }
  427  
  428  // DenyEgress drops all guest-originated traffic except the orchestrator control
  429  // path, isolating a throwaway sandbox (the pause-resume prefetch harvest) so its
  430  // envd init cannot reach the network. Like UpdateInternet it marks custom rules
  431  // so the slot's cleanup resets the firewall before reuse.
  432  func (s *Slot) DenyEgress(ctx context.Context) error {
  433  	ctx, span := tracer.Start(ctx, "slot-internet-deny-egress", trace.WithAttributes(
  434  		attribute.String("namespace_id", s.NamespaceID()),
  435  	))
  436  	defer span.End()
  437  
  438  	// Guard against an uninitialized firewall before marking custom rules: a
  439  	// stored flag with a nil firewall would also panic the cleanup (ResetInternet).
  440  	if s.Firewall == nil {
  441  		return fmt.Errorf("firewall is not initialized for slot '%s'", s.NamespaceID())
  442  	}
  443  
  444  	n, err := ns.GetNS(filepath.Join(NetNamespacesDir, s.NamespaceID()))
  445  	if err != nil {
  446  		return fmt.Errorf("failed to get slot network namespace '%s': %w", s.NamespaceID(), err)
  447  	}
  448  	defer n.Close()
  449  
  450  	// Set before mutating: a partial failure must still trigger cleanup.
  451  	s.firewallCustomRules.Store(true)
  452  
  453  	err = n.Do(func(_ ns.NetNS) error {
  454  		return s.Firewall.DenyEgress(ctx)
  455  	})
  456  	if err != nil {
  457  		return fmt.Errorf("failed execution in network namespace '%s': %w", s.NamespaceID(), err)
  458  	}
  459  
  460  	return nil
  461  }
  462  
  463  func (s *Slot) ResetInternet(ctx context.Context) error {
  464  	ctx, span := tracer.Start(ctx, "slot-internet-reset", trace.WithAttributes(
  465  		attribute.String("namespace_id", s.NamespaceID()),
  466  	))
  467  	defer span.End()
  468  
  469  	if !s.firewallCustomRules.Load() {
  470  		return nil
  471  	}
  472  
  473  	n, err := ns.GetNS(filepath.Join(NetNamespacesDir, s.NamespaceID()))
  474  	if err != nil {
  475  		return fmt.Errorf("failed to get slot network namespace '%s': %w", s.NamespaceID(), err)
  476  	}
  477  	defer n.Close()
  478  
  479  	err = n.Do(func(_ ns.NetNS) error {
  480  		// Revert BYOP so the next tenant can't inherit non-TCP-only deny rules.
  481  		return s.Firewall.ApplyRules(ctx, false, nil, nil)
  482  	})
  483  	if err != nil {
  484  		return fmt.Errorf("failed execution in network namespace '%s': %w", s.NamespaceID(), err)
  485  	}
  486  
  487  	s.firewallCustomRules.Store(false)
  488  
  489  	return nil
  490  }
````

`packages/orchestrator/pkg/server/sandboxes.go` L609-L652:

````go
  609  // transitionEgress moves the in-memory egress config (read per-connection by
  610  // the userspace proxy) and the in-netns kernel firewall from one state to
  611  // another. Loosen last, tighten first: the kernel must never be more relaxed
  612  // than the config the proxy sees. On failure both layers stay consistent.
  613  // updateInternet is a parameter so tests can observe the ordering.
  614  func transitionEgress(
  615  	ctx context.Context,
  616  	cfg *sandbox.Config,
  617  	updateInternet func(context.Context, *orchestrator.SandboxNetworkEgressConfig) error,
  618  	from, to *orchestrator.SandboxNetworkEgressConfig,
  619  ) error {
  620  	if to.GetEgressProxyAddress() != "" {
  621  		// BYOP loosens the kernel firewall: internal-destined TCP is handed
  622  		// to the userspace proxy instead of dropped, so the proxy must see
  623  		// the config before the kernel lets that traffic through.
  624  		applyNetworkEgress(cfg, to)
  625  		if err := updateInternet(ctx, to); err != nil {
  626  			// The kernel kept its rules (atomic flush); undo the publish.
  627  			applyNetworkEgress(cfg, from)
  628  
  629  			return fmt.Errorf("failed to update sandbox network: %w", err)
  630  		}
  631  
  632  		return nil
  633  	}
  634  
  635  	if err := updateInternet(ctx, to); err != nil {
  636  		return fmt.Errorf("failed to update sandbox network: %w", err)
  637  	}
  638  	applyNetworkEgress(cfg, to)
  639  
  640  	return nil
  641  }
  642  
  643  // applyNetworkEgress publishes the egress config to the in-memory sandbox
  644  // config, collapsing an all-empty egress (no CIDRs, domains, rules, or proxy)
  645  // to nil so readers treat it as "no custom egress".
  646  func applyNetworkEgress(cfg *sandbox.Config, egress *orchestrator.SandboxNetworkEgressConfig) {
  647  	if len(egress.GetAllowedCidrs()) == 0 && len(egress.GetDeniedCidrs()) == 0 && len(egress.GetAllowedDomains()) == 0 && len(egress.GetRules()) == 0 && egress.GetEgressProxyAddress() == "" {
  648  		cfg.SetNetworkEgress(nil)
  649  	} else {
  650  		cfg.SetNetworkEgress(egress)
  651  	}
  652  }
````

Effective rule order (chain `PREROUTE_FILTER`, table `inet slot-firewall`, hook prerouting prio -150,
inside netns `ns-<idx>`, matching only `iifname "tap0"` = guest-originated):
1. ct state established,related -> accept
2. daddr in `filtered_always_allowlist` -> accept (all protocols). Contents =
   `fw.allowedRanges` = `["192.0.2.1/32"] + ALLOW_SANDBOX_INTERNAL_CIDRS` (L106-L109, reset every apply at
   L338-L345).
3. daddr in `filtered_always_denylist` (= `DeniedSandboxCIDRs`) -> drop (all protocols; non-TCP only
   when BYOP)
4. non-TCP && daddr in `filtered_allowlist` (user `allowed_cidrs`) -> accept
5. non-TCP && daddr in `filtered_denylist` (user `denied_cidrs`) -> drop
6. policy accept. **TCP user allow/deny is not enforced here** — all TCP continues to the host veth where
   iptables REDIRECTs it into the tcpfirewall proxy (P4.3).

So user `allowed_cidrs` can never open a private range (rule 3 precedes rule 4 and drops TCP too).
Sets are `TypeIPAddr` interval sets: **no port granularity** at this layer, and the comment at L469-L472
warns that overlapping elements in one flush corrupt the non-merge set, so any added list must be
deduplicated/merged (and must not overlap `192.0.2.1/32` or the node-wide list).

### P4.3 Layer 2 — userspace TCP firewall (tcpfirewall)
Redirect rules (host netns, appended after the 192.0.2.1 hyperloop/portmapper/NFS REDIRECTs, so those match
first):

`packages/orchestrator/pkg/tcpfirewall/proxy.go` L146-L185:

````go
  146  type proxyRule struct {
  147  	dstPort   string // destination port to match (empty = all ports)
  148  	proxyPort string // port to redirect to
  149  	desc      string // description for error messages
  150  }
  151  
  152  func (p *Proxy) ruleArgs(s *network.Slot, rule proxyRule) []string {
  153  	args := []string{"-i", s.VethName(), "-p", "tcp"}
  154  	if rule.dstPort != "" {
  155  		args = append(args, "--dport", rule.dstPort)
  156  	}
  157  	args = append(args,
  158  		"-j", "REDIRECT", "--to-port", rule.proxyPort,
  159  	)
  160  
  161  	return args
  162  }
  163  
  164  func (p *Proxy) OnSlotCreate(s *network.Slot, tables *iptables.IPTables) error {
  165  	for _, rule := range p.proxyRules {
  166  		err := tables.Append("nat", "PREROUTING", p.ruleArgs(s, rule)...)
  167  		if err != nil {
  168  			return fmt.Errorf("error creating redirect rule for %s traffic: %w", rule.desc, err)
  169  		}
  170  	}
  171  
  172  	return nil
  173  }
  174  
  175  func (p *Proxy) OnSlotDelete(s *network.Slot, tables *iptables.IPTables) error {
  176  	var errs []error
  177  	for _, rule := range p.proxyRules {
  178  		err := tables.Delete("nat", "PREROUTING", p.ruleArgs(s, rule)...)
  179  		if err != nil {
  180  			errs = append(errs, fmt.Errorf("error deleting %s egress proxy redirect rule: %w", rule.desc, err))
  181  		}
  182  	}
  183  
  184  	return errors.Join(errs...)
  185  }
````

`packages/orchestrator/pkg/tcpfirewall/proxy.go` L262-L328:

````go
  262  func (t *connectionHandler) HandleConn(conn net.Conn) {
  263  	// Request tracing context.
  264  	ctx := t.ctx
  265  
  266  	// Get the underlying connection for sandbox lookup and original dst.
  267  	// tcpproxy may wrap in *tcpproxy.Conn for peeked bytes.
  268  	rawConn := tcpproxy.UnderlyingConn(conn)
  269  
  270  	// Look up sandbox by source address
  271  	sourceAddr := rawConn.RemoteAddr().String()
  272  	sbx, err := t.deps.sandboxes.GetByHostPort(sourceAddr)
  273  	if err != nil {
  274  		sourceIP, _, _ := net.SplitHostPort(sourceAddr)
  275  		t.deps.logger.Error(ctx, "failed to find sandbox for connection",
  276  			logger.WithSandboxIP(sourceIP),
  277  			zap.Error(err))
  278  		t.deps.metrics.RecordError(ctx, ErrorTypeSandboxLookup, t.protocol)
  279  		conn.Close()
  280  
  281  		return
  282  	}
  283  
  284  	sandboxID := sbx.Runtime.SandboxID
  285  	// Scope the limiter to this sandbox lifecycle: SandboxID is reused on
  286  	// checkpoint/resume and the IP is reused via the network slot pool, so
  287  	// only LifecycleID is unique per lifecycle.
  288  	limiterKey := sbx.LifecycleID
  289  	sbxLogger := t.deps.logger.With(logger.WithSandboxID(sandboxID))
  290  
  291  	// Check per-sandbox connection limit
  292  	maxLimit := t.deps.featureFlags.IntFlag(ctx, featureflags.TCPFirewallMaxConnectionsPerSandbox)
  293  	count, acquired := t.deps.limiter.TryAcquire(limiterKey, maxLimit)
  294  	if !acquired {
  295  		t.deps.metrics.RecordError(ctx, ErrorTypeLimitExceeded, t.protocol)
  296  		conn.Close()
  297  
  298  		return
  299  	}
  300  
  301  	// Get original destination (before iptables redirect)
  302  	ip, port, err := getOriginalDst(rawConn)
  303  	if err != nil {
  304  		sbxLogger.Error(ctx, "failed to get original destination", zap.Error(err))
  305  		t.deps.metrics.RecordError(ctx, ErrorTypeOrigDst, t.protocol)
  306  		t.deps.limiter.Release(limiterKey)
  307  		conn.Close()
  308  
  309  		return
  310  	}
  311  
  312  	t.deps.metrics.RecordConnectionsPerSandbox(ctx, count)
  313  	t.deps.metrics.RecordConnection(ctx, t.protocol)
  314  
  315  	// Release the connection slot once the handler returns.
  316  	defer t.deps.limiter.Release(limiterKey)
  317  
  318  	t.handler(ctx, egressConn{
  319  		conn:     conn,
  320  		sbx:      sbx,
  321  		dstIP:    ip,
  322  		dstPort:  port,
  323  		protocol: t.protocol,
  324  		tos:      t.deps.egressTOS.For(sbx.Runtime.SandboxType.EgressClass()),
  325  		logger:   sbxLogger,
  326  		metrics:  t.deps.metrics,
  327  	})
  328  }
````

`packages/orchestrator/pkg/tcpfirewall/handlers.go` (full file, 278 lines):

````go
    1  //go:build linux
    2  
    3  package tcpfirewall
    4  
    5  import (
    6  	"context"
    7  	"errors"
    8  	"fmt"
    9  	"net"
   10  	"strings"
   11  	"syscall"
   12  	"time"
   13  
   14  	"github.com/inetaf/tcpproxy"
   15  	"go.uber.org/zap"
   16  
   17  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
   18  	sandbox_network "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-network"
   19  )
   20  
   21  const (
   22  	// upstreamDialTimeout is the maximum time to wait for upstream connections.
   23  	// This prevents goroutine leaks from slow/unresponsive DNS or connections.
   24  	upstreamDialTimeout = 30 * time.Second
   25  
   26  	noHostnameValue = ""
   27  )
   28  
   29  // Sets both IP_TOS and IPV6_TCLASS because Happy Eyeballs may pick either family.
   30  // Tolerates ENOPROTOOPT from the non-matching one.
   31  func markDSCP(c syscall.RawConn, tos int) error {
   32  	if tos == 0 {
   33  		return nil
   34  	}
   35  
   36  	var sockErr error
   37  
   38  	err := c.Control(func(fd uintptr) {
   39  		v4Err := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, tos)
   40  		v6Err := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_TCLASS, tos)
   41  		if v4Err != nil && v6Err != nil {
   42  			sockErr = fmt.Errorf("setsockopt IP_TOS / IPV6_TCLASS both failed: %w", errors.Join(v4Err, v6Err))
   43  		}
   44  	})
   45  	if err != nil {
   46  		return err
   47  	}
   48  
   49  	return sockErr
   50  }
   51  
   52  // domainHandler handles connections with hostname information (HTTP Host header or TLS SNI).
   53  func domainHandler(ctx context.Context, c egressConn) {
   54  	// Get hostname from tcpproxy's wrapped connection (HTTP Host or TLS SNI).
   55  	// Hostname can be empty, e.g. for https://1.1.1.1 like requests.
   56  	var hostname string
   57  	if tc, ok := c.conn.(*tcpproxy.Conn); ok {
   58  		hostname = tc.HostName
   59  	}
   60  
   61  	allowed, matchType, err := isEgressAllowed(c.sbx, hostname, c.dstIP)
   62  	if err != nil {
   63  		c.logger.Error(ctx, "Egress check failed", zap.Error(err))
   64  		c.metrics.RecordError(ctx, ErrorTypeEgressCheck, c.protocol)
   65  		c.conn.Close()
   66  
   67  		return
   68  	}
   69  
   70  	if !allowed {
   71  		c.metrics.RecordDecision(ctx, DecisionBlocked, c.protocol, matchType)
   72  		c.conn.Close()
   73  
   74  		return
   75  	}
   76  
   77  	c.metrics.RecordDecision(ctx, DecisionAllowed, c.protocol, matchType)
   78  
   79  	// When allowed by domain match, dial the hostname directly (not the sandbox's resolved IP).
   80  	// This prevents DNS spoofing attacks where the sandbox modifies /etc/hosts to redirect
   81  	// an allowed domain to an arbitrary IP. We use Go's net.Dialer which provides built-in
   82  	// Happy Eyeballs (RFC 8305) for multi-IP fallback when some IPs are unreachable.
   83  	// After connecting, we verify the connected IP is not internal/private.
   84  	if matchType == MatchTypeDomain {
   85  		proxyWithIPVerification(ctx, c, net.JoinHostPort(hostname, fmt.Sprintf("%d", c.dstPort)))
   86  
   87  		return
   88  	}
   89  
   90  	// For non-domain matches, use the original destination IP
   91  	proxy(ctx, c, c.upstreamAddr())
   92  }
   93  
   94  // cidrOnlyHandler handles connections without hostname information.
   95  func cidrOnlyHandler(ctx context.Context, c egressConn) {
   96  	// No hostname available for CIDR-only handler
   97  	allowed, matchType, err := isEgressAllowed(c.sbx, noHostnameValue, c.dstIP)
   98  	if err != nil {
   99  		c.logger.Error(ctx, "Egress check failed", zap.Error(err))
  100  		c.metrics.RecordError(ctx, ErrorTypeEgressCheck, c.protocol)
  101  		c.conn.Close()
  102  
  103  		return
  104  	}
  105  
  106  	if !allowed {
  107  		c.metrics.RecordDecision(ctx, DecisionBlocked, c.protocol, matchType)
  108  		c.conn.Close()
  109  
  110  		return
  111  	}
  112  
  113  	c.metrics.RecordDecision(ctx, DecisionAllowed, c.protocol, matchType)
  114  
  115  	proxy(ctx, c, c.upstreamAddr())
  116  }
  117  
  118  // proxy proxies the connection to the upstream address.
  119  func proxy(ctx context.Context, c egressConn, upstreamAddr string) {
  120  	tracker := c.metrics.TrackConnection(c.protocol)
  121  	defer tracker.Close(ctx)
  122  
  123  	dp := &tcpproxy.DialProxy{
  124  		Addr:        upstreamAddr,
  125  		DialTimeout: upstreamDialTimeout,
  126  		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
  127  			dialer := &net.Dialer{
  128  				Timeout: upstreamDialTimeout,
  129  				Control: func(_, _ string, rawConn syscall.RawConn) error {
  130  					return markDSCP(rawConn, c.tos)
  131  				},
  132  			}
  133  
  134  			return dialer.DialContext(dialCtx, network, addr)
  135  		},
  136  	}
  137  	dp.HandleConn(c.conn)
  138  }
  139  
  140  // proxyWithIPVerification dials the upstream hostname using Go's net.Dialer (which provides
  141  // built-in Happy Eyeballs / RFC 8305 for multi-IP fallback), and verifies the resolved IP
  142  // is not internal/private BEFORE connecting. This prevents DNS rebinding attacks while
  143  // preserving multi-IP reliability.
  144  //
  145  // The ControlContext callback is called after DNS resolution but before the TCP connect()
  146  // syscall, so no TCP handshake occurs to internal IPs.
  147  func proxyWithIPVerification(ctx context.Context, c egressConn, upstreamAddr string) {
  148  	tracker := c.metrics.TrackConnection(c.protocol)
  149  	defer tracker.Close(ctx)
  150  
  151  	// Use tcpproxy.DialProxy with a custom DialContext that verifies resolved IPs
  152  	dp := &tcpproxy.DialProxy{
  153  		Addr:        upstreamAddr,
  154  		DialTimeout: upstreamDialTimeout,
  155  		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
  156  			// Use Go's net.Dialer which has built-in Happy Eyeballs (RFC 8305)
  157  			// This automatically tries multiple IPs with proper fallback
  158  			dialer := &net.Dialer{
  159  				Timeout: upstreamDialTimeout,
  160  				// ControlContext is called after DNS resolution but BEFORE the TCP connect() syscall.
  161  				// The 'address' parameter contains the resolved IP:port, allowing us to block
  162  				// connections to internal IPs before any TCP handshake occurs.
  163  				ControlContext: func(_ context.Context, _, address string, rawConn syscall.RawConn) error {
  164  					host, _, err := net.SplitHostPort(address)
  165  					if err != nil {
  166  						return fmt.Errorf("failed to parse resolved address %q: %w", address, err)
  167  					}
  168  
  169  					resolvedIP := net.ParseIP(host)
  170  					if resolvedIP == nil {
  171  						return fmt.Errorf("failed to parse IP from resolved address %q", host)
  172  					}
  173  
  174  					if isIPInAlwaysDeniedCIDRs(resolvedIP) {
  175  						c.logger.Warn(ctx, "Blocked connection to internal IP via hostname",
  176  							zap.String("upstream_addr", addr),
  177  							zap.String("resolved_ip", resolvedIP.String()))
  178  						c.metrics.RecordError(ctx, ErrorTypeResolvedIPBlocked, c.protocol)
  179  
  180  						return fmt.Errorf("hostname resolved to internal IP %s", resolvedIP)
  181  					}
  182  
  183  					return markDSCP(rawConn, c.tos)
  184  				},
  185  			}
  186  
  187  			return dialer.DialContext(dialCtx, network, addr)
  188  		},
  189  	}
  190  	dp.HandleConn(c.conn)
  191  }
  192  
  193  // isEgressAllowed checks if egress is allowed based on domain and CIDR rules.
  194  // Returns the allowed status and the match type for metrics.
  195  // Priority order:
  196  //  1. Allow domain / Allow CIDR (if either matches → allow)
  197  //  2. Deny domain / Deny CIDR (if either matches → deny)
  198  //  3. Default: allow
  199  func isEgressAllowed(sbx *sandbox.Sandbox, hostname string, ip net.IP) (bool, MatchType, error) {
  200  	egress := sbx.Config.GetNetworkEgress()
  201  	if egress == nil {
  202  		// No egress configuration, allow all traffic.
  203  		return true, MatchTypeNone, nil
  204  	}
  205  
  206  	// Priority 1: Check allowed domains
  207  	if hostname != noHostnameValue {
  208  		for _, domain := range egress.GetAllowedDomains() {
  209  			if matchDomain(hostname, domain) {
  210  				return true, MatchTypeDomain, nil // Explicitly allowed by domain
  211  			}
  212  		}
  213  	}
  214  
  215  	// Priority 1: Check allowed CIDRs
  216  	for _, cidr := range egress.GetAllowedCidrs() {
  217  		_, ipNet, err := net.ParseCIDR(cidr)
  218  		if err != nil {
  219  			return false, MatchTypeNone, fmt.Errorf("invalid allowed CIDR %q: %w", cidr, err)
  220  		}
  221  
  222  		if ipNet.Contains(ip) {
  223  			return true, MatchTypeCIDR, nil // Explicitly allowed by CIDR
  224  		}
  225  	}
  226  
  227  	// Priority 2: Check denied CIDRs
  228  	for _, cidr := range egress.GetDeniedCidrs() {
  229  		_, ipNet, err := net.ParseCIDR(cidr)
  230  		if err != nil {
  231  			return false, MatchTypeNone, fmt.Errorf("invalid denied CIDR %q: %w", cidr, err)
  232  		}
  233  
  234  		if ipNet.Contains(ip) {
  235  			return false, MatchTypeCIDR, nil // Blocked by CIDR
  236  		}
  237  	}
  238  
  239  	// Default: allow all traffic.
  240  	return true, MatchTypeNone, nil
  241  }
  242  
  243  // matchDomain checks if a hostname matches a domain pattern.
  244  // Patterns can be exact matches, wildcards (*), or suffix wildcards (*.example.com).
  245  func matchDomain(hostname, pattern string) bool {
  246  	switch {
  247  	case pattern == "":
  248  		// Empty pattern should never match
  249  		return false
  250  	case strings.EqualFold(pattern, hostname):
  251  		return true
  252  	case strings.EqualFold(pattern, "*"):
  253  		return true
  254  	case strings.HasPrefix(pattern, "*."):
  255  		suffix := pattern[1:]
  256  		if strings.HasSuffix(strings.ToLower(hostname), strings.ToLower(suffix)) {
  257  			return true
  258  		}
  259  	}
  260  
  261  	return false
  262  }
  263  
  264  // isIPInAlwaysDeniedCIDRs checks if an IP is within the denied sandbox CIDRs (internal/private ranges).
  265  func isIPInAlwaysDeniedCIDRs(ip net.IP) bool {
  266  	for _, cidr := range sandbox_network.DeniedSandboxCIDRs {
  267  		_, ipNet, err := net.ParseCIDR(cidr)
  268  		if err != nil {
  269  			continue
  270  		}
  271  
  272  		if ipNet.Contains(ip) {
  273  			return true
  274  		}
  275  	}
  276  
  277  	return false
  278  }
````

Decision (`isEgressAllowed`, L199-L241): no egress config -> allow; allowed domain (hostname from HTTP Host
/ TLS SNI) -> allow and dial the *hostname* with a pre-connect check that rejects any resolved IP in
`DeniedSandboxCIDRs` (L163-L184); allowed CIDR -> allow; denied CIDR -> deny; default allow. **The
private floor is not checked for IP-addressed TCP here** — it relies on layer 1 having dropped it. The
dial happens from the host network namespace, so a TCP destination that passes layer 1 is reached with host
routing (e.g. k3s ClusterIPs via kube-proxy/iptables on the host, or a host LAN IP).

### P4.4 Where a per-sandbox "allowed private CIDRs" list must be inserted

Carry it in the egress config: add `repeated string allowed_private_cidrs = 8;` (or a message with
`cidr` + optional `ports`) to `SandboxNetworkEgressConfig` (orchestrator.proto L117-L127), regenerate,
and read it via `sbx.Config.GetNetworkEgress()`.

Layer 1 (per netns):
- `Firewall.bufferUserRules` (firewall.go L332-L358), step 2 (L338-L345): build the always-allow set from
  `append(fw.allowedRanges, allowedPrivate...)` instead of `fw.allowedRanges` (merge/dedupe first).
- Thread the list through `ApplyRules(ctx, byop, allowedCIDRs, deniedCIDRs)` (L368-L389) — add a parameter
  — and its callers: `Slot.ConfigureInternet` (slot.go L354-L387; **also add the new field to the
  `hasUserRules` test at L361-L363**, otherwise a config with only private allows returns early at L366-L369
  and nothing is applied), `Slot.UpdateInternet` (L390-L426), `Slot.ResetInternet` (L463-L490, pass nil so
  a recycled slot loses the exemption), and `NewFirewall` initial apply (L113-L117, nil).
- For "a specific host IP:port" only TCP can be port-scoped: either add the IP (/32) to the allow set and
  enforce the port in layer 2, or add a separate rule before rule 3 that accepts `meta l4proto tcp` to
  that IP (so it reaches the proxy) while non-TCP to it stays dropped.

Layer 2 (tcpfirewall):
- `isEgressAllowed` (handlers.go L199-L241): insert a check of the per-sandbox private list after the
  allowed-CIDR loop (after L225) and before the denied-CIDR loop, so e.g. `denied_cidrs: ["0.0.0.0/0"]`
  + private allow still works. For `IP:port` entries the function needs the destination port: add a
  `port int` parameter and pass `c.dstPort` from `domainHandler` (L61) and `cidrOnlyHandler` (L97).
- `proxyWithIPVerification` (L163-L184): only if domain-allowed hostnames that resolve to private
  addresses must work, exempt the per-sandbox list in the `isIPInAlwaysDeniedCIDRs` check (L174). (Guest
  DNS is 8.8.8.8, see D4, so cluster-internal names do not resolve in the guest by default anyway.)

Non-TCP caveat (v1): host forwarding and NAT are installed only toward the default-route interface
(network.go L308-L322: `FORWARD -i veth-N -o <defaultGateway> ACCEPT`, `POSTROUTING -s HostIP/32 -o
<defaultGateway> MASQUERADE`). TCP is unaffected (proxied from the host), but UDP/ICMP to a private range
routed via another interface (e.g. k3s `cni0`/`flannel.1` for 10.42/10.43, or a DNS server at
10.43.0.10:53/udp) is **not** accepted/masqueraded by these rules; it needs extra FORWARD accept +
MASQUERADE rules for that egress interface (host FORWARD policy decides otherwise; Docker sets it to DROP).

Zero-code alternative (node-wide): `ALLOW_SANDBOX_INTERNAL_CIDRS=10.43.0.0/16,<hostIP>/32` puts those
ranges into rule 2 of every sandbox's netns firewall (pool.go L70-L76). Because layer 2 has no floor for
IP-addressed TCP, that alone makes TCP to those ranges work for **all** sandboxes (subject to each
sandbox's own `denied_cidrs`). v2 networking does not read this variable (only slot.go L238 does).

Default sandbox CIDRs `10.11.0.0/16` (host) and `10.12.0.0/16` (veth) do not overlap k3s defaults
(10.42.0.0/16 pods, 10.43.0.0/16 services); they do fall inside the floor's `10.0.0.0/8`, which only
concerns guest-originated destinations.

## P5. Feature flags

### P5.1 Flag machinery and offline store

`packages/shared/pkg/featureflags/flags.go` L52-L156:

````go
   52  // All flags must be defined here: https://app.launchdarkly.com/projects/default/flags/
   53  
   54  type JSONFlag struct {
   55  	name     string
   56  	fallback ldvalue.Value
   57  }
   58  
   59  func (f JSONFlag) Key() string {
   60  	return f.name
   61  }
   62  
   63  func (f JSONFlag) String() string {
   64  	return f.name
   65  }
   66  
   67  func (f JSONFlag) Fallback() ldvalue.Value {
   68  	return f.fallback
   69  }
   70  
   71  func NewJSONFlag(name string, fallback ldvalue.Value) JSONFlag {
   72  	flag := JSONFlag{name: name, fallback: fallback}
   73  	builder := launchDarklyOfflineStore.Flag(flag.name).ValueForAll(fallback)
   74  	launchDarklyOfflineStore.Update(builder)
   75  
   76  	return flag
   77  }
   78  
   79  var CleanNFSCache = NewJSONFlag("clean-nfs-cache", ldvalue.Null())
   80  
   81  // RateLimitConfigFlag provides per-team rate limit overrides.
   82  // JSON format:
   83  //
   84  //	{
   85  //	  "/sandboxes/": {"rate": 50, "burst": 100},
   86  //	  "/sandboxes/:sandboxID/pause": {"rate": 10, "burst": 20}
   87  //	}
   88  //
   89  // Entries set per-team route limits; routes absent from the flag remain unlimited.
   90  var RateLimitConfigFlag = NewJSONFlag("rate-limit-config", ldvalue.Null())
   91  
   92  const (
   93  	APIGroupRateLimitDisabled = "disabled"
   94  	APIGroupRateLimitShadow   = "shadow"
   95  	APIGroupRateLimitEnabled  = "enabled"
   96  )
   97  
   98  var RateLimitV2Mode = NewStringFlag("rate-limit-v2-mode", APIGroupRateLimitDisabled)
   99  
  100  type BoolFlag struct {
  101  	name     string
  102  	fallback bool
  103  }
  104  
  105  func (f BoolFlag) Key() string {
  106  	return f.name
  107  }
  108  
  109  func (f BoolFlag) String() string {
  110  	return f.name
  111  }
  112  
  113  func (f BoolFlag) Fallback() bool {
  114  	return f.fallback
  115  }
  116  
  117  // envBoolOr reads key as a bool, falling back when it is unset or unparseable.
  118  // It exists so a bool flag's fallback can be overridden on a cluster with no
  119  // LaunchDarkly (dev), where a flag otherwise resolves to a value only a rebuild
  120  // can change — the same reason EnvdUpgradeTargetFlag reads its fallback from the
  121  // environment.
  122  func envBoolOr(key string, fallback bool) bool {
  123  	raw := env.GetEnv(key, "")
  124  	if raw == "" {
  125  		return fallback
  126  	}
  127  	parsed, err := strconv.ParseBool(raw)
  128  	if err != nil {
  129  		return fallback
  130  	}
  131  
  132  	return parsed
  133  }
  134  
  135  func NewBoolFlag(name string, fallback bool) BoolFlag {
  136  	flag := BoolFlag{name: name, fallback: fallback}
  137  	builder := launchDarklyOfflineStore.Flag(flag.name).VariationForAll(fallback)
  138  	launchDarklyOfflineStore.Update(builder)
  139  
  140  	return flag
  141  }
  142  
  143  // OverrideBoolFlag forces a bool flag to a specific value in the offline store.
  144  // Only takes effect when LAUNCH_DARKLY_API_KEY is not set (i.e. dev/CLI tools).
  145  func OverrideBoolFlag(flag BoolFlag, value bool) {
  146  	builder := launchDarklyOfflineStore.Flag(flag.name).VariationForAll(value)
  147  	launchDarklyOfflineStore.Update(builder)
  148  }
  149  
  150  // OverrideJSONFlag forces a JSON flag to a specific value in the offline store.
  151  // Only takes effect when LAUNCH_DARKLY_API_KEY is not set (i.e. dev/CLI tools).
  152  func OverrideJSONFlag(flag JSONFlag, value ldvalue.Value) {
  153  	builder := launchDarklyOfflineStore.Flag(flag.name).ValueForAll(value)
  154  	launchDarklyOfflineStore.Update(builder)
  155  }
  156  
````

`packages/shared/pkg/featureflags/flags.go` L477-L500:

````go
  477  type IntFlag struct {
  478  	name     string
  479  	fallback int
  480  }
  481  
  482  func (f IntFlag) Key() string {
  483  	return f.name
  484  }
  485  
  486  func (f IntFlag) String() string {
  487  	return f.name
  488  }
  489  
  490  func (f IntFlag) Fallback() int {
  491  	return f.fallback
  492  }
  493  
  494  func NewIntFlag(name string, fallback int) IntFlag {
  495  	flag := IntFlag{name: name, fallback: fallback}
  496  	builder := launchDarklyOfflineStore.Flag(flag.name).ValueForAll(ldvalue.Int(fallback))
  497  	launchDarklyOfflineStore.Update(builder)
  498  
  499  	return flag
  500  }
````

`packages/shared/pkg/featureflags/flags.go` L887-L910:

````go
  887  type StringFlag struct {
  888  	name     string
  889  	fallback string
  890  }
  891  
  892  func (f StringFlag) Key() string {
  893  	return f.name
  894  }
  895  
  896  func (f StringFlag) String() string {
  897  	return f.name
  898  }
  899  
  900  func (f StringFlag) Fallback() string {
  901  	return f.fallback
  902  }
  903  
  904  func NewStringFlag(name string, fallback string) StringFlag {
  905  	flag := StringFlag{name: name, fallback: fallback}
  906  	builder := launchDarklyOfflineStore.Flag(flag.name).ValueForAll(ldvalue.String(fallback))
  907  	launchDarklyOfflineStore.Update(builder)
  908  
  909  	return flag
  910  }
````

`packages/shared/pkg/featureflags/client.go` L21-L112:

````go
   21  // launchDarklyOfflineStore is a test fixture that provides dynamically updatable feature flag state
   22  var launchDarklyOfflineStore = ldtestdata.DataSource()
   23  
   24  var launchDarklyApiKey = os.Getenv("LAUNCH_DARKLY_API_KEY")
   25  
   26  const waitForInit = 5 * time.Second
   27  
   28  type Client struct {
   29  	ld               *ldclient.LDClient
   30  	static           bool
   31  	deploymentName   string
   32  	serviceName      string
   33  	contextProviders []ContextProvider
   34  }
   35  
   36  // ContextProvider supplies an additional LD context on every flag evaluation.
   37  // Services register providers to inject specific contexts without leaking that
   38  // specificity into the shared client.
   39  type ContextProvider func(ctx context.Context) ldcontext.Context
   40  
   41  func NewClientWithDatasource(source *ldtestdata.TestDataSource) (*Client, error) {
   42  	ldClient, err := ldclient.MakeCustomClient(
   43  		"",
   44  		ldclient.Config{
   45  			DataSource: source,
   46  			// Disable all outbound network traffic: no analytics events and no
   47  			// diagnostic events. The SDK key is empty here so any network call
   48  			// would fail and produce noise anyway.
   49  			Events:           ldcomponents.NoEvents(),
   50  			DiagnosticOptOut: true,
   51  		},
   52  		0)
   53  	if err != nil {
   54  		return nil, err
   55  	}
   56  
   57  	return &Client{ld: ldClient}, nil
   58  }
   59  
   60  func NewClient() (*Client, error) {
   61  	if launchDarklyApiKey == "" {
   62  		c, err := NewClientWithDatasource(launchDarklyOfflineStore)
   63  		if err != nil {
   64  			return nil, err
   65  		}
   66  		c.static = true
   67  
   68  		return c, nil
   69  	}
   70  
   71  	ldClient, err := ldclient.MakeClient(launchDarklyApiKey, waitForInit)
   72  	if err != nil {
   73  		return nil, err
   74  	}
   75  
   76  	return &Client{ld: ldClient}, nil
   77  }
   78  
   79  // NewClientWithLogLevel creates a client with a specific log level.
   80  // Use ldlog.Error to suppress INFO/WARN logs in CLI tools.
   81  func NewClientWithLogLevel(logLevel ldlog.LogLevel) (*Client, error) {
   82  	cfg := ldclient.Config{
   83  		Logging: ldcomponents.Logging().MinLevel(logLevel),
   84  	}
   85  
   86  	if launchDarklyApiKey == "" {
   87  		cfg.DataSource = launchDarklyOfflineStore
   88  		// Disable all outbound network traffic when no API key is configured.
   89  		cfg.Events = ldcomponents.NoEvents()
   90  		cfg.DiagnosticOptOut = true
   91  		ldClient, err := ldclient.MakeCustomClient("", cfg, 0)
   92  		if err != nil {
   93  			return nil, err
   94  		}
   95  
   96  		return &Client{ld: ldClient, static: true}, nil
   97  	}
   98  
   99  	ldClient, err := ldclient.MakeCustomClient(launchDarklyApiKey, cfg, waitForInit)
  100  	if err != nil {
  101  		return nil, err
  102  	}
  103  
  104  	return &Client{ld: ldClient}, nil
  105  }
  106  
  107  // Live reports whether flag values can change at runtime. A client built
  108  // without an API key serves the offline store's values for the life of the
  109  // process, and a nil client serves fallbacks.
  110  func (c *Client) Live() bool {
  111  	return c != nil && c.ld != nil && !c.static
  112  }
````

### P5.2 Requested flag definitions

`packages/shared/pkg/featureflags/flags.go` L157-L325:

````go
  157  var (
  158  	SnapshotFeatureFlag                 = NewBoolFlag("use-nfs-for-snapshots", env.IsDevelopment())
  159  	TemplateFeatureFlag                 = NewBoolFlag("use-nfs-for-templates", env.IsDevelopment())
  160  	EnableWriteThroughCacheFlag         = NewBoolFlag("write-to-cache-on-writes", false)
  161  	UseNFSCacheForBuildingTemplatesFlag = NewBoolFlag("use-nfs-for-building-templates", env.IsDevelopment())
  162  	CreateStorageCacheSpansFlag         = NewBoolFlag("create-storage-cache-spans", env.IsDevelopment())
  163  	OrchAcceptsCombinedHostFlag         = NewBoolFlag("orch-accepts-combined-host", false)
  164  	WorkspacesEnabledFlag               = NewBoolFlag("workspacesEnabled", false)
  165  
  166  	// FsFreezeViaExecFlag freezes the guest rootfs with `fsfreeze -f /` run through
  167  	// the envd exec API before a filesystem-only pause, for guests whose envd
  168  	// predates the native /fsfreeze endpoint. Off = those guests fall back to a
  169  	// plain guest sync (today's behavior). Falls back to sync per-pause if the
  170  	// guest lacks fsfreeze or the freeze fails.
  171  	FsFreezeViaExecFlag = NewBoolFlag("fsfreeze-via-exec", false)
  172  
  173  	// FsOnlyResumeAPIFlag accepts memory:false on resume/connect of a
  174  	// memory-inclusive snapshot (an explicit cold-boot rescue). Off = the
  175  	// request is rejected, never silently downgraded to a memory restore.
  176  	FsOnlyResumeAPIFlag = NewBoolFlag("fs-only-resume-api", false)
  177  
  178  	// PrebootFsRecoveryFlag runs a jailed filesystem recovery before every cold
  179  	// boot of a rootfs that was not frozen at pause (fs_quiesced absent/false).
  180  	// Separate from FsOnlyResumeAPIFlag because it also changes the behavior of
  181  	// existing filesystem-only cold boots whose pause fell back to sync.
  182  	PrebootFsRecoveryFlag = NewBoolFlag("preboot-fs-recovery", false)
  183  
  184  	// StorageSoftDeleteCheckFlag enables reading the storage-index soft-delete
  185  	// tombstone on header load (one extra GCS Attrs on cold load). Off = no overhead.
  186  	StorageSoftDeleteCheckFlag = NewBoolFlag("storage-soft-delete-check", false)
  187  	// StorageSoftDeleteEnforceFlag makes a soft-deleted object fail the read
  188  	// (fail closed) instead of only emitting a metric + log. Requires the check flag.
  189  	StorageSoftDeleteEnforceFlag = NewBoolFlag("storage-soft-delete-enforce", false)
  190  
  191  	// UseMemFdFlag asks Firecracker to back guest memory with a memfd and
  192  	// pass the fd over the UFFD socket; the orchestrator then mmaps it
  193  	// directly instead of using process_vm_readv on pause.
  194  	UseMemFdFlag = NewBoolFlag("use-memfd", true)
  195  
  196  	// UseSyncWPFlag asks Firecracker (via use_sync_wp on snapshot load) to
  197  	// register guest memory for SYNCHRONOUS userfault write-protect events,
  198  	// which the orchestrator's serve loop resolves, instead of the kernel's
  199  	// in-place WP_ASYNC clears. Foundation for the copy-on-write background
  200  	// memory snapshot. Default off = WP_ASYNC, today's behavior. Enable only
  201  	// where the deployed FC accepts the use_sync_wp field: FC rejects unknown
  202  	// fields on snapshot load, so a mismatch fails the resume loudly.
  203  	UseSyncWPFlag = NewBoolFlag("use-sync-wp", false)
  204  
  205  	// InPlaceCheckpointFlag makes Checkpoint pause, snapshot and resume the
  206  	// SAME Firecracker process (in-place) instead of resuming a fresh sandbox
  207  	// from the new build. Only honored for sandboxes resumed with
  208  	// UseSyncWPFlag on: in-place skips the snapshot re-load that re-arms
  209  	// write-protection, so dirty tracking across repeated checkpoints relies
  210  	// on the sync-WP serve loop; resume-fresh stays the fallback for async
  211  	// sandboxes and when this flag is off.
  212  	InPlaceCheckpointFlag = NewBoolFlag("in-place-checkpoint", false)
  213  
  214  	// InPlaceCheckpointReportingFlag re-admits to the in-place checkpoint the
  215  	// sandboxes whose balloon runs free-page reporting, and those whose
  216  	// balloon could not be read. It only decides where DeferMemoryExportFlag
  217  	// is on: there the CoW window pauses reporting and the deferred reports
  218  	// drain onto the serve loop when it resumes, so off (the default) sends
  219  	// both cohorts resume-fresh, and on lets them in place for a per-team A/B
  220  	// of the two paths without a redeploy. Where the deferred export is off
  221  	// the in-place checkpoint takes the synchronous copy, never touches
  222  	// reporting, and every balloon goes in place regardless of this flag.
  223  	InPlaceCheckpointReportingFlag = NewBoolFlag("in-place-checkpoint-reporting", false)
  224  
  225  	// DeferMemoryExportFlag makes the in-place checkpoint export guest
  226  	// memory through the CoW window instead of the synchronous dirty-RAM
  227  	// copy: the dirty set is write-protect-armed while the VM is paused, the
  228  	// guest resumes immediately, and pages are captured in the background
  229  	// (first guest write to an uncaptured page copies the pre-image before
  230  	// the write proceeds). Only takes effect on the in-place path with a
  231  	// sync-WP UFFD backend. When the VM's balloon runs continuous free-page
  232  	// REPORTING, reporting is PAUSED for the window's lifetime (a REMOVE
  233  	// zapping an uncaptured page would export zeros where pause-time content
  234  	// is owed); requires an FC build with /balloon/reporting — pause failures
  235  	// fall back to the synchronous copy. (The synchronous pre-pause
  236  	// free-page-hinting drain settles before the dirty readout and needs no
  237  	// pause.) Default off = today's synchronous copy. Because of that pause,
  238  	// this flag also moves the checkpoint ROUTE of reporting-built (and
  239  	// unread-balloon) sandboxes: turning it on sends them resume-fresh unless
  240  	// InPlaceCheckpointReportingFlag re-admits them, and turning it off
  241  	// mid-ramp sends them back in place. The route panel and the checkpoint
  242  	// counter's route label show the move.
  243  	DeferMemoryExportFlag = NewBoolFlag("defer-memory-export", false)
  244  
  245  	// SyncWPTrackerDirtyFlag derives the pause-time dirty set from the
  246  	// orchestrator's page tracker (installs + synchronous WP-fault
  247  	// promotions) instead of Firecracker's GetDirtyMemory pagemap scan,
  248  	// skipping that RPC entirely. Only consulted for sandboxes resumed with
  249  	// UseSyncWPFlag on — under WP_ASYNC the kernel clears protections
  250  	// in-place and the tracker never sees guest writes. Evaluated fresh at
  251  	// each pause, so flipping it off immediately reverts running sandboxes to
  252  	// the pagemap source (kill switch). Burn-in gate before enabling: the
  253  	// dirty-source divergence log (emitted while this flag is off) must
  254  	// show pagemap_only == 0 for sync-WP sandboxes — a nonzero count means
  255  	// the tracker missed a write and would corrupt the snapshot.
  256  	SyncWPTrackerDirtyFlag = NewBoolFlag("sync-wp-tracker-dirty", false)
  257  
  258  	// MemfdBackgroundCopyFlag streams the memfd into the snapshot cache on
  259  	// a goroutine so Pause returns as soon as the diff metadata is written.
  260  	// Only takes effect when UseMemFdFlag is also on.
  261  	MemfdBackgroundCopyFlag = NewBoolFlag("memfd-background-copy", true)
  262  
  263  	// MemfileDiffDedupFlag enables 4 KiB-page dedup of the memfile diff
  264  	// against the base memfile. bestEffort skips uncached blocks; directIO
  265  	// opens the dedup output with O_DIRECT. The remaining keys budget fetch
  266  	// defragmentation of the deduped diff — fetchRunWindowPages is the
  267  	// uncompressed frame/window size served per backing fetch — see
  268  	// orchestrator block.DedupBudget for semantics (0 = disabled/default).
  269  	MemfileDiffDedupFlag = NewJSONFlag("memfile-diff-dedup", ldvalue.FromJSONMarshal(map[string]any{
  270  		"enabled":                        false,
  271  		"bestEffort":                     false,
  272  		"directIO":                       false,
  273  		"maxFetchWindowsPerBlock":        0,
  274  		"maxPromotedParentPagesPerBlock": 0,
  275  		"maxPagesPerPromotedFrame":       0,
  276  		"blockFaultPct":                  0,
  277  		"fetchRunWindowPages":            0,
  278  	}))
  279  
  280  	// MemfdDedupInflightServeFlag lets a resume that overlaps an in-flight
  281  	// memfile dedup serve dirty pages straight from the still-mapped memfd
  282  	// instead of blocking until dedup finishes. It gates both windows: serving
  283  	// via a provisional local header while dedup is still computing the deduped
  284  	// header, and serving during the dedup drain before the compacted diff is
  285  	// ready. Only affects the memfd-dedup path; off restores the prior
  286  	// wait-for-dedup behavior.
  287  	MemfdDedupInflightServeFlag = NewBoolFlag("memfd-dedup-inflight-serve", false)
  288  
  289  	// MemfdDedupFreeIndexFlag frees the memfd dedup cache's packed index when
  290  	// its memfd is released. The index only translates in-flight drain reads
  291  	// to memfd offsets, which cannot be served once the memfd is gone, yet it
  292  	// otherwise stays on the cache for as long as the diff store keeps it.
  293  	// Only MemfdDedupInflightServeFlag pauses build an index, so elsewhere
  294  	// this flag has nothing to free.
  295  	MemfdDedupFreeIndexFlag = NewBoolFlag("memfd-dedup-free-index", false)
  296  
  297  	// SnapshotCacheDropProvisionalHeaderFlag drops the two long-lived holders
  298  	// of a pause's provisional memfile header: the local template's header
  299  	// holder, cleared once Fetch has read it to build the memfile device, and the
  300  	// snapshot the upload keeps, cleared as the upload is created. The device
  301  	// keeps its own reference until the deduped or the published header
  302  	// replaces it. Only MemfdDedupInflightServeFlag pauses build a provisional
  303  	// header.
  304  	SnapshotCacheDropProvisionalHeaderFlag = NewBoolFlag("snapshot-cache-drop-provisional-header", false)
  305  
  306  	// PeerToPeerChunkTransferFlag enables peer-to-peer chunk routing.
  307  	PeerToPeerChunkTransferFlag = NewBoolFlag("peer-to-peer-chunk-transfer", false)
  308  	// PeerToPeerAsyncCheckpointFlag makes Checkpoint upload fire-and-forget instead
  309  	// of synchronous. Only safe to enable after PeerToPeerChunkTransferFlag is ON.
  310  	PeerToPeerAsyncCheckpointFlag = NewBoolFlag("peer-to-peer-async-checkpoint", false)
  311  
  312  	// DeferRootfsExportFlag moves the rootfs diff seal (the reflink, which forces a
  313  	// synchronous host->NVMe writeback) off the pause critical path. On the
  314  	// suspend (pause) path, pause() ejects the cache and stops the sandbox, then
  315  	// reflinks the diff in the background — nothing reads the diff until a later
  316  	// resume. On the in-place checkpoint path, pause() swaps a fresh writable
  317  	// cache in, resumes the VM, seals the frozen old cache in the background and
  318  	// folds it back into the writable cache when done. Off by default; falls
  319  	// back to the synchronous export when off or on a non-NBD provider.
  320  	DeferRootfsExportFlag = NewBoolFlag("defer-rootfs-export", false)
  321  
  322  	PersistentVolumesFlag           = NewBoolFlag("can-use-persistent-volumes", env.IsDevelopment())
  323  	SandboxLabelBasedSchedulingFlag = NewBoolFlag("sandbox-label-based-scheduling", false)
  324  	FreePageReportingFlag           = NewBoolFlag("free-page-reporting", false)
  325  	FreezeUserCgroupFlag            = NewBoolFlag("freeze-user-cgroup", env.IsDevelopment())
````

`packages/shared/pkg/featureflags/flags.go` L502-L523:

````go
  502  var (
  503  	MaxSandboxesPerNode = NewIntFlag("max-sandboxes-per-node", 200)
  504  	// The LD keys keep the legacy "gcloud-" prefix, but the limits apply to uploads on all storage providers.
  505  	StorageConcurrentUploadLimit  = NewIntFlag("gcloud-concurrent-upload-limit", 8)
  506  	StorageMaxUploadTasks         = NewIntFlag("gcloud-max-tasks", 16)
  507  	ClickhouseBatcherMaxBatchSize = NewIntFlag("clickhouse-batcher-max-batch-size", 1000)
  508  	ClickhouseBatcherMaxDelay     = NewIntFlag("clickhouse-batcher-max-delay", 1000) // 1s in milliseconds
  509  	ClickhouseBatcherQueueSize    = NewIntFlag("clickhouse-batcher-queue-size", 1000)
  510  	BestOfKSampleSize             = NewIntFlag("best-of-k-sample-size", 3)                           // Default K=3
  511  	BestOfKMaxOvercommit          = NewIntFlag("best-of-k-max-overcommit", 400)                      // Default R=4 (stored as percentage, max over-commit ratio)
  512  	BestOfKAlpha                  = NewIntFlag("best-of-k-alpha", 50)                                // Default Alpha=0.5 (stored as percentage for int flag, current usage weight)
  513  	EnvdInitTimeoutMilliseconds   = NewIntFlag("envd-init-request-timeout-milliseconds", 50)         // Timeout for envd init request in milliseconds
  514  	EnvdTimeoutMilliseconds       = NewIntFlag("envd-timeout-milliseconds", envdTimeoutFallbackMs()) // Timeout for waiting for envd on resume; falls back to ENVD_TIMEOUT env var (default 10s)
  515  	// GuestSyncTimeoutMs overrides the mandatory pre-pause guest-sync deadline
  516  	// for filesystem-only snapshots, in milliseconds. 0 (default) derives the
  517  	// timeout from guest RAM; a positive value pins it.
  518  	GuestSyncTimeoutMs = NewIntFlag("guest-sync-timeout-milliseconds", 0)
  519  	// PauseAdmissionGraceMs gates the pause/checkpoint snapshot-admission
  520  	// pre-flight, in milliseconds. Negative (default) disables the pre-flight;
  521  	// 0 probes the parent header's readiness without waiting; a positive value
  522  	// waits up to that long before refusing retryably.
  523  	PauseAdmissionGraceMs = NewIntFlag("pause-admission-grace-milliseconds", -1)
````

`packages/shared/pkg/featureflags/flags.go` L535-L536:

````go
  535  	PauseRefusalRestoreFlag       = NewBoolFlag("pause-refusal-restore", false)
  536  	MaxCacheWriterConcurrencyFlag = NewIntFlag("max-cache-writer-concurrency", 10)
````

`packages/shared/pkg/featureflags/flags.go` L559-L660:

````go
  559  	// NBDConnectionsPerDevice the number of NBD socket connections per device
  560  	NBDConnectionsPerDevice = NewIntFlag("nbd-connections-per-device", 1)
  561  
  562  	// NBDAsyncWriteZeroesFlag, when enabled, handles NBD WRITE_ZEROES/TRIM
  563  	// commands in a goroutine instead of inline on the dispatch read loop.
  564  	// Inline handling can stall the read loop via head-of-line blocking on the
  565  	// shared write lock (when a reply writer is blocked on a full socket send
  566  	// buffer), which makes the kernel time out the NBD connection and surfaces
  567  	// as guest I/O errors. Disabled by default.
  568  	NBDAsyncWriteZeroesFlag = NewBoolFlag("nbd-async-write-zeroes", false)
  569  
  570  	// MemoryPrefetchMaxFetchWorkers is the maximum number of parallel fetch workers per sandbox for memory prefetching.
  571  	// Fetching is I/O bound so we can have more parallelism.
  572  	MemoryPrefetchMaxFetchWorkers = NewIntFlag("memory-prefetch-max-fetch-workers", 16)
  573  
  574  	// MemoryPrefetchMaxCopyWorkers is the maximum number of parallel copy workers per sandbox for memory prefetching.
  575  	// Copy uses uffd syscalls, so we limit parallelism to avoid overwhelming the system.
  576  	MemoryPrefetchMaxCopyWorkers = NewIntFlag("memory-prefetch-max-copy-workers", 8)
  577  
  578  	// MemoryPrefetchCoalesceMaxMB caps how many contiguous prefetch blocks are
  579  	// merged into a single source.Slice fetch (in MiB of extent size). 0
  580  	// disables coalescing: every block is fetched individually, matching
  581  	// today's behavior. The copy phase is unaffected either way — it always
  582  	// installs one page at a time, because Userfaultfd.Prefault installs a
  583  	// single page per call.
  584  	MemoryPrefetchCoalesceMaxMB = NewIntFlag("memory-prefetch-coalesce-max-mb", 0)
  585  
  586  	// ResumePrefetchSourceFlag selects which trace the resume prefetcher
  587  	// replays:
  588  	//   "init"     — only the build-time / harvested read-hot init trace
  589  	//                (meta.Prefetch.Memory), prefaulted. Preserves today's
  590  	//                behavior, so this is the default and a no-op-equivalent.
  591  	//   "last-cycle" — only the sandbox's own pause diff (the pages the last
  592  	//                resume→pause cycle wrote), derived from the memfile header
  593  	//                and replayed fetch-only.
  594  	//   "both"     — init first (prefaulted), then last-cycle (fetch-only) behind
  595  	//                a barrier, so the large last-cycle fetch stays off the
  596  	//                resume-critical path.
  597  	//   "off"      — kill switch, no resume prefetch.
  598  	// Unknown values fall back to "init".
  599  	ResumePrefetchSourceFlag = NewStringFlag("resume-prefetch-source", "init")
  600  
  601  	// ResumeLastCyclePrefetchMaxMiBFlag caps how much of the last-cycle diff a single
  602  	// resume prefetches, in MiB. -1 (the default, negative = no limit per the
  603  	// codebase convention) is uncapped; the recorded diff is small by
  604  	// construction, so this exists to throttle the heavy-churn tail against the
  605  	// shared object-store pool without a redeploy. A non-negative N keeps the
  606  	// first N MiB of blocks in offset order and leaves the rest to demand-fault.
  607  	ResumeLastCyclePrefetchMaxMiBFlag = NewIntFlag("resume-last-cycle-prefetch-max-mib", -1)
  608  
  609  	// PauseResumePrefetchHarvestFlag makes the orchestrator, after a pause
  610  	// snapshot is durable, run a throwaway warm resume of the just-written
  611  	// artifact (driven by envd /init, workload frozen, egress denied) to record
  612  	// the resume page-fault trace and turn it into a prefetch mapping. Off by
  613  	// default; the harvest is best-effort and never affects the pause result.
  614  	PauseResumePrefetchHarvestFlag = NewBoolFlag("pause-resume-prefetch-harvest", false)
  615  
  616  	// PauseResumePrefetchConsumeFlag controls whether a harvested mapping is
  617  	// persisted into the pause artifact metadata (and therefore replayed on the
  618  	// customer's next resume). When off, the harvest still runs and emits its
  619  	// trace-size metrics but does NOT write the mapping, so resumes are
  620  	// unaffected — letting us validate harvest behaviour with no customer-visible
  621  	// change before enabling prefetch on resume. Off by default.
  622  	PauseResumePrefetchConsumeFlag = NewBoolFlag("pause-resume-prefetch-consume", false)
  623  
  624  	// PauseResumePrefetchHarvestTimeoutMsFlag bounds the throwaway harvest resume
  625  	// (slot-hold cap), in milliseconds. The harvest is best-effort: a cut-short
  626  	// run is discarded (the build is simply re-harvested on its next pause), so
  627  	// erring short is cheap. A normal warm harvest completes in a few seconds; the
  628  	// default leaves headroom for a large warm resume to fully drain while keeping
  629  	// the worst-case slot hold modest. Tunable per rollout via LD; the fallback
  630  	// (returned when LD is unavailable or the flag is unset) is the default.
  631  	PauseResumePrefetchHarvestTimeoutMsFlag = NewIntFlag("pause-resume-prefetch-harvest-timeout-ms", 15000) // 15s
  632  
  633  	// TCPFirewallMaxConnectionsPerSandbox is the maximum number of concurrent TCP firewall
  634  	// connections allowed per sandbox. Negative means no limit.
  635  	TCPFirewallMaxConnectionsPerSandbox = NewIntFlag("tcpfirewall-max-connections-per-sandbox", -1)
  636  
  637  	// SandboxMaxIncomingConnections is the maximum number of concurrent HTTP proxy
  638  	// connections allowed per sandbox. Negative means no limit.
  639  	SandboxMaxIncomingConnections = NewIntFlag("sandbox-max-incoming-connections", -1)
  640  
  641  	// BuildBaseRootfsSizeLimitMB is the maximum size of the base rootfs filesystem created from the OCI image, in MB.
  642  	BuildBaseRootfsSizeLimitMB = NewIntFlag("build-base-rootfs-size-limit-mb", 25000)
  643  
  644  	// MinAutoResumeTimeoutSeconds is the minimum auto-resume timeout in seconds.
  645  	// This prevents thrashing from very short timeouts.
  646  	MinAutoResumeTimeoutSeconds = NewIntFlag("minimum-autoresume-timeout", 300)
  647  
  648  	// BuildReservedDiskSpaceMB is the amount of disk space in MB reserved for root on the guest filesystem.
  649  	// Reserved blocks are only usable by root (uid 0), protecting the guest OS from disk-full conditions.
  650  	BuildReservedDiskSpaceMB = NewIntFlag("build-reserved-disk-space-mb", 256)
  651  
  652  	// MaxStartingInstancesPerNode limits concurrent sandbox start/resume operations on a single orchestrator node.
  653  	// Must be > 0.
  654  	MaxStartingInstancesPerNode = NewIntFlag("max-starting-instances-per-node", 3)
  655  
  656  	// MaxConcurrentEvictions caps the number of sandbox evictions that can run
  657  	// in parallel per API instance. Excess items remain expired in the store
  658  	// and are picked up by the next eviction tick. Must be > 0; non-positive
  659  	// values are ignored at refresh time.
  660  	MaxConcurrentEvictions = NewIntFlag("max-concurrent-evictions", 256)
````

`packages/shared/pkg/featureflags/flags.go` L693-L705:

````go
  693  var ReclaimConfigFlag = NewJSONFlag("guest-pause-reclaim", ldvalue.Null())
  694  
  695  // FreePageHintingConfig controls virtio-balloon free-page-hinting.
  696  // "enabled" configures FreePageHinting=true on the balloon at install time
  697  // (kernel-side eligibility is targeted separately via the LD context — the
  698  // race fixed in https://lore.kernel.org/lkml/20240429125100.7393-1-david@redhat.com/
  699  // is on the hinting flow, gated by the per-use-case timeouts below).
  700  // "pause"/"build" are pre-pause drain timeouts in ms keyed by SnapshotUseCase;
  701  // missing/zero/negative disables the drain for that use case. "stop" and
  702  // "stop_grace" bound the stop of a cycle that outlived its budget (see
  703  // GetPrePauseHintConfig).
  704  // Example: {"enabled": true, "pause": 500, "build": 0, "stop": 2000, "stop_grace": 100}
  705  var FreePageHintingConfig = NewJSONFlag("free-page-hinting-config", ldvalue.Null())
````

`packages/shared/pkg/featureflags/flags.go` L911-L965:

````go
  911  
  912  const (
  913  	DefaultKernelVersion = "vmlinux-6.1.158"
  914  
  915  	// DefaultEnvdVersion is the envd new template builds bake when neither the
  916  	// build-envd-version flag nor DEFAULT_ENVD_VERSION says otherwise:
  917  	// "promoted" selects the node-local promoted binary (HOST_ENVD_PATH), the
  918  	// behavior every build has always had, so deployments without
  919  	// LaunchDarkly (dev, self-host) are unaffected.
  920  	DefaultEnvdVersion = "promoted"
  921  )
  922  
  923  // The Firecracker version per release line: legacy lines pin
  924  // last-tag_short-SHA dev builds; e2b lines (vX.Y-<e2b-major>) pin releases
  925  // published by the Publish fc-versions workflow.
  926  // TODO: The short tag here has only 7 characters — the one from our build pipeline will likely have exactly 8 so this will break.
  927  const (
  928  	DefaultFirecrackerV1_10Version = "v1.10.1_30cbb07"
  929  	DefaultFirecrackerV1_12Version = "v1.12.1_210cbac"
  930  	DefaultFirecrackerV1_14Version = "v1.14.1_431f1fc"
  931  	// The v1.14-0 release line. 0.2.0 introduces the in-place checkpoint's
  932  	// balloon reporting-pause API; filesystem-only snapshots ship with every
  933  	// e2b release from 0.1.0 — the per-feature floors live in fcversion.
  934  	DefaultFirecrackerV1_14_0Version = "v1.14-0.2.0"
  935  	// New template builds get the current release; existing builds keep
  936  	// resolving within their own line below (cross-line upgrades are an
  937  	// operator decision via the firecracker-versions flag, never a baked
  938  	// default — the map invariant key == LDKey(value) enforces it).
  939  	DefaultFirecrackerVersion = DefaultFirecrackerV1_14_0Version
  940  )
  941  
  942  var FirecrackerVersionMap = map[string]string{
  943  	"v1.10":   DefaultFirecrackerV1_10Version,
  944  	"v1.12":   DefaultFirecrackerV1_12Version,
  945  	"v1.14":   DefaultFirecrackerV1_14Version,
  946  	"v1.14-0": DefaultFirecrackerV1_14_0Version,
  947  }
  948  
  949  // BuildIoEngine Sync is used by default as there seems to be a bad interaction between Async and a lot of io operations.
  950  var (
  951  	BuildFirecrackerVersion = NewStringFlag("build-firecracker-version", env.GetEnv("DEFAULT_FIRECRACKER_VERSION", DefaultFirecrackerVersion))
  952  	BuildKernelVersion      = NewStringFlag("build-kernel-version", env.GetEnv("DEFAULT_KERNEL_VERSION", DefaultKernelVersion))
  953  	// BuildEnvdVersion selects which staged envd binary a template build bakes
  954  	// into the rootfs — the envd counterpart of BuildKernelVersion /
  955  	// BuildFirecrackerVersion, same default mechanism. "promoted" (the
  956  	// fallback) is the node-local promoted binary; a concrete version id
  957  	// (e.g. v0.7.0, or a git SHA while those age out) selects a staged binary
  958  	// (the flat envd.<id> sibling or the release bucket's <id>/envd layout,
  959  	// see build/core/envd.ResolveBuildBinary). A pinned target that is not
  960  	// staged FAILS the build rather than silently baking a different envd —
  961  	// feature gates key on the baked version, so a silent substitute
  962  	// misgates. The build-site LD context carries template/team, so cohort
  963  	// canaries come for free.
  964  	BuildEnvdVersion = NewStringFlag("build-envd-version", env.GetEnv("DEFAULT_ENVD_VERSION", DefaultEnvdVersion))
  965  	BuildIoEngine    = NewStringFlag("build-io-engine", "Sync")
````

`packages/shared/pkg/featureflags/flags.go` L1001-L1071:

````go
 1001  	// EnvdUpgradeTargetFlag drives the resume-time envd live-upgrade.
 1002  	// Multivariate string:
 1003  	//   "off"        (fallback) — no upgrade; dev has no LD so this is inert & safe.
 1004  	//   "promoted"   — track the node-local promoted envd (HOST_ENVD_PATH); upgrade
 1005  	//                  whenever it differs from the sandbox's built-with version
 1006  	//                  (no per-publish flag edits needed).
 1007  	//   "<version>"  — pin a specific staged binary, in either layout beside the
 1008  	//                  promoted one: the flat /fc-envd/envd.<version> sibling or
 1009  	//                  the release bucket's /fc-envd/<version>/envd directory.
 1010  	//                  Release names may carry dots and hyphens (v0.7.0,
 1011  	//                  v0.8.0-rc1); legacy git-SHA suffixes keep resolving while
 1012  	//                  the old envd.<sha> objects age out.
 1013  	// The resume-site LD context carries envd-version/team/template, so %-ramp
 1014  	// and cohort canaries come for free. The fallback is env-overridable
 1015  	// (ENVD_UPGRADE_TARGET) so it can be exercised where there is no LD (dev),
 1016  	// mirroring build-firecracker-version's DEFAULT_FIRECRACKER_VERSION.
 1017  	EnvdUpgradeTargetFlag = NewStringFlag("envd-upgrade-target", env.GetEnv("ENVD_UPGRADE_TARGET", "off"))
 1018  	// EnvdOfflineUpgradeTargetFlag drives the OFFLINE envd upgrade of a
 1019  	// filesystem-only snapshot: at cold-boot resume the rootfs binary is rewritten
 1020  	// (jailed debugfs) before the guest boots, reaching envd too old to self-upgrade
 1021  	// (< MinEnvdVersionForUpgrade). Same value grammar and resolver as
 1022  	// EnvdUpgradeTargetFlag ("off" / "promoted" / "<version>"); a SEPARATE flag so
 1023  	// the newer/riskier offline mechanism ramps independently of the live path. The
 1024  	// fallback is env-overridable (ENVD_OFFLINE_UPGRADE_TARGET) for dev, where there
 1025  	// is no LD. Default off.
 1026  	EnvdOfflineUpgradeTargetFlag = NewStringFlag("envd-offline-upgrade-target", env.GetEnv("ENVD_OFFLINE_UPGRADE_TARGET", "off"))
 1027  
 1028  	// EnvdBinaryCacheFlag serves the envd upgrade paths from a node-local copy of
 1029  	// the host envd binary instead of re-reading it from the gcsfuse mount: the
 1030  	// version probe, the live delivery to a running envd, and the offline rootfs
 1031  	// swap's staging copy. Off keeps every one of those reading the source
 1032  	// directly, which is the behaviour before this flag existed.
 1033  	//
 1034  	// It gates a caching optimisation, not a new mechanism, so the risk it
 1035  	// isolates is the cache serving a stale binary — an upgrade delivering
 1036  	// something other than the version it recorded. That is recoverable (the
 1037  	// version arbiter refuses to record a success it cannot confirm) but worth a
 1038  	// kill switch that needs no deploy. Falls back on in development, where there
 1039  	// is no LD and the path would otherwise never be exercised.
 1040  	// The fallback is env-overridable (ENVD_BINARY_CACHE) so the cached and
 1041  	// uncached paths can be compared on a cluster with no LaunchDarkly, where the
 1042  	// alternative is two binary deploys.
 1043  	//
 1044  	// Ramp it on a NODE-SCOPED key only: deployment (the domain, so effectively the
 1045  	// cluster), instance-group, or orchestrator (the node, carrying its build as an
 1046  	// attribute). Those are what the orchestrator attaches to every evaluation, so
 1047  	// all three read sites agree on them. A sandbox- or team-keyed rule is absent
 1048  	// at the startup warm, which runs before any sandbox exists -- such a rule
 1049  	// still gates both upgrade paths but silently disables the pre-warm, so every
 1050  	// node's first eligible resume defers. A template-keyed rule matches the
 1051  	// offline path only, since the live path carries the template as an attribute
 1052  	// of the sandbox context rather than as a context of its own. None of that is a
 1053  	// limitation worth lifting: the subject of this flag is a cache shared by every
 1054  	// sandbox on the node, so the node, its group and its cluster are the cohorts
 1055  	// that mean anything.
 1056  	EnvdBinaryCacheFlag = NewBoolFlag("envd-binary-cache", envBoolOr("ENVD_BINARY_CACHE", env.IsDevelopment()))
 1057  	// FsOnlyResumeCPUModelFlag restricts where a filesystem-only snapshot may be
 1058  	// resumed: placement keeps only nodes reporting this CPU model, on top of the
 1059  	// build-compatibility rule every sandbox is already subject to. The value is
 1060  	// a bare CPU model as /proc/cpuinfo reports it — machineinfo.IceLakeModel is
 1061  	// "106" (n2), machineinfo.EmeraldRapidsModel is "207" (n4).
 1062  	//
 1063  	// Empty (the default) turns the restriction off, leaving filesystem-only
 1064  	// snapshots on the cross-generation rule a memory restore uses. A deployment
 1065  	// with no LaunchDarkly therefore keeps placing them exactly as before, rather
 1066  	// than needing an LD rule to unpin itself. Memory snapshots never read it.
 1067  	FsOnlyResumeCPUModelFlag = NewStringFlag("fs-only-resume-cpu-model", "")
 1068  
 1069  	DefaultPersistentVolumeType = NewStringFlag("default-persistent-volume-type", "")
 1070  	BuildNodeInfo               = NewJSONFlag("preferred-build-node", ldvalue.Null())
 1071  	FirecrackerVersions         = NewJSONFlag("firecracker-versions", ldvalue.FromJSONMarshal(FirecrackerVersionMap))
````

`packages/shared/pkg/featureflags/flags.go` L1424-L1457:

````go
 1424  func ResolveFirecrackerVersion(ctx context.Context, ff *Client, buildVersion string) string {
 1425  	info, err := fcversion.New(buildVersion)
 1426  	if err != nil {
 1427  		recordFirecrackerFallback(ctx, "parse_error", "")
 1428  
 1429  		return buildVersion
 1430  	}
 1431  
 1432  	key, ok := info.LDKey()
 1433  	if !ok {
 1434  		recordFirecrackerFallback(ctx, "no_ld_key", "")
 1435  
 1436  		return buildVersion
 1437  	}
 1438  
 1439  	versions := ff.JSONFlag(ctx, FirecrackerVersions).AsValueMap()
 1440  
 1441  	if resolved, ok := versions.Get(key).AsOptionalString().Get(); ok {
 1442  		// An empty map value would blank the binary path fleet-wide; serve
 1443  		// the stored version instead and make the misconfiguration loud.
 1444  		if resolved == "" {
 1445  			recordFirecrackerFallback(ctx, "empty_value", key)
 1446  
 1447  			return buildVersion
 1448  		}
 1449  		recordFirecrackerResolved(ctx, key, resolved)
 1450  
 1451  		return resolved
 1452  	}
 1453  
 1454  	recordFirecrackerFallback(ctx, "key_absent", key)
 1455  
 1456  	return buildVersion
 1457  }
````

Summary table (name — default — notes):

| Flag key | Default | Notes |
|---|---|---|
| `max-sandboxes-per-node` | 200 | read at server.New and every 30 s into `ServiceInfo.MaxSandboxes`; Create returns ResourceExhausted at >= |
| `max-starting-instances-per-node` | 3 | semaphore; fresh creates fail fast, resumes (and Checkpoint's resume) wait up to 15 s (`acquireTimeout`, server/utils.go L14-L28) |
| `in-place-checkpoint` | false | Checkpoint pauses/snapshots/resumes the same FC; only for sandboxes resumed with `use-sync-wp` |
| `use-sync-wp` | false | requires an FC that accepts `use_sync_wp` (the e2b `v1.14-0.x` line) |
| `defer-memory-export` | false | in-place CoW export; needs FC `/balloon/reporting` (v1.14-0.2.0) |
| `sync-wp-tracker-dirty` | false | tracker-derived dirty set |
| `use-memfd` | true | memfd-backed guest memory |
| `memfd-background-copy` | true | |
| `memfile-diff-dedup` | JSON `{"enabled":false,...}` | |
| `resume-prefetch-source` | "init" | init/last-cycle/both/off |
| `pause-resume-prefetch-harvest` / `-consume` | false / false | |
| `memory-prefetch-max-fetch-workers` / `-copy-workers` | 16 / 8 | |
| `free-page-reporting` | false | balloon |
| `free-page-hinting-config` | JSON null | |
| `guest-pause-reclaim` | JSON null | |
| `defer-rootfs-export` | false | |
| `build-firecracker-version` | env `DEFAULT_FIRECRACKER_VERSION` or `v1.14-0.2.0` | |
| `build-kernel-version` | env `DEFAULT_KERNEL_VERSION` or `vmlinux-6.1.158` | |
| `build-envd-version` | env `DEFAULT_ENVD_VERSION` or `promoted` | |
| `firecracker-versions` | JSON `FirecrackerVersionMap` | maps release line -> concrete version at resume |
| `tcpfirewall-max-connections-per-sandbox` | -1 | |
| `sandbox-max-incoming-connections` | -1 | |
| `envd-timeout-milliseconds` | `ENVD_TIMEOUT` or 10000 | |

Hugepages are not a flag: they come per request (`SandboxConfig.huge_pages`, orchestrator.proto L16) and
need `vm.nr_hugepages` on the host (A1.2).

### P5.3 Overriding without LaunchDarkly

With `LAUNCH_DARKLY_API_KEY` empty, `NewClient` serves `launchDarklyOfflineStore`, a
`ldtestdata.TestDataSource` whose values are the fallbacks registered by `NewBoolFlag`/`NewIntFlag`/...
(`c.static = true`, `Live()` false). There is **no generic env/file override** in the code. Existing
hooks: env-driven fallbacks (`DEFAULT_FIRECRACKER_VERSION`, `DEFAULT_KERNEL_VERSION`,
`DEFAULT_ENVD_VERSION`, `ENVD_UPGRADE_TARGET`, `ENVD_OFFLINE_UPGRADE_TARGET`, `ENVD_BINARY_CACHE`,
`ENVD_TIMEOUT`, `LOGS_READ_CONFIG`, and `ENVIRONMENT` via `env.IsDevelopment()`), and programmatic
`OverrideBoolFlag` / `OverrideJSONFlag` (flags.go L143-L155). Re-calling a constructor also overwrites
the store (as `cmd/resume-build` does):

`packages/orchestrator/cmd/resume-build/main.go` L106-L140:

````go
  106  	if *fphTimeoutMs > 0 {
  107  		featureflags.NewJSONFlag("free-page-hinting-config", ldvalue.FromJSONMarshal(map[string]any{
  108  			"enabled": true,
  109  			"pause":   *fphTimeoutMs,
  110  		}))
  111  	}
  112  
  113  	if *reclaim {
  114  		featureflags.NewJSONFlag("guest-pause-reclaim", ldvalue.FromJSONMarshal(map[string]int{
  115  			"sync":           500,
  116  			"drop_caches":    200,
  117  			"compact_memory": 1000,
  118  			"fstrim":         500,
  119  		}))
  120  	}
  121  
  122  	if *disableMemfd {
  123  		featureflags.OverrideBoolFlag(featureflags.UseMemFdFlag, false)
  124  	}
  125  
  126  	if *syncWP || *trackerDirty || *inPlace || *deferMemoryExport {
  127  		featureflags.OverrideBoolFlag(featureflags.UseSyncWPFlag, true)
  128  	}
  129  
  130  	if *inPlace || *deferMemoryExport {
  131  		featureflags.OverrideBoolFlag(featureflags.InPlaceCheckpointFlag, true)
  132  	}
  133  
  134  	if *deferMemoryExport {
  135  		featureflags.OverrideBoolFlag(featureflags.DeferMemoryExportFlag, true)
  136  	}
  137  
  138  	if *trackerDirty {
  139  		featureflags.OverrideBoolFlag(featureflags.SyncWPTrackerDirtyFlag, true)
  140  	}
````

**Minimal change point:** extend `applyTestFlagOverrides()` in `packages/orchestrator/main.go` (L29-L52;
it runs before `factories.Run`, hence before any flag is read) to apply operator overrides, e.g. from a
JSON file named by `E2B_FLAG_OVERRIDES_FILE` (`{"max-sandboxes-per-node": 50, "use-sync-wp": true, ...}`):
for each key, update the store with the value's type. Because `launchDarklyOfflineStore` is unexported,
add one exported helper in `packages/shared/pkg/featureflags/flags.go`, e.g.

```go
func OverrideFlagValue(name string, v ldvalue.Value) {
	launchDarklyOfflineStore.Update(launchDarklyOfflineStore.Flag(name).ValueForAll(v))
}
```

(`ValueForAll` works for bool/int/string/JSON; bool flags created with `VariationForAll` also evaluate
correctly from a boolean `ldvalue`). Overrides only take effect when `LAUNCH_DARKLY_API_KEY` is empty.
Values are fixed for the life of the process (static client), and the 30 s refresh loops keep returning
them.

## P6. ClickHouse

Files in `packages/orchestrator` mentioning ClickHouse (non-test Go): `pkg/cfg/model.go` (config fields),
`pkg/factories/run.go` (connections + writers), `pkg/sandbox/sandbox.go`, `pkg/sandbox/hoststats.go`,
`pkg/sandbox/hoststats_collector.go` (these three only import the `hoststats` *types/interface* package),
and the dev CLIs `cmd/create-build/main.go`, `cmd/resume-build/main.go` (not part of the server binary).

Guard — nothing is dialled when both strings are empty:

`packages/orchestrator/pkg/factories/run.go` L173-L241:

````go
  173  // openClickhouseEndpoints connects to every configured endpoint, returning them
  174  // with the closers that release them. One driver per endpoint rather than per
  175  // writer keeps connection pools, batcher queues, and OTel metrics isolated so a
  176  // slow/failing endpoint cannot stall the others.
  177  func openClickhouseEndpoints(ctx context.Context, config cfg.Config) ([]ClickhouseEndpoint, []closer) {
  178  	var (
  179  		endpoints []ClickhouseEndpoint
  180  		closers   []closer
  181  	)
  182  
  183  	// Legacy singular ClickHouse delivery path. Fatal on init error and keeps
  184  	// the unsuffixed default batcher names to preserve pre-multi-endpoint
  185  	// behavior and existing dashboards/alerts.
  186  	if config.ClickhouseConnectionString != "" {
  187  		conn, err := clickhouse.NewDriver(config.ClickhouseConnectionString)
  188  		if err != nil {
  189  			logger.L().Fatal(ctx, "failed to create clickhouse driver", zap.Error(err))
  190  		}
  191  
  192  		endpoints = append(endpoints, ClickhouseEndpoint{Conn: conn, Primary: true})
  193  		closers = append(closers, closer{"clickhouse connection", func(context.Context) error {
  194  			return conn.Close()
  195  		}})
  196  	}
  197  
  198  	additionalEndpoints, droppedDuplicates := config.AdditionalClickhouseEndpoints()
  199  	for _, dsn := range droppedDuplicates {
  200  		endpoint, err := clickhouse.EndpointFromDSN(dsn)
  201  		if err != nil {
  202  			logger.L().Info(ctx, "dropped duplicate unparseable ClickHouse endpoint", zap.Error(err))
  203  
  204  			continue
  205  		}
  206  		logger.L().Info(ctx, "dropped duplicate ClickHouse endpoint", zap.String("endpoint", endpoint))
  207  	}
  208  
  209  	if len(additionalEndpoints) > 0 {
  210  		logger.L().Info(ctx, "resolved additional ClickHouse delivery endpoints",
  211  			zap.Int("count", len(additionalEndpoints)),
  212  		)
  213  	}
  214  
  215  	// Additional ClickHouse delivery endpoints are best-effort.
  216  	for _, dsn := range additionalEndpoints {
  217  		label, err := clickhouse.EndpointFromDSN(dsn)
  218  		if err != nil {
  219  			logger.L().Error(ctx, "failed to parse clickhouse DSN, skipping endpoint", zap.Error(err))
  220  
  221  			continue
  222  		}
  223  
  224  		conn, err := clickhouse.NewDriver(dsn)
  225  		if err != nil {
  226  			logger.L().Error(ctx, "failed to create clickhouse driver, skipping endpoint",
  227  				zap.String("endpoint", label),
  228  				zap.Error(err),
  229  			)
  230  
  231  			continue
  232  		}
  233  
  234  		endpoints = append(endpoints, ClickhouseEndpoint{Conn: conn, Label: label})
  235  		closers = append(closers, closer{"clickhouse connection " + label, func(context.Context) error {
  236  			return conn.Close()
  237  		}})
  238  	}
  239  
  240  	return endpoints, closers
  241  }
````

`packages/orchestrator/pkg/factories/run.go` L665-L682:

````go
  665  	// clickhouse delivery endpoints
  666  	clickhouseEndpoints, clickhouseClosers := openClickhouseEndpoints(ctx, config)
  667  	closers = append(closers, clickhouseClosers...)
  668  
  669  	sbxEventsDeliveryTargets := make([]event.Delivery[event.SandboxEvent], 0, len(clickhouseEndpoints))
  670  	hostStatsTargets := make([]clickhousehoststats.Delivery, 0, len(clickhouseEndpoints))
  671  
  672  	for _, endpoint := range clickhouseEndpoints {
  673  		if delivery := newClickhouseEventsDelivery(ctx, endpoint, featureFlags); delivery != nil {
  674  			sbxEventsDeliveryTargets = append(sbxEventsDeliveryTargets, delivery)
  675  		}
  676  
  677  		if delivery := newClickhouseHostStatsDelivery(ctx, endpoint, featureFlags); delivery != nil {
  678  			hostStatsTargets = append(hostStatsTargets, delivery)
  679  		}
  680  	}
  681  
  682  	hostStatsDelivery := clickhousehoststats.NewMultiDelivery(hostStatsTargets...)
````

`packages/orchestrator/pkg/factories/run.go` L696-L722:

````go
  696  	// Redis sandbox events delivery target
  697  	if redisClient != nil {
  698  		sbxEventsDeliveryRedis := event.NewRedisStreamsDelivery[event.SandboxEvent](redisClient, event.SandboxEventsStreamName)
  699  		sbxEventsDeliveryTargets = append(sbxEventsDeliveryTargets, sbxEventsDeliveryRedis)
  700  	}
  701  
  702  	// Orchestrator-owned sandbox routing record (sandbox:routing:{id}).
  703  	if redisClient != nil {
  704  		routingPublisher, err := routing.New(
  705  			tel.MeterProvider,
  706  			sandboxcatalog.NewRedisSandboxRoutingCatalog(redisClient),
  707  			serviceInstanceID,
  708  			config.NodeIP,
  709  		)
  710  		if err != nil {
  711  			logger.L().Fatal(ctx, "failed to create sandbox routing publisher", zap.Error(err))
  712  		}
  713  		sandboxes.Subscribe(routingPublisher)
  714  	} else {
  715  		logger.L().Warn(ctx, "redis disabled; orchestrator sandbox routing records are not published")
  716  	}
  717  
  718  	// Wrapper closers run before per-driver closers (deliveries write through the drivers).
  719  	eventsService := events.NewEventsService(sbxEventsDeliveryTargets)
  720  	closers = append(closers, closer{"sandbox host stats deliveries (all)", hostStatsDelivery.Close})
  721  	closers = append(closers, closer{"sandbox events deliveries (all)", eventsService.Close})
  722  
````

`packages/clickhouse/pkg/hoststats/hoststats.go` L48-L83:

````go
   48  // noopDelivery is a Delivery that discards all stats.
   49  // Used in environments where host stats collection is not needed (CLI tools, tests).
   50  type noopDelivery struct{}
   51  
   52  var _ Delivery = (*noopDelivery)(nil)
   53  
   54  // NewNoopDelivery returns a Delivery that silently discards all stats.
   55  func NewNoopDelivery() Delivery {
   56  	return &noopDelivery{}
   57  }
   58  
   59  func (d *noopDelivery) Push(_ SandboxHostStat) error  { return nil }
   60  func (d *noopDelivery) Close(_ context.Context) error { return nil }
   61  
   62  // multiDelivery fans out to every target. Push is serial (each target's Push
   63  // is a non-blocking batcher send); Close is parallel so a stalled target
   64  // can't block the others from draining.
   65  type multiDelivery struct {
   66  	targets []Delivery
   67  }
   68  
   69  var _ Delivery = (*multiDelivery)(nil)
   70  
   71  // NewMultiDelivery returns noop for 0 targets and the target directly for 1,
   72  // so callers can wrap unconditionally.
   73  func NewMultiDelivery(targets ...Delivery) Delivery {
   74  	switch len(targets) {
   75  	case 0:
   76  		return NewNoopDelivery()
   77  	case 1:
   78  		return targets[0]
   79  	default:
   80  		return &multiDelivery{targets: targets}
   81  	}
   82  }
   83  
````

`packages/orchestrator/pkg/events/events.go` L28-L49:

````go
   28  func NewEventsService(deliveryTargets []events.Delivery[events.SandboxEvent]) *EventsService {
   29  	return &EventsService{
   30  		deliveryTargets: deliveryTargets,
   31  	}
   32  }
   33  
   34  func (e *EventsService) Publish(ctx context.Context, teamID uuid.UUID, event events.SandboxEvent) {
   35  	deliveryKey := events.DeliveryKey(teamID)
   36  
   37  	err := validateEvent(event)
   38  	if err != nil {
   39  		logger.L().Error(ctx, "Failed to publish sandbox event due to validation error", zap.Error(err), zap.Any("event", event))
   40  
   41  		return
   42  	}
   43  
   44  	for _, target := range e.deliveryTargets {
   45  		if err := target.Publish(ctx, deliveryKey, event); err != nil {
   46  			logger.L().Error(ctx, "Failed to publish sandbox event", zap.Error(err), zap.Any("event", event))
   47  		}
   48  	}
   49  }
````

With `CLICKHOUSE_CONNECTION_STRING=""` and `CLICKHOUSE_CONNECTION_STRINGS` unset: `openClickhouseEndpoints`
returns no endpoints (L186 guard; the additional list is empty), so no driver is created; host stats go to
`NewMultiDelivery()` -> noop; sandbox events go only to Redis if configured, else to an empty target list.
ClickHouse is fully skipped. Redis is likewise optional (`ErrRedisDisabled` tolerated, run.go L639-L645;
without Redis: no routing records, no peer registry, no event stream).

---

# C. Snapshot lineage, storage layout, deletion, header format

## C1. SchedulingMetadata

Message (orchestrator.proto L193-L212, quoted in P3). Computed from the resolved memfile/rootfs headers:

`packages/orchestrator/pkg/scheduling/metadata.go` (full file, 110 lines):

````go
    1  package scheduling
    2  
    3  import (
    4  	"cmp"
    5  	"slices"
    6  
    7  	"github.com/google/uuid"
    8  
    9  	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
   10  	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
   11  )
   12  
   13  // chainLimit caps how many build IDs are reported per artifact.
   14  const chainLimit = 128
   15  
   16  // FromHeaders reports, per artifact, the deduplicated build IDs whose data the
   17  // header references (with their referenced bytes), plus the base (root) and the
   18  // final/current build. buildID is the final layer; the snapshot path passes the
   19  // new build there because its memfile header may not be resolved yet, so it
   20  // derives the rest from the resolved parent headers and passes newMemfileBytes
   21  // (a pre-dedup upper bound) for the new layer. Lists are sorted by build ID —
   22  // order is not significant for affinity matching.
   23  //
   24  // A nil memfileHeader (filesystem-only snapshot) yields rootfs-only metadata
   25  // with empty memfile fields; the rootfs header is always required.
   26  func FromHeaders(buildID uuid.UUID, memfileHeader, rootfsHeader *header.Header, newMemfileBytes uint64) *orchestrator.SchedulingMetadata {
   27  	if rootfsHeader == nil || rootfsHeader.Metadata == nil {
   28  		return nil
   29  	}
   30  
   31  	rootfsBase := rootfsHeader.Metadata.BaseBuildId
   32  	rootIDs, rootBytes, rootDropped := artifactBuilds(rootfsHeader, rootfsBase, buildID, 0)
   33  
   34  	md := &orchestrator.SchedulingMetadata{
   35  		RootfsBaseBuildId:   rootfsBase.String(),
   36  		BuildId:             buildID.String(),
   37  		RootfsBuildIds:      rootIDs,
   38  		RootfsBuildBytes:    rootBytes,
   39  		RootfsDroppedBuilds: uint32(rootDropped),
   40  	}
   41  
   42  	if memfileHeader != nil && memfileHeader.Metadata != nil {
   43  		memfileBase := memfileHeader.Metadata.BaseBuildId
   44  		memIDs, memBytes, memDropped := artifactBuilds(memfileHeader, memfileBase, buildID, newMemfileBytes)
   45  		md.MemfileBaseBuildId = memfileBase.String()
   46  		md.MemfileBuildIds = memIDs
   47  		md.MemfileBuildBytes = memBytes
   48  		md.MemfileDroppedBuilds = uint32(memDropped)
   49  	}
   50  
   51  	return md
   52  }
   53  
   54  // artifactBuilds returns the build IDs referenced by the header and their
   55  // referenced bytes (aligned, sorted by ID), plus how many were dropped. The
   56  // outlined base and build are always kept; build is added with injectBuildBytes
   57  // when it is not already in the header (the not-yet-resolved new layer). Without
   58  // an ordered chain there is no natural tail to trim, so over chainLimit the
   59  // lightest layers are dropped.
   60  func artifactBuilds(h *header.Header, base, build uuid.UUID, injectBuildBytes uint64) ([]string, []uint64, int) {
   61  	bytesByID := h.Mapping.BytesByBuild()
   62  	if base != uuid.Nil {
   63  		if _, ok := bytesByID[base]; !ok {
   64  			bytesByID[base] = 0
   65  		}
   66  	}
   67  	if build != uuid.Nil {
   68  		if _, ok := bytesByID[build]; !ok {
   69  			bytesByID[build] = injectBuildBytes
   70  		}
   71  	}
   72  
   73  	ids := make([]uuid.UUID, 0, len(bytesByID))
   74  	for id := range bytesByID {
   75  		ids = append(ids, id)
   76  	}
   77  
   78  	dropped := 0
   79  	if len(ids) > chainLimit {
   80  		slices.SortFunc(ids, func(a, b uuid.UUID) int {
   81  			if ka, kb := pinned(a, base, build), pinned(b, base, build); ka != kb {
   82  				if ka {
   83  					return -1
   84  				}
   85  
   86  				return 1
   87  			}
   88  			if c := cmp.Compare(bytesByID[b], bytesByID[a]); c != 0 {
   89  				return c
   90  			}
   91  
   92  			return cmp.Compare(a.String(), b.String())
   93  		})
   94  		dropped = len(ids) - chainLimit
   95  		ids = ids[:chainLimit]
   96  	}
   97  
   98  	slices.SortFunc(ids, func(a, b uuid.UUID) int { return cmp.Compare(a.String(), b.String()) })
   99  
  100  	outIDs := make([]string, len(ids))
  101  	outBytes := make([]uint64, len(ids))
  102  	for i, id := range ids {
  103  		outIDs[i] = id.String()
  104  		outBytes[i] = bytesByID[id]
  105  	}
  106  
  107  	return outIDs, outBytes, dropped
  108  }
  109  
  110  func pinned(id, base, build uuid.UUID) bool { return id == base || id == build }
````

`packages/orchestrator/pkg/sandbox/template/storage_template.go` L383-L424:

````go
  383  // SchedulingMetadata reads the headers from the resolved memfile/rootfs devices
  384  // rather than the header holders, which stay unset for templates loaded from
  385  // storage (the headers are resolved internally by NewStorage during Fetch).
  386  func (t *storageTemplate) SchedulingMetadata(ctx context.Context) *orchestrator.SchedulingMetadata {
  387  	// The rootfs is always present; its header carries the build ID and is the
  388  	// minimum needed for scheduling metadata.
  389  	rootfs, rootfsErr := t.rootfs.WaitWithContext(ctx)
  390  	if rootfsErr != nil {
  391  		return nil
  392  	}
  393  
  394  	rh := rootfs.Header()
  395  	if rh == nil || rh.Metadata == nil {
  396  		return nil
  397  	}
  398  
  399  	// Filesystem-only snapshots have no memfile object, so memfile.WaitWithContext
  400  	// errors on reload. Tolerate that and report rootfs-only scheduling metadata
  401  	// (FromHeaders treats a nil memfile header as rootfs-only) instead of
  402  	// dropping the rootfs affinity data too.
  403  	var mh *header.Header
  404  	if memfile, memfileErr := t.memfile.WaitWithContext(ctx); memfileErr == nil {
  405  		// Use the durable (deduped) header, not the live one: during the
  406  		// provisional window memfile.Header() maps dirty pages to a synthetic build
  407  		// id that is never uploaded or registered, which would put a nonexistent
  408  		// layer into scheduling metadata. Non-blocking: this runs on the
  409  		// create/resume response path, so while a provisional swap is still pending
  410  		// (deduped header not yet known) report rootfs-only metadata rather than
  411  		// block the response for the whole dedup. No swap pending → live header.
  412  		if dh, ok := memfile.(interface {
  413  			DurableHeaderNow() (*header.Header, bool)
  414  		}); ok {
  415  			if h, ready := dh.DurableHeaderNow(); ready {
  416  				mh = h
  417  			}
  418  		} else {
  419  			mh = memfile.Header()
  420  		}
  421  	}
  422  
  423  	return scheduling.FromHeaders(rh.Metadata.BuildId, mh, rh, 0)
  424  }
````

- `*_build_ids`: every build ID that the artifact's header mapping references (all ancestor layers that
  still contribute blocks, plus base and the build itself), deduplicated and sorted by string. Byte counts
  are aligned.
- Capped at `chainLimit = 128`: the lightest layers are dropped first and counted in
  `*_dropped_builds`. **A GC must therefore not rely on these lists; read the headers (C3).**
- Filesystem-only snapshots: memfile fields empty. During a pending memfile dedup the response may carry
  rootfs-only metadata (storage_template.go L405-L417).
- Returned from Create (resume), Pause and Checkpoint responses and in `TemplateBuildMetadata`.

## C2. Storage layout per build

With `TEMPLATE_STORAGE_URL=file:///var/lib/e2b/storage/templates` the object key is
`<buildID>/<file>` under that directory (fs backend: `filepath.Join(basePath, path)`):

`packages/shared/pkg/storage/paths.go` L8-L86:

````go
    8  const (
    9  	GuestEnvdPath = "/usr/bin/envd"
   10  
   11  	MemfileName  = "memfile"
   12  	RootfsName   = "rootfs.ext4"
   13  	SnapfileName = "snapfile"
   14  	MetadataName = "metadata.json"
   15  
   16  	HeaderSuffix = ".header"
   17  )
   18  
   19  type Paths struct {
   20  	BuildID string `json:"build_id"`
   21  }
   22  
   23  // Key for the cache. Unique for template-build pair.
   24  func (p Paths) CacheKey() string {
   25  	return p.BuildID
   26  }
   27  
   28  func (p Paths) StorageDir() string {
   29  	return p.BuildID
   30  }
   31  
   32  func (p Paths) Memfile() string {
   33  	return fmt.Sprintf("%s/%s", p.BuildID, MemfileName)
   34  }
   35  
   36  func (p Paths) MemfileHeader() string {
   37  	return p.HeaderFile(MemfileName)
   38  }
   39  
   40  func (p Paths) Rootfs() string {
   41  	return fmt.Sprintf("%s/%s", p.BuildID, RootfsName)
   42  }
   43  
   44  func (p Paths) RootfsHeader() string {
   45  	return p.HeaderFile(RootfsName)
   46  }
   47  
   48  func (p Paths) Snapfile() string {
   49  	return fmt.Sprintf("%s/%s", p.BuildID, SnapfileName)
   50  }
   51  
   52  func (p Paths) Metadata() string {
   53  	return fmt.Sprintf("%s/%s", p.BuildID, MetadataName)
   54  }
   55  
   56  func (p Paths) MemfileCompressed(ct CompressionType) string {
   57  	return fmt.Sprintf("%s/%s%s", p.BuildID, MemfileName, ct.Suffix())
   58  }
   59  
   60  func (p Paths) RootfsCompressed(ct CompressionType) string {
   61  	return fmt.Sprintf("%s/%s%s", p.BuildID, RootfsName, ct.Suffix())
   62  }
   63  
   64  // DataFile returns the storage path for a data file (e.g. "memfile", "rootfs.ext4"),
   65  // with compression suffix appended if ct is not CompressionNone.
   66  func (p Paths) DataFile(name string, ct CompressionType) string {
   67  	if ct == CompressionNone {
   68  		return fmt.Sprintf("%s/%s", p.BuildID, name)
   69  	}
   70  
   71  	return fmt.Sprintf("%s/%s%s", p.BuildID, name, ct.Suffix())
   72  }
   73  
   74  // HeaderFile returns the storage path for a header sidecar of a data file
   75  // (e.g. "memfile" → "{buildID}/memfile.header").
   76  func (p Paths) HeaderFile(name string) string {
   77  	return fmt.Sprintf("%s/%s%s", p.BuildID, name, HeaderSuffix)
   78  }
   79  
   80  // SplitPath splits a storage path of the form "{buildID}/{fileName}"
   81  // back into its components. This is the inverse of the path methods.
   82  func SplitPath(path string) (buildID, fileName string) {
   83  	buildID, fileName, _ = strings.Cut(path, "/")
   84  
   85  	return buildID, fileName
   86  }
````

`packages/shared/pkg/storage/compress_frame_table.go` L373-L382:

````go
  373  func (ct CompressionType) Suffix() string {
  374  	switch ct {
  375  	case CompressionZstd:
  376  		return ".zstd"
  377  	case CompressionLZ4:
  378  		return ".lz4"
  379  	default:
  380  		return ""
  381  	}
  382  }
````

`packages/shared/pkg/storage/storageopts/storageopts.go` L21-L21:

````go
   21  	ObjectMetadataUncompressedSize = "uncompressed-size"
````

`packages/shared/pkg/storage/storage_fs.go` L43-L56:

````go
   43  func newFileSystemStorage(basePath, uploadBaseURL string, hmacKey []byte) *fsStorage {
   44  	return &fsStorage{
   45  		basePath:  basePath,
   46  		uploadURL: uploadBaseURL,
   47  		hmacKey:   hmacKey,
   48  	}
   49  }
   50  
   51  func (s *fsStorage) DeleteObjectsWithPrefix(_ context.Context, prefix string) error {
   52  	filePath := s.getPath(prefix)
   53  
   54  	return os.RemoveAll(filePath)
   55  }
   56  
````

`packages/shared/pkg/storage/storage_fs.go` L100-L102:

````go
  100  func (s *fsStorage) getPath(path string) string {
  101  	return filepath.Join(s.basePath, path)
  102  }
````

Per build directory `<TEMPLATE_STORAGE>/<buildID>/`:

| File | Present when |
|---|---|
| `metadata.json` | always (template metadata JSON, `metadata.Template`) |
| `rootfs.ext4` (or `rootfs.ext4.lz4` / `.zstd`) | rootfs diff data for this layer |
| `rootfs.ext4.header` | always (mapping over all ancestor rootfs layers) |
| `memfile` (or `memfile.lz4` / `.zstd`) | memory snapshots only (not fs-only) |
| `memfile.header` | memory snapshots only |
| `snapfile` | memory snapshots only (Firecracker VM state) |
| `<data>.<codec>.uncompressed-size` | fs backend sidecar for compressed objects |
| `.<name>.tmp-*` | transient during atomic writes (`replaceFile`) |

`metadata.json` fields relevant to lineage:

`packages/orchestrator/pkg/template/metadata/template_metadata.go` L104-L119:

````go
  104  type FromTemplate struct {
  105  	Alias   string `json:"alias"`
  106  	BuildID string `json:"build_id"`
  107  }
  108  
  109  type Start struct {
  110  	StartCmd string  `json:"start_command"`
  111  	ReadyCmd string  `json:"ready_command"`
  112  	Context  Context `json:"context"`
  113  }
  114  
  115  type TemplateMetadata struct {
  116  	BuildID            string `json:"build_id"`
  117  	KernelVersion      string `json:"kernel_version"`
  118  	FirecrackerVersion string `json:"firecracker_version"`
  119  }
````

`packages/orchestrator/pkg/template/metadata/template_metadata.go` L156-L164:

````go
  156  type Template struct {
  157  	Version      uint64           `json:"version"`
  158  	Template     TemplateMetadata `json:"template"`
  159  	Context      Context          `json:"context"`
  160  	Start        *Start           `json:"start,omitempty"`
  161  	FromImage    *string          `json:"from_image,omitempty"`
  162  	FromTemplate *FromTemplate    `json:"from_template,omitempty"`
  163  	Prefetch     *Prefetch        `json:"prefetch,omitempty"`
  164  
````

Local caches keyed by build ID (not storage, but hold references while in use):
`TEMPLATE_CACHE_DIR/<buildID>/cache/<uuid>/{snapfile,metadata.json}` (paths_cache.go L42-L56),
`DEFAULT_CACHE_DIR` chunk cache (wiped at startup), and the in-memory template cache (25 h TTL, pinned while a
sandbox uses the template):

`packages/orchestrator/pkg/sandbox/template/cache.go` L34-L46:

````go
   34  // Should be longer than the maximum possible sandbox lifetime.
   35  const (
   36  	templateExpiration       = time.Hour * 25
   37  	templateExpirationBuffer = time.Hour
   38  
   39  	buildCacheTTL           = time.Hour * 25
   40  	buildCacheDelayEviction = time.Second * 60
   41  
   42  	// How long a template lingers after its last pin is released, when it had
   43  	// already expired out of the cache while pinned. Short on purpose — see
   44  	// (*Cache).release.
   45  	unpinnedGraceTTL = time.Minute
   46  )
````

## C3. Header format (what a build references)

`packages/shared/pkg/storage/header/header.go` L27-L45:

````go
   27  type Header struct {
   28  	Metadata *Metadata
   29  	// Builds maps build IDs to per-build metadata (size, checksum, FrameTable).
   30  	// nil for V3 (uncompressed) headers; the read path falls back to a Size()
   31  	// RPC and reads uncompressed data when nil.
   32  	Builds map[uuid.UUID]BuildData
   33  
   34  	// Mapping is the per-block source map. Stored compactly (9-10 B/entry vs 40
   35  	// for a BuildMap) so long-lived cached headers don't dominate orchestrator
   36  	// heap. Read via At / All / Slice / Len, not indexing.
   37  	Mapping Mapping
   38  
   39  	// IncompletePendingUpload is set on diff headers produced by ToDiffHeader and
   40  	// cleared on the finalized headers swapped in by the upload pipeline. It
   41  	// is in-memory only (never serialized), and signals that the build's data
   42  	// has not yet reached object storage — readers must serve from the local
   43  	// cache and skip FrameTable lookups for the still-missing self entry.
   44  	IncompletePendingUpload bool
   45  }
````

`packages/shared/pkg/storage/header/metadata.go` L20-L76:

````go
   20  const (
   21  	// metadataVersion is used by template-manager for uncompressed builds (V3 headers).
   22  	metadataVersion = 3
   23  	// MetadataVersionV4 is used for compressed builds (V4 headers with FrameTables).
   24  	MetadataVersionV4 = 4
   25  	// MetadataVersionV5 is V4 with a columnar, varint-encoded mapping section.
   26  	// Same semantics as V4 (Builds map + FrameTables); only the on-disk mapping
   27  	// layout differs. Far smaller and far more compressible for the large,
   28  	// fragmented headers produced by page-granular memfile dedup.
   29  	MetadataVersionV5 = 5
   30  )
   31  
   32  type Metadata struct {
   33  	Version    uint64
   34  	BlockSize  uint64
   35  	Size       uint64
   36  	Generation uint64
   37  	BuildId    uuid.UUID
   38  	// TODO: Use the base build id when setting up the snapshot rootfs
   39  	BaseBuildId uuid.UUID
   40  }
   41  
   42  func NewTemplateMetadata(buildId uuid.UUID, blockSize, size uint64) *Metadata {
   43  	return &Metadata{
   44  		Version:     metadataVersion,
   45  		Generation:  0,
   46  		BlockSize:   blockSize,
   47  		Size:        size,
   48  		BuildId:     buildId,
   49  		BaseBuildId: buildId,
   50  	}
   51  }
   52  
   53  func (m *Metadata) NextGeneration(buildID uuid.UUID) *Metadata {
   54  	return &Metadata{
   55  		Version:     m.Version,
   56  		Generation:  m.Generation + 1,
   57  		BlockSize:   m.BlockSize,
   58  		Size:        m.Size,
   59  		BuildId:     buildID,
   60  		BaseBuildId: m.BaseBuildId,
   61  	}
   62  }
   63  
   64  // metadataSize is the binary size of the Metadata struct, computed from the struct layout.
   65  var metadataSize = binary.Size(Metadata{})
   66  
   67  func deserializeMetadata(data []byte) (*Metadata, error) {
   68  	var metadata Metadata
   69  
   70  	err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &metadata)
   71  	if err != nil {
   72  		return nil, fmt.Errorf("failed to read metadata: %w", err)
   73  	}
   74  
   75  	return &metadata, nil
   76  }
````

`packages/shared/pkg/storage/header/mapping.go` L12-L20:

````go
   12  // BuildMap maps a byte range in the block device to a region in a build's storage.
   13  // Offset, Length, and BuildStorageOffset are in bytes.
   14  type BuildMap struct {
   15  	// Offset is the starting position of this range in the block device.
   16  	Offset             uint64
   17  	Length             uint64
   18  	BuildId            uuid.UUID
   19  	BuildStorageOffset uint64
   20  }
````

`packages/shared/pkg/storage/header/serialization.go` L14-L65:

````go
   14  const metadataVersionMask = 0xFFFF
   15  
   16  func metadataFormatVersion(version uint64) uint64 {
   17  	return version & metadataVersionMask
   18  }
   19  
   20  // SerializeHeader serializes a header, dispatching to the version-specific format.
   21  //
   22  // V3 (Version <= 3): [Metadata] [v3 mappings…]
   23  // V4 (Version >= 4): [Metadata] [uint8 flags] [uint32 uncompressedSize] [LZ4( Builds + v4 mappings )]
   24  func SerializeHeader(h *Header) ([]byte, error) {
   25  	switch metadataFormatVersion(h.Metadata.Version) {
   26  	case 1, 2, 3:
   27  		return serializeV3(h.Metadata, h.Mapping)
   28  	case MetadataVersionV4:
   29  		data, _, err := serializeV4(h.Metadata, h.Builds, h.Mapping, h.IncompletePendingUpload)
   30  
   31  		return data, err
   32  	case MetadataVersionV5:
   33  		data, _, err := serializeV5(h.Metadata, h.Builds, h.Mapping, h.IncompletePendingUpload)
   34  
   35  		return data, err
   36  	default:
   37  		return nil, fmt.Errorf("unsupported header version %d", h.Metadata.Version)
   38  	}
   39  }
   40  
   41  // DeserializeBytes auto-detects the header version and deserializes accordingly.
   42  // See SerializeHeader for the binary layout.
   43  func DeserializeBytes(data []byte) (*Header, error) {
   44  	if len(data) < metadataSize {
   45  		return nil, fmt.Errorf("header too short: %d bytes", len(data))
   46  	}
   47  
   48  	metadata, err := deserializeMetadata(data[:metadataSize])
   49  	if err != nil {
   50  		return nil, err
   51  	}
   52  
   53  	blockData := data[metadataSize:]
   54  
   55  	switch metadataFormatVersion(metadata.Version) {
   56  	case MetadataVersionV5:
   57  		return deserializeV5(metadata, blockData)
   58  	case MetadataVersionV4:
   59  		return deserializeV4(metadata, blockData)
   60  	case 1, 2, 3:
   61  		return deserializeV3(metadata, blockData)
   62  	default:
   63  		return nil, fmt.Errorf("unsupported header version %d", metadata.Version)
   64  	}
   65  }
````

`packages/shared/pkg/storage/header/serialization.go` L91-L120:

````go
   91  // LoadHeader fetches a serialized header from storage and deserializes it.
   92  // Returns the on-wire byte count alongside the header so callers can attribute
   93  // it to throughput telemetry. Errors (including storage.ErrObjectNotExist) are
   94  // returned as-is.
   95  func LoadHeader(ctx context.Context, s storage.StorageProvider, path string) (*Header, int, error) {
   96  	h, n, err := LoadStoredHeader(ctx, s, path)
   97  	if err != nil {
   98  		return nil, n, err
   99  	}
  100  
  101  	if !h.IncompletePendingUpload {
  102  		backfillMissingV3UncompressedBuilds(h)
  103  	}
  104  
  105  	return h, n, nil
  106  }
  107  
  108  // LoadStoredHeader is LoadHeader without the V3 uncompressed-build backfill:
  109  // its Builds map holds exactly the entries the serialized header carried.
  110  func LoadStoredHeader(ctx context.Context, s storage.StorageProvider, path string) (*Header, int, error) {
  111  	blob, err := s.OpenBlob(ctx, path)
  112  	if err != nil {
  113  		return nil, 0, fmt.Errorf("open blob %s: %w", path, err)
  114  	}
  115  
  116  	// read.blob (the transfer) is emitted per-layer inside each backend WriteTo;
  117  	// only the deserialize/decompress phase below is single-layer.
  118  	data, err := storage.GetBlob(ctx, blob)
  119  	if err != nil {
  120  		return nil, 0, err
````

`packages/shared/pkg/storage/header/compact.go` L245-L250:

````go
  245  }
  246  
  247  // Builds returns the deduplicated build IDs referenced by the mapping. The
  248  // returned slice is shared with the Mapping; callers must not mutate it.
  249  func (m Mapping) Builds() []uuid.UUID { return m.builds }
  250  
````

`packages/shared/pkg/storage/header/compact.go` L301-L315:

````go
  301  func (m Mapping) BytesByBuild() map[uuid.UUID]uint64 {
  302  	sums := make([]uint64, len(m.builds))
  303  	for i := range m.offsets {
  304  		if bi := m.buildIndex(i); bi >= 0 {
  305  			sums[bi] += uint64(m.lengthBlocks(i))
  306  		}
  307  	}
  308  
  309  	out := make(map[uuid.UUID]uint64, len(m.builds))
  310  	for bi, blocks := range sums {
  311  		out[m.builds[bi]] = blocks * m.blockSize
  312  	}
  313  
  314  	return out
  315  }
````

- Layout: little-endian `Metadata{Version, BlockSize, Size, Generation, BuildId[16], BaseBuildId[16]}`
  followed by mappings (V3 raw; V4/V5 `[flags u8][uncompressedSize u32][LZ4(builds + mappings)]`).
  Format version = `Version & 0xFFFF` (1-3, 4, 5).
- References: `h.Mapping.Builds()` = deduplicated build IDs whose storage the header maps blocks to
  (ancestors + self); `h.Builds` (V4+) has per-build size/checksum/frame table. `uuid.Nil` denotes
  sparse/zero ranges (`ignoreBuildID`, metadata.go L78) and is not a build. `BaseBuildId` is the root layer.
- A GC can compute references by, for every retained build B, reading `B/rootfs.ext4.header` and (if
  present) `B/memfile.header` with `header.DeserializeBytes`, collecting `Mapping.Builds()` (minus Nil)
  and `Metadata.BaseBuildId`; the live set is the union over retained builds plus builds used by running
  sandboxes and in-progress uploads. A build is deletable only if it is in no header of any retained build.
  Importing `github.com/e2b-dev/infra/packages/shared/pkg/storage/header` from a Go GC gives exact parsing.

## C4. Deleting a build

The only deletion RPC is `TemplateService.TemplateBuildDelete` (template-manager service; requires
`template-manager` in `ORCHESTRATOR_SERVICES`). There is no SandboxService RPC that deletes a snapshot
build.

`packages/orchestrator/template-manager.proto` L128-L132:

````proto
  128  // Data required for deleting a template.
  129  message TemplateBuildDeleteRequest {
  130    string buildID = 1;
  131    string templateID = 2;
  132  }
````

`packages/orchestrator/template-manager.proto` L176-L189:

````proto
  176  // Interface exported by the server.
  177  service TemplateService {
  178    // TemplateCreate is a gRPC service that creates a new template
  179    rpc TemplateCreate (TemplateCreateRequest) returns (google.protobuf.Empty);
  180  
  181    // TemplateStatus is a gRPC service that streams the status of a template build
  182    rpc TemplateBuildStatus (TemplateStatusRequest) returns (TemplateBuildStatusResponse);
  183  
  184    // TemplateBuildDelete is a gRPC service that deletes files associated with a template build
  185    rpc TemplateBuildDelete (TemplateBuildDeleteRequest) returns (google.protobuf.Empty);
  186  
  187    // InitLayerFileUpload requests an upload URL for a tar file containing layer files to be cached for the template build.
  188    rpc InitLayerFileUpload (InitLayerFileUploadRequest) returns (InitLayerFileUploadResponse);
  189  }
````

`packages/orchestrator/pkg/template/server/delete_template.go` (full file, 54 lines):

````go
    1  //go:build linux
    2  
    3  package server
    4  
    5  import (
    6  	"context"
    7  	"errors"
    8  
    9  	"go.opentelemetry.io/otel"
   10  	"go.opentelemetry.io/otel/trace"
   11  	"google.golang.org/protobuf/types/known/emptypb"
   12  
   13  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/builderrors"
   14  	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/template"
   15  	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
   16  	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
   17  	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
   18  )
   19  
   20  var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/template/server")
   21  
   22  func (s *ServerStore) TemplateBuildDelete(ctx context.Context, in *templatemanager.TemplateBuildDeleteRequest) (*emptypb.Empty, error) {
   23  	s.wg.Add(1)
   24  	defer s.wg.Done()
   25  	done := s.info.TrackWork()
   26  	defer done()
   27  
   28  	ctx, childSpan := tracer.Start(ctx, "template-delete-request", trace.WithAttributes(
   29  		telemetry.WithTemplateID(in.GetTemplateID()),
   30  		telemetry.WithBuildID(in.GetBuildID()),
   31  	))
   32  	defer childSpan.End()
   33  
   34  	if in.GetTemplateID() == "" || in.GetBuildID() == "" {
   35  		return nil, errors.New("template id and build id are required fields")
   36  	}
   37  
   38  	buildInfo, err := s.buildCache.Get(in.GetBuildID())
   39  	if err == nil && buildInfo.IsRunning() {
   40  		// Cancel the build if it is running
   41  		logger.L().Info(ctx, "Canceling running template build", logger.WithTemplateID(in.GetTemplateID()), logger.WithBuildID(in.GetBuildID()))
   42  		telemetry.ReportEvent(ctx, "cancel in progress template build")
   43  		buildInfo.SetFail(&templatemanager.TemplateBuildStatusReason{
   44  			Message: builderrors.ErrCanceled.Error(),
   45  		})
   46  	}
   47  
   48  	err = template.Delete(ctx, s.artifactsregistry, s.templateStorage, in.GetTemplateID(), in.GetBuildID())
   49  	if err != nil {
   50  		return nil, err
   51  	}
   52  
   53  	return nil, nil
   54  }
````

`packages/orchestrator/pkg/template/template/main.go` (full file, 39 lines):

````go
    1  package template
    2  
    3  import (
    4  	"context"
    5  	"errors"
    6  	"fmt"
    7  
    8  	"go.opentelemetry.io/otel"
    9  
   10  	artifactsregistry "github.com/e2b-dev/infra/packages/shared/pkg/artifacts-registry"
   11  	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
   12  	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
   13  )
   14  
   15  var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/template/template")
   16  
   17  func Delete(ctx context.Context, artifactRegistry artifactsregistry.ArtifactsRegistry, templateStorage storage.StorageProvider, templateId string, buildId string) error {
   18  	childCtx, childSpan := tracer.Start(ctx, "delete-template")
   19  	defer childSpan.End()
   20  
   21  	err := templateStorage.DeleteObjectsWithPrefix(ctx, buildId)
   22  	if err != nil {
   23  		return fmt.Errorf("error when deleting template objects: %w", err)
   24  	}
   25  
   26  	err = artifactRegistry.Delete(childCtx, templateId, buildId)
   27  	if err != nil {
   28  		// snapshot build are not stored in docker repository
   29  		if errors.Is(err, artifactsregistry.ErrImageNotExists) {
   30  			return nil
   31  		}
   32  
   33  		telemetry.ReportEvent(childCtx, err.Error())
   34  
   35  		return err
   36  	}
   37  
   38  	return nil
   39  }
````

`packages/shared/pkg/artifacts-registry/registry_local.go` (full file, 14 lines):

````go
    1  package artifacts_registry
    2  
    3  import "context"
    4  
    5  type LocalArtifactsRegistry struct{}
    6  
    7  func NewLocalArtifactsRegistry() (*LocalArtifactsRegistry, error) {
    8  	return &LocalArtifactsRegistry{}, nil
    9  }
   10  
   11  func (g *LocalArtifactsRegistry) Delete(context.Context, string, string) error {
   12  	// for now, just assume local image can be deleted manually
   13  	return nil
   14  }
````

`TemplateBuildDelete` = cancel a running build with that ID (if any) + `DeleteObjectsWithPrefix(buildID)`
(fs backend: `os.RemoveAll(<base>/<buildID>)`) + artifacts-registry delete (Local: no-op). It does **not**
check lineage references, does not evict the in-memory template cache or `TEMPLATE_CACHE_DIR/<buildID>`,
and ignores whether a running sandbox maps that build. Deleting a build that a running sandbox or a retained
descendant references breaks lazy block reads. Safe deletion = GC reference check (C3) + no running sandbox
whose lineage includes the build (List + each sandbox's SchedulingMetadata / headers) + no in-flight pause
upload. Removing `<TEMPLATE_STORAGE>/<buildID>/` directly is equivalent to the RPC for the fs backend.

---

# D. Networking

## D1. CIDRs and slot addressing

`packages/orchestrator/pkg/sandbox/network/slot.go` L96-L157:

````go
   96  
   97  func NewSlot(key string, idx int, config Config, egressProxy EgressProxy) (*Slot, error) {
   98  	if idx < 1 || idx > vrtSlotsSize {
   99  		return nil, fmt.Errorf("slot index %d is out of range [1, %d]", idx, vrtSlotsSize)
  100  	}
  101  
  102  	vEthIp, err := netutils.GetIndexedIP(vrtNetworkCIDR, idx*vrtAddressPerSlot)
  103  	if err != nil {
  104  		return nil, fmt.Errorf("failed to get veth indexed IP: %w", err)
  105  	}
  106  
  107  	vPeerIp, err := netutils.GetIndexedIP(vrtNetworkCIDR, idx*vrtAddressPerSlot+1)
  108  	if err != nil {
  109  		return nil, fmt.Errorf("failed to get vpeer indexed IP: %w", err)
  110  	}
  111  
  112  	vrtCIDR := fmt.Sprintf("%s/%d", vPeerIp.String(), vrtMask)
  113  	_, vrtNet, err := net.ParseCIDR(vrtCIDR)
  114  	if err != nil {
  115  		return nil, fmt.Errorf("failed to parse vrt CIDR: %w", err)
  116  	}
  117  
  118  	hostIp, err := netutils.GetIndexedIP(hostNetworkCIDR, idx)
  119  	if err != nil {
  120  		return nil, fmt.Errorf("failed to get host IP: %w", err)
  121  	}
  122  
  123  	hostCIDR := fmt.Sprintf("%s/%d", hostIp.String(), hostMask)
  124  	_, hostNet, err := net.ParseCIDR(hostCIDR)
  125  	if err != nil {
  126  		return nil, fmt.Errorf("failed to parse host CIDR: %w", err)
  127  	}
  128  
  129  	tapCIDR := fmt.Sprintf("%s/%d", tapIp, tapMask)
  130  	tapIp, tapNet, err := net.ParseCIDR(tapCIDR)
  131  	if err != nil {
  132  		return nil, fmt.Errorf("failed to parse tap CIDR: %w", err)
  133  	}
  134  
  135  	slot := &Slot{
  136  		Key: key,
  137  		Idx: idx,
  138  
  139  		vPeerIp: vPeerIp,
  140  		vEthIp:  vEthIp,
  141  		vrtMask: vrtNet.Mask,
  142  
  143  		tapIp:   tapIp,
  144  		tapMask: tapNet.Mask,
  145  
  146  		HostIP:   hostIp,
  147  		hostNet:  hostNet,
  148  		hostCIDR: hostCIDR,
  149  
  150  		hyperloopPort: strconv.FormatUint(uint64(config.HyperloopProxyPort), 10),
  151  
  152  		config:      config,
  153  		egressProxy: egressProxy,
  154  	}
  155  
  156  	return slot, nil
  157  }
````

Defaults: host CIDR `10.11.0.0/16` (`SANDBOXES_HOST_NETWORK_CIDR`), vrt CIDR `10.12.0.0/16`
(`SANDBOXES_VRT_NETWORK_CIDR`); read once at package init with `env.GetEnv` (slot.go L43-L48, L492-L533);
not set by compose (defaults used). Slot count = `2^(32-prefix)/2 - 2` = 32766 for a /16. Per slot:
netns `ns-<idx>`, host veth `veth-<idx>` = `10.12.0.(2*idx)`/31, vpeer `eth0` in the netns =
`10.12.0.(2*idx+1)`/31, host IP `10.11.0.idx`/32, tap `tap0` = `169.254.0.22/30`, guest = `169.254.0.21`,
tap MAC `02:FC:00:00:00:06` host side (guest `02:FC:00:00:00:05`).

## D2. NETWORK_VERSION

`NETWORK_VERSION` (pool.go L95-L96, default 1, validated 1|2). Embed sets `"1"`. v1 = per-slot iptables in
host + nftables inside each netns; v2 = nftables host tables (`v2-host-firewall`) and requires
`net.ipv4.ip_forward=1`:

`packages/orchestrator/pkg/factories/run.go` L819-L885:

````go
  819  	// network pool
  820  	slotStorage, err := network.NewStorageLocal(ctx, config.NetworkConfig, egressSetup.Proxy)
  821  	if err != nil {
  822  		logger.L().Fatal(ctx, "failed to create network pool", zap.Error(err))
  823  	}
  824  	var networkPool network.PoolInterface
  825  	selectedNetworkVersion := 0
  826  	switch config.NetworkConfig.NetworkVersion {
  827  	case 2:
  828  		logger.L().Info(ctx, "using v2 network pool (nftables)")
  829  		v2Metrics, metricsErr := networkv2.NewMetrics(tel.MeterProvider)
  830  		if metricsErr != nil {
  831  			logger.L().Fatal(ctx, "failed to create v2 network metrics", zap.Error(metricsErr))
  832  		}
  833  
  834  		if err := networkv2.ValidateV2Prerequisites(); err != nil {
  835  			logger.L().Fatal(ctx, "v2 network prerequisites not met", zap.Error(err))
  836  		}
  837  
  838  		hostFw, hfErr := networkv2.NewHostFirewallWithMetrics(network.DefaultGateway(), config.NetworkConfig, v2Metrics)
  839  		if hfErr != nil {
  840  			logger.L().Fatal(ctx, "failed to create v2 host firewall", zap.Error(hfErr))
  841  		}
  842  
  843  		// Flush to empty only when reclaim tore every slot down; a partial
  844  		// failure leaves anchor namespaces whose set entries must survive.
  845  		if reclaimClean {
  846  			if err := hostFw.ReconcileSlots(ctx, nil); err != nil {
  847  				logger.L().Fatal(ctx, "failed to reconcile v2 host firewall slots", zap.Error(err))
  848  			}
  849  		} else if reclaimRan {
  850  			v2Metrics.RecordReconciliationSkipped(ctx)
  851  		}
  852  
  853  		observer, obsErr := networkv2.NewVethObserver()
  854  		if obsErr != nil {
  855  			logger.L().Fatal(ctx, "failed to create v2 veth observer", zap.Error(obsErr))
  856  		}
  857  
  858  		v2Pool := networkv2.NewV2Pool(slotStorage, config.NetworkConfig, hostFw, observer, networkv2.WithPoolMetrics(v2Metrics))
  859  		startService("network pool", func() error {
  860  			v2Pool.Populate(ctx)
  861  
  862  			return nil
  863  		})
  864  		networkPool = v2Pool
  865  		selectedNetworkVersion = 2
  866  	case 1:
  867  		if usesSandboxRuntime && !config.DisableStartupReclaim {
  868  			if err := networkv2.PurgeHostFirewallTable(); err != nil {
  869  				logger.L().Fatal(ctx, "refusing to start v1: stale v2 host firewall table could hijack v1 sandbox egress", zap.Error(err))
  870  			}
  871  		}
  872  		v1Pool := network.NewPool(network.NewSlotsPoolSize, network.ReusedSlotsPoolSize, slotStorage, config.NetworkConfig)
  873  		startService("network pool", func() error {
  874  			v1Pool.Populate(ctx)
  875  
  876  			return nil
  877  		})
  878  		networkPool = v1Pool
  879  		selectedNetworkVersion = 1
  880  	default:
  881  		// Config validation runs before startup side effects. Keep this exhaustive
  882  		// defense so future parsing and selection logic cannot silently diverge.
  883  		logger.L().Fatal(ctx, "unsupported NETWORK_VERSION after validated config", zap.Int("network_version", config.NetworkConfig.NetworkVersion))
  884  	}
  885  	closers = append(closers, closer{"network pool", networkPool.Close})
````

`packages/orchestrator/pkg/sandbox/network/v2/pool.go` L105-L135:

````go
  105  // ValidateV2Prerequisites checks that required kernel parameters are set for v2 networking.
  106  // Returns an error if any prerequisite is missing. Call before NewV2Pool.
  107  func ValidateV2Prerequisites() error {
  108  	checks := []struct {
  109  		path     string
  110  		expected string
  111  		desc     string
  112  	}{
  113  		{"/proc/sys/net/ipv4/ip_forward", "1", "IPv4 forwarding"},
  114  	}
  115  
  116  	var errs []error
  117  	for _, c := range checks {
  118  		val, err := os.ReadFile(c.path)
  119  		if err != nil {
  120  			errs = append(errs, fmt.Errorf("cannot read %s: %w", c.path, err))
  121  
  122  			continue
  123  		}
  124  		if strings.TrimSpace(string(val)) != c.expected {
  125  			errs = append(errs, fmt.Errorf("%s: %s must be %s, got %q", c.desc, c.path, c.expected, strings.TrimSpace(string(val))))
  126  		}
  127  	}
  128  
  129  	if _, err := network.IptablesHandle(); err != nil {
  130  		errs = append(errs, fmt.Errorf("iptables (needed for the per-slot FORWARD accepts): %w", err))
  131  	}
  132  
  133  	if len(errs) > 0 {
  134  		return fmt.Errorf("v2 networking prerequisites not met: %w", errors.Join(errs...))
  135  	}
````
v1 does not validate `ip_forward`, but guest traffic is routed through the host, so **set
`net.ipv4.ip_forward=1` on a bare Debian 13 host** (sysctl drop-in); Docker hosts already have it.

## D3. Default route requirement

`packages/orchestrator/pkg/sandbox/network/host.go` (full file, 60 lines):

````go
    1  //go:build linux
    2  
    3  package network
    4  
    5  import (
    6  	"context"
    7  	"errors"
    8  	"fmt"
    9  
   10  	"github.com/vishvananda/netlink"
   11  	"go.uber.org/zap"
   12  
   13  	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
   14  	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
   15  )
   16  
   17  // Host loopback interface name
   18  const loopbackInterface = "lo"
   19  
   20  // Host default gateway name
   21  var defaultGateway = utils.Must(getDefaultGateway(context.Background()))
   22  
   23  // DefaultGateway returns the detected host default gateway interface name.
   24  func DefaultGateway() string { return defaultGateway }
   25  
   26  //	func getDefaultGateway() (string, error) {
   27  //		route, err := exec.Command(
   28  //			"sh",
   29  //			"-c",
   30  //			"ip route show default | awk '{print $5}'",
   31  //		).Output()
   32  //		if err != nil {
   33  //			return "", fmt.Errorf("error fetching default gateway: %w", err)
   34  //		}
   35  //
   36  //		return string(route), nil
   37  //	}
   38  func getDefaultGateway(ctx context.Context) (string, error) {
   39  	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
   40  	if err != nil {
   41  		return "", fmt.Errorf("error fetching routes: %w", err)
   42  	}
   43  
   44  	for _, route := range routes {
   45  		// 0.0.0.0/0
   46  		if route.Dst.String() == "0.0.0.0/0" && route.Gw != nil {
   47  			logger.L().Info(ctx, "default gateway", zap.String("gateway", route.Gw.String()))
   48  
   49  			link, linkErr := netlink.LinkByIndex(route.LinkIndex)
   50  
   51  			if linkErr != nil {
   52  				return "", fmt.Errorf("error fetching interface for default gateway: %w", linkErr)
   53  			}
   54  
   55  			return link.Attrs().Name, nil
   56  		}
   57  	}
   58  
   59  	return "", errors.New("cannot find default gateway")
   60  }
````
Evaluated at package init with `utils.Must`: the process **panics at startup** unless there is an IPv4
route with `Dst == 0.0.0.0/0` **and a gateway** (`route.Gw != nil`). A default route without a gateway
(point-to-point / `dev wg0` only) fails. The interface found is the one used in all FORWARD/MASQUERADE
rules below (egress for non-TCP guest traffic; TCP is re-originated by the host proxy and follows host
routing). The string comparison relies on vishvananda/netlink (v1.3.1 per go.mod) returning a non-nil
`0.0.0.0/0` Dst for the default route, which it does for this version.

## D4. iptables/nftables installed per slot (v1) and host-wide

`packages/orchestrator/pkg/sandbox/network/network.go` (full file, 452 lines):

````go
    1  //go:build linux
    2  
    3  package network
    4  
    5  import (
    6  	"context"
    7  	"errors"
    8  	"fmt"
    9  	"net"
   10  	"os"
   11  	"runtime"
   12  	"strconv"
   13  
   14  	"github.com/coreos/go-iptables/iptables"
   15  	"github.com/vishvananda/netlink"
   16  	"github.com/vishvananda/netns"
   17  	"go.uber.org/zap"
   18  	"golang.org/x/sys/unix"
   19  
   20  	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
   21  )
   22  
   23  type notExistError interface {
   24  	IsNotExist() bool
   25  }
   26  
   27  type multiUnwrapError interface {
   28  	Unwrap() []error
   29  }
   30  
   31  func ignoreExpectedAbsent(err error, isExpected func(error) bool) bool {
   32  	if err == nil {
   33  		return true
   34  	}
   35  
   36  	var joined multiUnwrapError
   37  	if errors.As(err, &joined) {
   38  		for _, child := range joined.Unwrap() {
   39  			if !ignoreExpectedAbsent(child, isExpected) {
   40  				return false
   41  			}
   42  		}
   43  
   44  		return true
   45  	}
   46  
   47  	return isExpected(err)
   48  }
   49  
   50  func isIPTablesNotExist(err error) bool {
   51  	var notExist notExistError
   52  
   53  	return errors.As(err, &notExist) && notExist.IsNotExist()
   54  }
   55  
   56  func isRouteNotExist(err error) bool {
   57  	return errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT)
   58  }
   59  
   60  func isLinkNotExist(err error) bool {
   61  	var linkNotFound netlink.LinkNotFoundError
   62  
   63  	return errors.As(err, &linkNotFound) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT)
   64  }
   65  
   66  func isNamespaceNotExist(err error) bool {
   67  	return os.IsNotExist(err) || errors.Is(err, unix.ENOENT)
   68  }
   69  
   70  func appendUnlessExpectedAbsentf(errs *[]error, err error, isExpected func(error) bool, format string) {
   71  	if ignoreExpectedAbsent(err, isExpected) {
   72  		return
   73  	}
   74  
   75  	*errs = append(*errs, fmt.Errorf(format, err))
   76  }
   77  
   78  func (s *Slot) CreateNetwork(ctx context.Context) (retErr error) {
   79  	// Prevent thread changes so we can safely manipulate with namespaces
   80  	runtime.LockOSThread()
   81  	defer runtime.UnlockOSThread()
   82  
   83  	// Save the original (host) namespace and restore it upon function exit
   84  	hostNS, err := netns.Get()
   85  	if err != nil {
   86  		return fmt.Errorf("cannot get current (host) namespace: %w", err)
   87  	}
   88  
   89  	cleanupNeeded := false
   90  	defer func() {
   91  		restoreErr := netns.Set(hostNS)
   92  		if restoreErr != nil {
   93  			logger.L().Error(ctx, "error resetting network namespace back to the host namespace", zap.Error(restoreErr))
   94  		}
   95  
   96  		if retErr != nil && cleanupNeeded {
   97  			if restoreErr != nil {
   98  				retErr = errors.Join(retErr, fmt.Errorf("error resetting network namespace back to the host namespace before cleanup: %w", restoreErr))
   99  			} else if cleanupErr := s.RemoveNetwork(); cleanupErr != nil {
  100  				retErr = errors.Join(retErr, fmt.Errorf("error cleaning up partially created network: %w", cleanupErr))
  101  			}
  102  		}
  103  
  104  		err = hostNS.Close()
  105  		if err != nil {
  106  			logger.L().Error(ctx, "error closing host network namespace", zap.Error(err))
  107  		}
  108  	}()
  109  
  110  	// An existing namespace for this index is a stale reclaim anchor from a
  111  	// failed teardown whose iptables rules, routes and veth may still exist. Run
  112  	// a full (idempotent) RemoveNetwork to reclaim them; deleting only the
  113  	// namespace would orphan those rules. On failure the anchor is kept and we
  114  	// abort so the slot is retried later instead of leaking.
  115  	available, err := isNamespaceAvailable(NetNamespacesDir, s.NamespaceID())
  116  	if err != nil {
  117  		return fmt.Errorf("cannot check for stale namespace: %w", err)
  118  	}
  119  	if !available {
  120  		if err = s.RemoveNetwork(); err != nil {
  121  			return fmt.Errorf("cannot reclaim stale network slot: %w", err)
  122  		}
  123  	}
  124  
  125  	// Create NS for the sandbox
  126  	ns, err := netns.NewNamed(s.NamespaceID())
  127  	if err != nil {
  128  		return fmt.Errorf("cannot create new namespace: %w", err)
  129  	}
  130  	cleanupNeeded = true
  131  
  132  	defer ns.Close()
  133  
  134  	// Create the Veth and Vpeer
  135  	vethAttrs := netlink.NewLinkAttrs()
  136  	vethAttrs.Name = s.VethName()
  137  	veth := &netlink.Veth{
  138  		LinkAttrs: vethAttrs,
  139  		PeerName:  s.VpeerName(),
  140  	}
  141  
  142  	err = netlink.LinkAdd(veth)
  143  	if err != nil {
  144  		return fmt.Errorf("error creating veth device: %w", err)
  145  	}
  146  
  147  	vpeer, err := netlink.LinkByName(s.VpeerName())
  148  	if err != nil {
  149  		return fmt.Errorf("error finding vpeer: %w", err)
  150  	}
  151  
  152  	err = netlink.LinkSetUp(vpeer)
  153  	if err != nil {
  154  		return fmt.Errorf("error setting vpeer device up: %w", err)
  155  	}
  156  
  157  	err = netlink.AddrAdd(vpeer, &netlink.Addr{
  158  		IPNet: &net.IPNet{
  159  			IP:   s.VpeerIP(),
  160  			Mask: s.VrtMask(),
  161  		},
  162  	})
  163  	if err != nil {
  164  		return fmt.Errorf("error adding vpeer device address: %w", err)
  165  	}
  166  
  167  	// Move Veth device to the host NS
  168  	err = netlink.LinkSetNsFd(veth, int(hostNS))
  169  	if err != nil {
  170  		return fmt.Errorf("error moving veth device to the host namespace: %w", err)
  171  	}
  172  
  173  	err = netns.Set(hostNS)
  174  	if err != nil {
  175  		return fmt.Errorf("error setting network namespace: %w", err)
  176  	}
  177  
  178  	vethInHost, err := netlink.LinkByName(s.VethName())
  179  	if err != nil {
  180  		return fmt.Errorf("error finding veth: %w", err)
  181  	}
  182  
  183  	err = netlink.LinkSetUp(vethInHost)
  184  	if err != nil {
  185  		return fmt.Errorf("error setting veth device up: %w", err)
  186  	}
  187  
  188  	err = netlink.AddrAdd(vethInHost, &netlink.Addr{
  189  		IPNet: &net.IPNet{
  190  			IP:   s.VethIP(),
  191  			Mask: s.VrtMask(),
  192  		},
  193  	})
  194  	if err != nil {
  195  		return fmt.Errorf("error adding veth device address: %w", err)
  196  	}
  197  
  198  	err = netns.Set(ns)
  199  	if err != nil {
  200  		return fmt.Errorf("error setting network namespace to %s: %w", ns.String(), err)
  201  	}
  202  
  203  	// Create Tap device for FC in NS
  204  	tapAttrs := netlink.NewLinkAttrs()
  205  	tapAttrs.Name = s.TapName()
  206  	tapAttrs.Namespace = ns
  207  	tap := &netlink.Tuntap{
  208  		Mode:      netlink.TUNTAP_MODE_TAP,
  209  		LinkAttrs: tapAttrs,
  210  	}
  211  
  212  	err = netlink.LinkAdd(tap)
  213  	if err != nil {
  214  		return fmt.Errorf("error creating tap device: %w", err)
  215  	}
  216  
  217  	// Keep resumed guests' cached gateway ARP entries valid. Tap devices are
  218  	// isolated in per-sandbox network namespaces, so sharing this MAC is safe.
  219  	err = netlink.LinkSetHardwareAddr(tap, tapHostHardwareAddr)
  220  	if err != nil {
  221  		return fmt.Errorf("error setting tap device hardware address: %w", err)
  222  	}
  223  
  224  	err = netlink.LinkSetUp(tap)
  225  	if err != nil {
  226  		return fmt.Errorf("error setting tap device up: %w", err)
  227  	}
  228  
  229  	err = netlink.AddrAdd(tap, &netlink.Addr{
  230  		IPNet: &net.IPNet{
  231  			IP:   s.TapIP(),
  232  			Mask: s.TapCIDR(),
  233  		},
  234  	})
  235  	if err != nil {
  236  		return fmt.Errorf("error setting address of the tap device: %w", err)
  237  	}
  238  
  239  	// Set NS lo device up
  240  	lo, err := netlink.LinkByName(loopbackInterface)
  241  	if err != nil {
  242  		return fmt.Errorf("error finding lo: %w", err)
  243  	}
  244  
  245  	err = netlink.LinkSetUp(lo)
  246  	if err != nil {
  247  		return fmt.Errorf("error setting lo device up: %w", err)
  248  	}
  249  
  250  	// Add NS default route
  251  	err = netlink.RouteAdd(&netlink.Route{
  252  		Scope: netlink.SCOPE_UNIVERSE,
  253  		Gw:    s.VethIP(),
  254  	})
  255  	if err != nil {
  256  		return fmt.Errorf("error adding default NS route: %w", err)
  257  	}
  258  
  259  	tables, err := iptables.New()
  260  	if err != nil {
  261  		return fmt.Errorf("error initializing iptables: %w", err)
  262  	}
  263  
  264  	// Add NAT routing rules to NS
  265  	err = tables.Append("nat", "POSTROUTING", "-o", s.VpeerName(), "-s", s.NamespaceIP(), "-j", "SNAT", "--to", s.HostIPString())
  266  	if err != nil {
  267  		return fmt.Errorf("error creating postrouting rule to vpeer: %w", err)
  268  	}
  269  
  270  	err = tables.Append("nat", "PREROUTING", "-i", s.VpeerName(), "-d", s.HostIPString(), "-j", "DNAT", "--to", s.NamespaceIP())
  271  	if err != nil {
  272  		return fmt.Errorf("error creating postrouting rule from vpeer: %w", err)
  273  	}
  274  
  275  	// Marker for downstream L3 firewalls. 0 disables; see SANDBOX_EGRESS_DSCP.
  276  	// Slots are pooled: this seeds the untenanted class, Pool.Get re-stamps
  277  	// for builds, recycle restores it.
  278  	dscp := s.config.untenantedDSCP()
  279  	if dscp > 0 {
  280  		err = tables.Append("mangle", "POSTROUTING", s.dscpMangleRuleArgs(dscp)...)
  281  		if err != nil {
  282  			return fmt.Errorf("error creating DSCP mangle rule on vpeer: %w", err)
  283  		}
  284  	}
  285  	s.egressDSCP.Store(uint32(dscp))
  286  
  287  	err = s.InitializeFirewall()
  288  	if err != nil {
  289  		return fmt.Errorf("error initializing slot firewall: %w", err)
  290  	}
  291  
  292  	// Go back to original namespace
  293  	err = netns.Set(hostNS)
  294  	if err != nil {
  295  		return fmt.Errorf("error setting network namespace to %s: %w", hostNS.String(), err)
  296  	}
  297  
  298  	// Add routing from host to FC namespace
  299  	err = netlink.RouteAdd(&netlink.Route{
  300  		Gw:  s.VpeerIP(),
  301  		Dst: s.HostNet(),
  302  	})
  303  	if err != nil {
  304  		return fmt.Errorf("error adding route from host to FC: %w", err)
  305  	}
  306  
  307  	// Add host forwarding rules
  308  	err = tables.Append("filter", "FORWARD", "-i", s.VethName(), "-o", defaultGateway, "-j", "ACCEPT")
  309  	if err != nil {
  310  		return fmt.Errorf("error creating forwarding rule to default gateway: %w", err)
  311  	}
  312  
  313  	err = tables.Append("filter", "FORWARD", "-i", defaultGateway, "-o", s.VethName(), "-j", "ACCEPT")
  314  	if err != nil {
  315  		return fmt.Errorf("error creating forwarding rule from default gateway: %w", err)
  316  	}
  317  
  318  	// Add host postrouting rules
  319  	err = tables.Append("nat", "POSTROUTING", "-s", s.HostCIDR(), "-o", defaultGateway, "-j", "MASQUERADE")
  320  	if err != nil {
  321  		return fmt.Errorf("error creating postrouting rule: %w", err)
  322  	}
  323  
  324  	// Redirect traffic destined for hyperloop proxy
  325  	err = tables.Append(
  326  		"nat", "PREROUTING", "-i", s.VethName(),
  327  		"-p", "tcp", "-d", s.config.OrchestratorInSandboxIPAddress, "--dport", "80",
  328  		"-j", "REDIRECT", "--to-port", s.hyperloopPort,
  329  	)
  330  	if err != nil {
  331  		return fmt.Errorf("error creating HTTP redirect rule to sandbox hyperloop proxy server: %w", err)
  332  	}
  333  
  334  	// Redirect traffic destined for portmapper
  335  	err = tables.Append("nat", "PREROUTING",
  336  		"--in-interface", s.VethName(), "--protocol", "tcp",
  337  		"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "111",
  338  		"--jump", "REDIRECT", "--to-port", fmt.Sprintf("%d", s.config.PortmapperPort),
  339  	)
  340  	if err != nil {
  341  		return fmt.Errorf("error creating NFS redirect rule to sandbox portmapper server: %w", err)
  342  	}
  343  
  344  	// Redirect traffic destined for NFS proxy
  345  	err = tables.Append("nat", "PREROUTING",
  346  		"--in-interface", s.VethName(), "--protocol", "tcp",
  347  		"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "2049",
  348  		"--jump", "REDIRECT", "--to-port", fmt.Sprintf("%d", s.config.NFSProxyPort),
  349  	)
  350  	if err != nil {
  351  		return fmt.Errorf("error creating NFS redirect rule to sandbox NFS proxy server: %w", err)
  352  	}
  353  
  354  	// Create rules needed by egress proxy
  355  	err = s.egressProxy.OnSlotCreate(s, tables)
  356  	if err != nil {
  357  		return err
  358  	}
  359  
  360  	return nil
  361  }
  362  
  363  func (s *Slot) RemoveNetwork() error {
  364  	var errs []error
  365  
  366  	err := s.CloseFirewall()
  367  	if err != nil {
  368  		errs = append(errs, fmt.Errorf("error closing firewall: %w", err))
  369  	}
  370  
  371  	tables, err := iptables.New()
  372  	if err != nil {
  373  		errs = append(errs, fmt.Errorf("error initializing iptables: %w", err))
  374  	} else {
  375  		// Delete host forwarding rules
  376  		err = tables.Delete("filter", "FORWARD", "-i", s.VethName(), "-o", defaultGateway, "-j", "ACCEPT")
  377  		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting host forwarding rule to default gateway: %w")
  378  
  379  		err = tables.Delete("filter", "FORWARD", "-i", defaultGateway, "-o", s.VethName(), "-j", "ACCEPT")
  380  		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting host forwarding rule from default gateway: %w")
  381  
  382  		// Delete host postrouting rules
  383  		err = tables.Delete("nat", "POSTROUTING", "-s", s.HostCIDR(), "-o", defaultGateway, "-j", "MASQUERADE")
  384  		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting host postrouting rule: %w")
  385  
  386  		// Delete hyperloop proxy redirect rule
  387  		err = tables.Delete(
  388  			"nat", "PREROUTING", "-i", s.VethName(),
  389  			"-p", "tcp", "-d", s.config.OrchestratorInSandboxIPAddress, "--dport", "80",
  390  			"-j", "REDIRECT", "--to-port", s.hyperloopPort,
  391  		)
  392  		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting sandbox hyperloop proxy redirect rule: %w")
  393  
  394  		// Delete changes made by egress proxy
  395  		err = s.egressProxy.OnSlotDelete(s, tables)
  396  		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "%w")
  397  	}
  398  
  399  	// Delete routing from host to FC namespace
  400  	err = netlink.RouteDel(&netlink.Route{
  401  		Gw:  s.VpeerIP(),
  402  		Dst: s.HostNet(),
  403  	})
  404  	appendUnlessExpectedAbsentf(&errs, err, isRouteNotExist, "error deleting route from host to FC: %w")
  405  
  406  	// Delete veth device
  407  	// We explicitly delete the veth device from the host namespace because even though deleting
  408  	// is deleting the device there may be a race condition when creating a new veth device with
  409  	// the same name immediately after deleting the namespace.
  410  	veth, err := netlink.LinkByName(s.VethName())
  411  	if err != nil {
  412  		appendUnlessExpectedAbsentf(&errs, err, isLinkNotExist, "error finding veth: %w")
  413  	} else {
  414  		err = netlink.LinkDel(veth)
  415  		appendUnlessExpectedAbsentf(&errs, err, isLinkNotExist, "error deleting veth device: %w")
  416  	}
  417  
  418  	if tables != nil {
  419  		// Delete NFS proxy redirect rule
  420  		err = tables.Delete("nat", "PREROUTING",
  421  			"--in-interface", s.VethName(), "--protocol", "tcp",
  422  			"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "2049",
  423  			"--jump", "REDIRECT", "--to-port", strconv.Itoa(int(s.config.NFSProxyPort)),
  424  		)
  425  		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting sandbox NFS proxy redirect rule: %w")
  426  
  427  		// Delete portmapper redirect rule
  428  		err = tables.Delete("nat", "PREROUTING",
  429  			"--in-interface", s.VethName(), "--protocol", "tcp",
  430  			"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "111",
  431  			"--jump", "REDIRECT", "--to-port", strconv.Itoa(int(s.config.PortmapperPort)),
  432  		)
  433  		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting sandbox portmapper redirect rule: %w")
  434  	}
  435  
  436  	// Delete the named namespace only after every host-side (root namespace)
  437  	// teardown above has succeeded. The /run/netns entry is the anchor that
  438  	// startup reclaim uses to rediscover a leaked slot, and the host-side
  439  	// iptables/route/veth state removed above is keyed by the slot index. If any
  440  	// of that teardown failed, deleting the namespace now would orphan the
  441  	// remaining state with no way to rediscover and retry it. Preserving the
  442  	// anchor lets the next teardown attempt (including startup reclaim) finish
  443  	// the job. CreateNetwork removes a stale anchor before reusing the slot.
  444  	if len(errs) > 0 {
  445  		return errors.Join(errs...)
  446  	}
  447  
  448  	err = netns.DeleteNamed(s.NamespaceID())
  449  	appendUnlessExpectedAbsentf(&errs, err, isNamespaceNotExist, "error deleting namespace: %w")
  450  
  451  	return errors.Join(errs...)
  452  }
````
Plus the egress-proxy REDIRECTs appended by `OnSlotCreate` (tcpfirewall/proxy.go L152-L173, quoted in P4.3).
Resulting rules for slot N (host netns, iptables legacy/nft via the `iptables` binary; `go-iptables`
v0.8.0):

```
-t filter -A FORWARD -i veth-N -o <gwif> -j ACCEPT
-t filter -A FORWARD -i <gwif> -o veth-N -j ACCEPT
-t nat -A POSTROUTING -s 10.11.0.N/32 -o <gwif> -j MASQUERADE
-t nat -A PREROUTING -i veth-N -p tcp -d 192.0.2.1 --dport 80   -j REDIRECT --to-port 5010
-t nat -A PREROUTING -i veth-N -p tcp -d 192.0.2.1 --dport 111  -j REDIRECT --to-port 5012
-t nat -A PREROUTING -i veth-N -p tcp -d 192.0.2.1 --dport 2049 -j REDIRECT --to-port 5011
-t nat -A PREROUTING -i veth-N -p tcp --dport 80  -j REDIRECT --to-port 5016
-t nat -A PREROUTING -i veth-N -p tcp --dport 443 -j REDIRECT --to-port 5017
-t nat -A PREROUTING -i veth-N -p tcp             -j REDIRECT --to-port 5018
route: 10.11.0.N/32 via 10.12.0.(2N+1)
```
Inside netns `ns-N`: `nat POSTROUTING -o eth0 -s 169.254.0.21 -j SNAT --to 10.11.0.N`,
`nat PREROUTING -i eth0 -d 10.11.0.N -j DNAT --to 169.254.0.21`, optional mangle DSCP, default route via
`10.12.0.2N`, and the nftables `inet slot-firewall` table (P4.2).
Host-wide from Embed host-setup (not the orchestrator): `-t mangle -I FORWARD 1 -p tcp --tcp-flags SYN,RST
SYN -j TCPMSS --clamp-mss-to-pmtu`. v1 startup (reclaim enabled) also purges a stale nft table
`v2-host-firewall` (run.go L866-L871).

## D5. DNS inside the guest

The resolver is baked into the template rootfs at build time: `/etc/resolv.conf` = `nameserver 8.8.8.8`,
made immutable during provisioning and unlocked at the end:

`packages/shared/pkg/sandbox-network/firewall.go` L16-L20:

````go
   16  const (
   17  	AllInternetTrafficCIDR = "0.0.0.0/0"
   18  
   19  	DefaultNameserver = "8.8.8.8"
   20  )
````

`packages/orchestrator/pkg/template/build/core/rootfs/files/resolv.conf.tpl` (full file, 3 lines):

````text
    1  {{- /*gotype:github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/rootfs.templateModel*/ -}}
    2  {{ .WriteFile "/etc/resolv.conf" 0o644 }}
    3  
    4  nameserver {{ .Nameserver }}
````

`packages/orchestrator/pkg/template/build/core/rootfs/templates.go` L54-L63:

````go
   54  func newTemplateModel(buildContext buildcontext.BuildContext, provisionLogPrefix, provisionResultPath string) *templateModel {
   55  	return &templateModel{
   56  		Context:             buildContext,
   57  		Hostname:            "e2b.local",
   58  		ProvisionLogPrefix:  provisionLogPrefix,
   59  		ProvisionExitPrefix: ProvisioningExitPrefix,
   60  		ProvisionResultPath: provisionResultPath,
   61  		Nameserver:          sandbox_network.DefaultNameserver,
   62  	}
   63  }
````

`packages/orchestrator/pkg/template/build/phases/base/provision.sh` L13-L14:

````bash
   13  echo "Making configuration immutable"
   14  $BUSYBOX chattr +i /etc/resolv.conf
````

`packages/orchestrator/pkg/template/build/phases/base/provision.sh` L169-L170:

````bash
  169  echo "Unlocking immutable configuration"
  170  $BUSYBOX chattr -i /etc/resolv.conf
````
Kernel `ip=` argument (no usable DNS field; the last field receives the tap name):

`packages/orchestrator/pkg/sandbox/fc/process.go` L395-L397:

````go
  395  	// IPv4 configuration - format: [local_ip]::[gateway_ip]:[netmask]:hostname:iface:dhcp_option:[dns]
  396  	ipv4 := fmt.Sprintf("%s::%s:%s:instance:%s:off:%s", p.slot.NamespaceIP(), p.slot.TapIPString(), p.slot.TapMaskString(), p.slot.VpeerName(), p.slot.TapName())
  397  	kernelArgs := buildKernelArgs(ipv4, options).String()
````

`packages/orchestrator/pkg/sandbox/fc/kernel_args.go` L96-L123:

````go
   96  func buildKernelArgsFor(arch string, ipv4 string, options ProcessOptions) KernelArgs {
   97  	args := KernelArgs{
   98  		// Disable kernel logs for production to speed the FC operations
   99  		// https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md#logging-and-performance
  100  		"quiet":    "",
  101  		"loglevel": "1",
  102  
  103  		// Define kernel init path
  104  		"init": options.InitScriptPath,
  105  
  106  		// Networking IPv4 and IPv6
  107  		"ip":            ipv4,
  108  		"ipv6.disable":  "0",
  109  		"ipv6.autoconf": "1",
  110  
  111  		// Wait 1 second before exiting FC after panic or reboot
  112  		"panic": "1",
  113  
  114  		"reboot":           "k",
  115  		"pci":              "off",
  116  		"random.trust_cpu": "on",
  117  
  118  		"rootflags": ext4RootFlags,
  119  
  120  		// The kernel defaults to SELinux; an image whose /etc/selinux/config says enforcing
  121  		// would hang on the unlabeled rootfs before envd starts.
  122  		"selinux": "0",
  123  	}
````
=> `ip=169.254.0.21::169.254.0.22:255.255.255.252:instance:eth0:off:tap0`. DNS to 8.8.8.8 is UDP/TCP 53:
UDP is forwarded/MASQUERADEd out of the default-gateway interface; TCP 53 goes through the tcpfirewall
"other" port. Changing guest DNS requires a different `/etc/resolv.conf` in the template (or at runtime via
envd), and if the resolver is private (e.g. k3s CoreDNS 10.43.0.10) the P4 caveats (layer-1 exemption +
non-default-interface FORWARD/MASQUERADE for UDP) apply.

---

# E. Gotchas for a standalone Debian 13 deploy

E1. `ENVIRONMENT=local` (as Embed sets) also: disables the `/orchestrator.lock` single-instance guard
(run.go L398-L419, `IsDevelopment`), turns on fallbacks of `use-nfs-for-snapshots`, `use-nfs-for-templates`,
`use-nfs-for-building-templates`, `create-storage-cache-spans`, `can-use-persistent-volumes`,
`freeze-user-cgroup`, `network-transform-rules`, `byop-proxy-enabled`, `enable-sandbox-iam-tokens`,
`envd-binary-cache` (flags.go L158-L162, L322-L325, L391-L403, L1056), enables guest kernel logs in
template builds (create_sandbox.go L151) and BYOP dev CIDRs (byop.go L20-L26). Prefer `ENVIRONMENT=prod`
(default) + the P2 patch.

E2. `NODE_ID` is mandatory (panic). `ARTIFACTS_REGISTRY_PROVIDER=Local` is mandatory when template-manager
runs (default `GCP_ARTIFACTS`). Set `TEMPLATE_STORAGE_URL`/`BUILD_CACHE_STORAGE_URL` explicitly (legacy
defaults point at `/tmp`).

E3. `DEFAULT_KERNEL_VERSION` must match the fetched kernel directory (A1.7).

E4. Host packages: `iptables rsync e2fsprogs iproute2 util-linux curl` (+ build: `gcc libc6-dev
linux-libc-dev git make`), kernel modules `nbd` (nbds_max > NBD_POOL_SIZE), `tun`, `kvm_*`; sysctls
`vm.nr_hugepages` (only if huge_pages requested), `vm.max_map_count=1048576`, `net.ipv4.ip_forward=1`;
cgroup v2 at `/sys/fs/cgroup`.

E5. Startup reclaim (P1) is host-wide by default for Firecracker processes, NBD devices, `ns-<int>`
netns and `/tmp/fc-*-*.sock`-style files; either patch or ensure nothing else on the host matches.

E6. SIGTERM waits for sandboxes to end by themselves (P2); a systemd unit without the launcher's cgroup
kill (or FORCE_STOP / the patch) will hang until `TimeoutStopSec`.

E7. Listeners bind all interfaces (A5); firewall them.
