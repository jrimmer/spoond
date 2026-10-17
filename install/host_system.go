// The host phase's system steps: packages, kernel modules, the nbd
// udev rule, hugepages and the legacy iptables backend — the parts of
// deploy/e2b/host-setup.sh that have no spoond state of their own.
// Every write is compared before it lands, every command goes through
// the Env (query runs in a dry run too, exec does not), and a failure
// carries the fix with no phase prefix.
package install

import (
	"bufio"
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
	hostSteps["hugepages"] = hostHugepages
	hostSteps["iptables"] = hostIptables
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

// nbdParamPath is the running module's parameter; tunModulePath is
// the loaded-module marker. The checks decide whether a modprobe is
// needed at all, so a settled host reruns the step without touching
// the kernel.
const (
	nbdParamPath  = "/sys/module/nbd/parameters/nbds_max"
	tunModulePath = "/sys/module/tun"
)

// hostModules loads the kernel modules the leases run on: nbd (256
// devices, 16 partitions each), tun and kvm. The configs land first so
// the next boot matches; a module already loaded in the wanted state
// is left alone, the rest get their modprobe. The running nbds_max is
// always read back: nbd is often loaded from an earlier life with a
// smaller count, and only a reboot (or rmmod without leases) fixes
// that.
func hostModules(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	if err := env.writeFile(r, "/etc/modules-load.d/e2b.conf", []byte(modulesConf), 0o644); err != nil {
		return err
	}
	if err := env.writeFile(r, "/etc/modprobe.d/e2b-nbd.conf", []byte(nbdConf), 0o644); err != nil {
		return err
	}

	// nbd: loaded with the right count is a kept; loaded smaller is
	// the one failure a rerun cannot fix; absent wants the modprobe.
	loaded, err := nbdLoaded(env)
	switch {
	case errors.Is(err, errNotLoaded):
		if err := env.exec(ctx, "modprobe", "nbd", "nbds_max=256", "max_part=16"); err != nil {
			return fmt.Errorf("%w — modprobe could not load nbd; fix the kernel output above, then rerun", err)
		}
		if env.DryRun {
			// The load never happened, so there is nothing to read
			// back; the row says what the real run would do.
			r.Add(Skipped, "modprobe nbd", "dry-run: would load with nbds_max=256 max_part=16")
			return loadTun(ctx, env, r)
		}
		if loaded, err = nbdLoaded(env); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if loaded < wantNbdsMax {
		return fmt.Errorf("nbd is loaded with nbds_max=%d; run `rmmod nbd` (no leases running) or reboot, then rerun", loaded)
	}
	r.Add(Kept, "nbd", fmt.Sprintf("loaded with nbds_max=%d", loaded))
	return loadTun(ctx, env, r)
}

// loadTun loads tun when the module is not in the kernel; the module
// directory under /sys is the loaded marker.
func loadTun(ctx context.Context, env *Env, r *Reporter) error {
	if _, err := os.Stat(env.Resolve(tunModulePath)); errors.Is(err, os.ErrNotExist) {
		if err := env.exec(ctx, "modprobe", "tun"); err != nil {
			return fmt.Errorf("%w — modprobe could not load tun; fix the kernel output above, then rerun", err)
		}
		r.Add(Changed, "modprobe tun", "")
		return nil
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", tunModulePath, err)
	}
	r.Add(Kept, "tun", "loaded")
	return nil
}

// errNotLoaded says the nbd module is not in the kernel.
var errNotLoaded = errors.New("nbd is not loaded")

// nbdLoaded returns the running nbds_max, or errNotLoaded when the nbd
// module is not loaded.
func nbdLoaded(env *Env) (int, error) {
	b, err := os.ReadFile(env.Resolve(nbdParamPath))
	if errors.Is(err, os.ErrNotExist) {
		return 0, errNotLoaded
	}
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", nbdParamPath, err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", nbdParamPath, strings.TrimSpace(string(b)))
	}
	return got, nil
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

func init() { hostSteps["hugepages"] = hostHugepages }

// Hugepage sizing: 2 MiB pages, and the host keeps max(6 GiB, 17% of
// MemTotal); pages = what is left over 2 MiB, rounded down. 6 GiB in
// 2 MiB pages is 3072, the floor below which leases do not fit.
const (
	hugepageSize   = 2 << 20
	hugepagesFloor = 3072
)

// hugepagesFor is the auto formula: (MemTotal - max(6 GiB, 17%)) / 2 MiB,
// rounded down; the error carries the total when the host is too small.
func hugepagesFor(memTotalBytes int64) (int, error) {
	keep := memTotalBytes * 17 / 100
	if min := int64(6) << 30; keep < min {
		keep = min
	}
	left := memTotalBytes - keep
	if left < int64(hugepagesFloor)*hugepageSize {
		return 0, fmt.Errorf("not enough memory for leases: %d MiB", left>>20)
	}
	return int(left / hugepageSize), nil
}

// grubPath is the grub default file the kernel command line comes
// from; hugepagesToken is the token inside GRUB_CMDLINE_LINUX_DEFAULT
// that carries the reservation.
const (
	grubPath       = "/etc/default/grub"
	hugepagesToken = "hugepages="
)

// hostHugepages reserves the 2 MiB pages the lease pool runs from.
// The count is --hugepages, or auto: MemTotal minus what the host
// keeps. The count lands in /etc/sysctl.d/90-e2b.conf (applies now,
// via sysctl -p) and on the kernel command line in /etc/default/grub
// (via update-grub; a large pool rarely allocates fully on a running
// host). HugePages_Total is read back: below the count, only a reboot
// finishes the reservation and the phase asks for one after its other
// steps ran.
func hostHugepages(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	pages, err := hugepagesCount(env)
	if err != nil {
		return err
	}
	st.Hugepages = pages

	// The sysctl file: the reservation plus the two settings the
	// leases and the gateway networking need.
	sysctl := fmt.Sprintf("vm.nr_hugepages = %d\nvm.max_map_count = 1048576\nnet.ipv4.ip_forward = 1\n", pages)
	before, err := os.ReadFile(env.Resolve(sysctlE2BPath))
	changed := err != nil || string(before) != sysctl
	if err := env.writeFile(r, sysctlE2BPath, []byte(sysctl), 0o644); err != nil {
		return err
	}
	if changed && !env.DryRun {
		if err := env.exec(ctx, "sysctl", "-p", sysctlE2BPath); err != nil {
			return fmt.Errorf("%w — sysctl could not apply %s; fix the kernel output above, then rerun", err, sysctlE2BPath)
		}
		r.Add(Changed, sysctlE2BPath, "applied with sysctl -p")
	}

	// The kernel command line: replace an existing hugepages= token
	// inside GRUB_CMDLINE_LINUX_DEFAULT, keep every other token.
	grub, err := os.ReadFile(env.Resolve(grubPath))
	if err != nil {
		return fmt.Errorf("read %s: %w", grubPath, err)
	}
	out, changed := setGrubHugepages(string(grub), pages)
	if err := env.writeFile(r, grubPath, []byte(out), 0o644); err != nil {
		return err
	}
	if changed && !env.DryRun {
		if err := env.exec(ctx, "update-grub"); err != nil {
			return fmt.Errorf("%w — update-grub failed; fix the grub output above, then rerun", err)
		}
		r.Add(Changed, grubPath, "regenerated the grub config with update-grub")
	}

	// What did the kernel actually reserve?
	total, err := hugepagesTotal(env)
	if err != nil {
		return err
	}
	if total < pages {
		st.RebootNeeded = true
		r.Add(Skipped, hugepagesToken+strconv.Itoa(pages), fmt.Sprintf("%d of %d pages reserved; the rest comes at boot", total, pages))
		return nil
	}
	r.Add(Kept, hugepagesToken+strconv.Itoa(pages), fmt.Sprintf("%d pages reserved", total))
	return nil
}

// sysctlE2BPath is the sysctl drop-in the step writes.
const sysctlE2BPath = "/etc/sysctl.d/90-e2b.conf"

// hugepagesCount reads the wanted page count: --hugepages when it is a
// number, the auto formula over /proc/meminfo otherwise.
func hugepagesCount(env *Env) (int, error) {
	if hp := env.Opt.hugepages(); hp != HugepagesAuto {
		n, err := strconv.Atoi(hp)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("--hugepages %q: want a positive page count or %q", hp, HugepagesAuto)
		}
		return n, nil
	}
	total, err := memTotal(env)
	if err != nil {
		return 0, err
	}
	return hugepagesFor(total)
}

