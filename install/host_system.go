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
	"strings"
)

func init() { hostSteps["packages"] = hostPackages }

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
