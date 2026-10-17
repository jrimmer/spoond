// The host phase's system steps: packages, kernel modules, the nbd
// udev rule, hugepages and the legacy iptables backend — the parts of
// deploy/e2b/host-setup.sh that have no spoond state of their own.
// Every write is compared before it lands, every command goes through
// the Env (query runs in a dry run too, exec does not), and a failure
// carries the fix with no phase prefix.
package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func init() {
	hostSteps["packages"] = hostPackages
	hostSteps["modules"] = hostModules
	hostSteps["udev"] = hostUdev
}

// basePackages are the host packages every install needs: the firewall
// tools, the snapshot store's filesystem tools, Docker and its runtime.
var basePackages = []string{
	"iptables", "nftables", "rsync", "e2fsprogs", "iproute2", "util-linux",
	"curl", "ca-certificates", "docker.io", "docker-buildx", "containerd", "runc",
}

// zfsPackages turn --store-disk into the forkdcache pool: zfs-dkms
// builds the module against the running kernel's headers.
var zfsPackages = []string{"linux-headers-amd64", "zfs-dkms", "zfsutils-linux", "zfs-zed"}

// hostPackages installs the host packages. Each is checked with
// dpkg-query; the missing ones go through one apt-get install, after
// apt-get update only when something is missing. With --store-disk the
// ZFS set joins the list, and zfs-dkms's apt candidate is checked
// first: it lives in contrib, which a default Debian mirror list
// leaves out.
func hostPackages(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	pkgs := basePackages
	if env.Opt.StoreDisk != "" {
		pkgs = append(pkgs, zfsPackages...)
	}
	var missing []string
	for _, pkg := range pkgs {
		out, err := env.query(ctx, "dpkg-query", "-W", "-f=${Status}", pkg)
		if err == nil && strings.TrimSpace(string(out)) == "install ok installed" {
			r.Add(Kept, pkg, "installed")
			continue
		}
		missing = append(missing, pkg)
	}
	if len(missing) == 0 {
		return nil
	}
	// A fresh host has no package lists yet; refresh before the one
	// install (and before the contrib check below reads a candidate).
	if err := env.exec(ctx, "apt-get", "update"); err != nil {
		return fmt.Errorf("%w — apt could not refresh its package lists; fix the apt output above, then rerun", err)
	}
	if env.Opt.StoreDisk != "" {
		out, err := env.query(ctx, "apt-cache", "policy", "zfs-dkms")
		if err != nil {
			return fmt.Errorf("%w — apt-cache could not read the package lists", err)
		}
		if aptNoCandidate(out) {
			return errors.New("zfs-dkms: enable the contrib component in /etc/apt/sources.list.d/debian.sources, then rerun")
		}
	}
	args := append([]string{"install", "-y", "--no-install-recommends"}, missing...)
	if err := env.exec(ctx, "apt-get", args...); err != nil {
		return fmt.Errorf("%w — apt could not install the packages; fix the apt output above, then rerun", err)
	}
	for _, pkg := range missing {
		note := ""
		if env.DryRun {
			note = "dry-run: nothing changed"
		}
		r.Add(Changed, pkg, note)
	}
	return nil
}

// aptNoCandidate reports whether `apt-cache policy` output names no
// install candidate for the package (it is in no enabled component).
func aptNoCandidate(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Candidate:"); ok {
			return strings.TrimSpace(v) == "(none)"
		}
	}
	// No policy output at all: apt knows no package by that name.
	return true
}

func init() { hostSteps["modules"] = hostModules }

// modulesConf loads nbd, tun and kvm at boot; nbdConf carries nbd's
// sizes (the module parameter only applies at load time, so it must
// not rely on the boot-time load order).
const (
	modulesConf = "nbd\ntun\nkvm\n"
	nbdConf     = "options nbd nbds_max=256 max_part=16\n"
)

// wantNbdsMax is the device count the leases need; wantNbdMaxPart is
// the partitions per device.
const (
	wantNbdsMax    = 256
	wantNbdMaxPart = 16
)

// nbdParamPath is the running module's parameter, read back after the
// modprobe; a smaller value means nbd loaded before the option did.
const nbdParamPath = "/sys/module/nbd/parameters/nbds_max"

// hostModules loads the kernel modules the leases run on: nbd (256
// devices, 16 partitions each), tun and kvm. The configs land first so
// the next boot matches, then the modules load now. The running
// nbds_max is read back: nbd is often already loaded from an earlier
// life with a smaller count, and only a reboot (or rmmod without
// leases) fixes that.
func hostModules(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	if err := env.writeFile(r, "/etc/modules-load.d/e2b.conf", []byte(modulesConf), 0o644); err != nil {
		return err
	}
	if err := env.writeFile(r, "/etc/modprobe.d/e2b-nbd.conf", []byte(nbdConf), 0o644); err != nil {
		return err
	}
	if err := env.exec(ctx, "modprobe", "nbd", "nbds_max=256", "max_part=16"); err != nil {
		return fmt.Errorf("%w — modprobe could not load nbd; fix the kernel output above, then rerun", err)
	}
	r.Add(Kept, "modprobe nbd", "")
	if err := env.exec(ctx, "modprobe", "tun"); err != nil {
		return fmt.Errorf("%w — modprobe could not load tun; fix the kernel output above, then rerun", err)
	}
	r.Add(Kept, "modprobe tun", "")

	b, err := os.ReadFile(env.Resolve(nbdParamPath))
	if err != nil {
		return fmt.Errorf("read %s: %w", nbdParamPath, err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("%s: %q is not a number", nbdParamPath, strings.TrimSpace(string(b)))
	}
	if got < wantNbdsMax {
		return fmt.Errorf("nbd is loaded with nbds_max=%d; run `rmmod nbd` (no leases running) or reboot, then rerun", got)
	}
	r.Add(Kept, nbdParamPath, fmt.Sprintf("nbds_max=%d", got))
	return nil
}

func init() { hostSteps["udev"] = hostUdev }

// nbdUdevRule gives every nbd device to group disk, mode 0660, so the
// leases' users can open them; host-setup.sh ships the same rule.
const nbdUdevRule = `KERNEL=="nbd*", GROUP="disk", MODE="0660"
`

// hostUdev writes the nbd device rule. The reload is skipped: the rule
// applies when the devices next appear (after the reboot the hugepages
// step may ask for, or at the next module load), and systemd-udevd
// re-reads the rules directory on every udevadm trigger.
func hostUdev(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	return env.writeFile(r, "/etc/udev/rules.d/97-nbd-device.rules", []byte(nbdUdevRule), 0o644)
}