// meminfoValue reads the first "Name:   value kB" line for name from
// Root/proc/meminfo and returns the value in bytes.
func meminfoValue(env *Env, name string) (int64, error) {
	fh, err := os.Open(env.Resolve(meminfoPath))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", meminfoPath, err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+":") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("%s: %s without a value", meminfoPath, name)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %s %q: not a number", meminfoPath, name, fields[1])
		}
		return kb << 10, nil
	}
	return 0, fmt.Errorf("%s: no %s line", meminfoPath, name)
}

// hugepagesTotal reads HugePages_Total (pages, not kB) from
// /proc/meminfo; meminfoValue shifts by 1 KiB, so undo that.
func hugepagesTotal(env *Env) (int, error) {
	b, err := meminfoValue(env, "HugePages_Total")
	if err != nil {
		return 0, err
	}
	return int(b >> 10), nil
}

// setGrubHugepages rewrites GRUB_CMDLINE_LINUX_DEFAULT in a
// /etc/default/grub body: the hugepages= token inside it is replaced
// (or appended, or dropped at 0), every other token and every other
// line stays. changed reports whether the body differs.
func setGrubHugepages(body string, pages int) (out string, changed bool) {
	token := hugepagesToken + strconv.Itoa(pages)
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "GRUB_CMDLINE_LINUX_DEFAULT=") {
			continue
		}
		// The value is one quoted shell word; the tokens are its
		// space-separated fields.
		open := strings.Index(line, "\"")
		close := strings.LastIndex(line, "\"")
		if open < 0 || close <= open {
			continue // unquoted or malformed: leave the line alone
		}
		prefix, suffix := line[:open+1], line[close:]
		inner := line[open+1 : close]
		var toks []string
		for _, f := range strings.Fields(inner) {
			if strings.HasPrefix(f, hugepagesToken) {
				continue
			}
			toks = append(toks, f)
		}
		if pages > 0 {
			toks = append(toks, token)
		}
		newline := prefix + strings.Join(toks, " ") + suffix
		if newline != line {
			lines[i] = newline
		}
		// Grub files carry one such line.
		break
	}
	out = strings.Join(lines, "\n")
	return out, out != body
}

func init() { hostSteps["iptables"] = hostIptables }

// iptablesLegacyPath is the alternative the leases' NAT and MSS rules
// run under: the nft backend still translates them, but the E2B
// substrate is proven on legacy. ip6tables has its own link.
const iptablesLegacyPath = "/usr/sbin/iptables-legacy"

// legacyPath returns the legacy alternative link for one command name.
func legacyPath(name string) string {
	if name == "ip6tables" {
		return "/usr/sbin/ip6tables-legacy"
	}
	return iptablesLegacyPath
}

// hostIptables points the iptables alternatives at the legacy backend
// (iptables and ip6tables both). The query decides; the set runs only
// when the value differs, so a rerun is two queries and a kept row.
func hostIptables(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	for _, name := range []string{"iptables", "ip6tables"} {
		out, err := env.query(ctx, "update-alternatives", "--query", name)
		if err != nil {
			return fmt.Errorf("%w — update-alternatives could not read the %s alternative; is the iptables package installed?", err, name)
		}
		cur := alternativesValue(out)
		if cur == legacyPath(name) {
			r.Add(Kept, name, "legacy backend")
			continue
		}
		if err := env.exec(ctx, "update-alternatives", "--set", name, legacyPath(name)); err != nil {
			return fmt.Errorf("%w — could not switch %s to the legacy backend; fix the output above, then rerun", err, name)
		}
		note := fmt.Sprintf("was %s", cur)
		if env.DryRun {
			note = "dry-run: nothing changed"
		}
		r.Add(Changed, name, note)
	}
	return nil
}

// alternativesValue pulls the Value line out of `update-alternatives
// --query` output.
func alternativesValue(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "Value:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
