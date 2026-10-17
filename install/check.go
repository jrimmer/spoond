// The check phase: the first row of the install phase table. It reads
// the host as it is — memory, OS, KVM, the ports spoond binds, and the
// snapshot store's free space — and stops with the fix when the host
// cannot run spoond. Paths are resolved under Env.Root, so a test
// writes fake /proc and /dev files into a temp dir and runs the real
// logic; the two probes that cannot be faked that way (port liveness
// and free space) sit behind the prober and spacer interfaces.
package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// What check reads and requires (docs/plans/2026-10-16-4.0-install.md):
// root; x86-64; /dev/kvm; memory >= 16 GiB; Debian 13 (other
// distributions warn); kernel >= 6.1; free ports 8890 8891 2222 8893
// 5000 5007 5008; the snapshot store has >= 100 GiB free.

const (
	// minMemory is the memory floor: 16 GiB total (the plan's
	// hugepage rule keeps 6 GiB of it for the host).
	minMemory = 16 << 30
	// minStoreFree is the snapshot-store free-space floor: the
	// template storage and the build cache live there.
	minStoreFree = 100 << 30
	// minKernelMajor, minKernelMinor are the kernel floor: the UFFD
	// snapshot path and the nbd module want >= 6.1.
	minKernelMajor = 6
	minKernelMinor = 1
	// defaultStore is the snapshot store whose free space is
	// checked; the host phase's --store-dir overrides it later.
	defaultStore = "/forkdcache/e2b"
)

// checkPaths are the host files check reads; every read resolves them
// under Env.Root so tests write fakes into a temp dir.
const (
	meminfoPath = "/proc/meminfo"
	releasePath = "/etc/os-release"
	kvmPath     = "/dev/kvm"
	unamePath   = "/proc/sys/kernel/osrelease"
)

// requiredPorts are the listeners a full install takes: the backend
// API, the proxy and LLM gateway, the SSH gateway, the dashboard, the
// local image registry, and the orchestrator's proxy and gRPC ports.
var requiredPorts = []string{"8890", "8891", "2222", "8893", "5000", "5007", "5008"}

// prober isolates the port probe. ListenOK reports whether port is
// free; an error is a probe failure (not a busy port).
type prober interface {
	ListenOK(port string) (bool, error)
}

// spacer isolates the free-space probe.
type spacer interface {
	FreeBytes(dir string) (uint64, error)
}

// hostProber and hostSpacer are the production probes, swapped in
// tests.
var (
	hostProber prober = sysProber{}
	hostSpacer spacer = sysSpacer{}
)

// sysProber binds :port on tcp4 and tcp6 and closes immediately: the
// plain "does anybody already listen here" test.
type sysProber struct{}

func (sysProber) ListenOK(port string) (bool, error) {
	_, err4 := probeOne("tcp4", port)
	_, err6 := probeOne("tcp6", port)
	switch {
	case err4 != nil && !isBusy(err4):
		return false, err4
	case err6 != nil && !isBusy(err6):
		return false, err6
	}
	return err4 == nil && err6 == nil, nil
}

// probeOne reports whether addr binds on one stack.
func probeOne(network, port string) (bool, error) {
	l, err := net.Listen(network, ":"+port)
	if err != nil {
		return false, err
	}
	_ = l.Close()
	return true, nil
}

// isBusy says whether a listen error is "address already in use": that
// is a finding (the port is not free), not a probe failure.
func isBusy(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// sysSpacer reports the free bytes of a directory with statfs.
type sysSpacer struct{}

func (sysSpacer) FreeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// checkPhase implements the check row. Every failure names the fix.
func checkPhase(ctx context.Context, env *Env) ([]Item, error) {
	r := &Reporter{Phase: PhaseCheck, Out: env.Out}

	// root: a real run writes /etc, /opt, /lib and /var/lib. With a
	// non-default Root (tests; a future --root flag) the check is
	// not about root, so it is skipped.
	if env.Root == "" || env.Root == "/" {
		if os.Geteuid() != 0 {
			r.Add(Failed, "root", ErrNotRoot.Error())
			return r.Items(), fmt.Errorf("check: %w", ErrNotRoot)
		}
		r.Add(Kept, "root", "running as root")
	} else {
		r.Add(Skipped, "root", "writing under "+env.Root+", root not required")
	}

	// x86-64: the E2B substrate (Firecracker, envd) is amd64-only,
	// and the runtime tarball installs amd64 binaries.
	if runtime.GOARCH != "amd64" {
		note := fmt.Sprintf("this spoond is %s; the E2B substrate is amd64 only — run the amd64 spoond binary on an x86-64 host", runtime.GOARCH)
		r.Add(Failed, "arch", note)
		return r.Items(), fmt.Errorf("check: %s", note)
	}
	r.Add(Kept, "arch: x86-64", "")

	// /dev/kvm: Firecracker needs it; a missing device is the
	// commonest failure on a freshly installed nested VM.
	ok, err := kvmHas(env)
	switch {
	case err != nil:
		r.Add(Failed, "kvm", "check failed: "+err.Error())
		return r.Items(), fmt.Errorf("check: %w", err)
	case !ok:
		note := "/dev/kvm is missing — enable KVM on the host (BIOS/UEFI virtualisation, the kvm module; inside a nested VM, the hypervisor must expose virt)"
		r.Add(Failed, "kvm", note)
		return r.Items(), fmt.Errorf("check: %s", note)
	}
	r.Add(Kept, "/dev/kvm", "present")

	// memory >= 16 GiB.
	total, err := memTotal(env)
	switch {
	case err != nil:
		r.Add(Failed, "memory", "check failed: "+err.Error())
		return r.Items(), fmt.Errorf("check: %w", err)
	case total < minMemory:
		note := fmt.Sprintf("%d GiB total, want >= 16 — the default sizes and the hugepage reservation do not fit; add memory", total>>30)
		r.Add(Failed, "memory", note)
		return r.Items(), fmt.Errorf("check: %s", note)
	}
	r.Add(Kept, "memory", fmt.Sprintf("%d GiB total", total>>30))

	// Debian 13; other distributions warn (the host phase's apt set
	// pins trixie, and its kernel package names). A warning does
	// not stop the install.
	id, versionID, err := osRelease(env)
	switch {
	case err != nil:
		r.Add(Failed, "os", "check failed: "+err.Error())
		return r.Items(), fmt.Errorf("check: %w", err)
	case id == "debian" && versionID == "13":
		r.Add(Kept, "os", "Debian 13")
	case id != "":
		r.Add(Skipped, "os: "+id+" "+versionID, "not Debian 13 — the host phase's apt packages and unit paths are written for trixie; continue at your own risk")
	default:
		r.Add(Skipped, "os", "no ID in /etc/os-release — not recognised as Debian 13, continue at your own risk")
	}

	// kernel >= 6.1.
	maj, min, err := kernelVersion(env)
	switch {
	case err != nil:
		r.Add(Failed, "kernel", "check failed: "+err.Error())
		return r.Items(), fmt.Errorf("check: %w", err)
	case maj < minKernelMajor || (maj == minKernelMajor && min < minKernelMinor):
		note := fmt.Sprintf("%d.%d, want >= 6.1 — UFFD snapshots, the nbd module and unprivileged user namespaces want a trixie-era kernel; upgrade the host", maj, min)
		r.Add(Failed, "kernel", note)
		return r.Items(), fmt.Errorf("check: %s", note)
	}
	r.Add(Kept, "kernel", fmt.Sprintf("%d.%d", maj, min))

	// free ports.
	for _, port := range requiredPorts {
		free, err := hostProber.ListenOK(port)
		switch {
		case err != nil:
			r.Add(Failed, "port :"+port, "check failed: "+err.Error())
			return r.Items(), fmt.Errorf("check: %w", err)
		case !free:
			note := "in use — stop the service that binds it, or move spoond off it in the unit env (BIND_ADDR, PROXY_ADDR, GATEWAY_ADDR, DASH_ADDR, the registry publish port, E2B_GRPC_ADDR, E2B_PROXY_URL) before installing"
			r.Add(Failed, "port :"+port, note)
			return r.Items(), fmt.Errorf("check: %s", note)
		default:
			r.Add(Kept, "port :"+port, "free")
		}
	}

	// the snapshot store has >= 100 GiB free.
	free, err := storeFree(env)
	switch {
	case err != nil:
		r.Add(Failed, "store free space", "check failed: "+err.Error())
		return r.Items(), fmt.Errorf("check: %w", err)
	case free < minStoreFree:
		note := fmt.Sprintf("%s: %d GiB free, want >= 100 — point --store-disk/--store-dir at a bigger volume, or free space on the store filesystem", env.Resolve(defaultStore), free>>30)
		r.Add(Failed, "store free space", note)
		return r.Items(), fmt.Errorf("check: %s", note)
	default:
		r.Add(Kept, "store free space", fmt.Sprintf("%s: %d GiB free", env.Resolve(defaultStore), free>>30))
	}

	return r.Items(), nil
}

// storeFree reads the free bytes of the snapshot store. When the store
// does not exist yet (the host phase creates it), the nearest existing
// parent is measured instead and the report says so.
func storeFree(env *Env) (uint64, error) {
	dir := env.Resolve(defaultStore)
	free, err := hostSpacer.FreeBytes(dir)
	if err == nil {
		return free, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	parent := filepath.Dir(dir)
	free, err = hostSpacer.FreeBytes(parent)
	if err != nil {
		return 0, fmt.Errorf("statfs %s (the store %s does not exist yet): %w", parent, dir, err)
	}
	return free, nil
}

// memTotal reads MemTotal (kB) from Root/proc/meminfo.
func memTotal(env *Env) (int64, error) {
	fh, err := os.Open(env.Resolve(meminfoPath))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", meminfoPath, err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("%s: MemTotal without a value", meminfoPath)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: MemTotal %q: not a number", meminfoPath, fields[1])
		}
		return kb << 10, nil
	}
	return 0, fmt.Errorf("%s: no MemTotal line", meminfoPath)
}

// osRelease reads ID and VERSION_ID from Root/etc/os-release.
func osRelease(env *Env) (id, versionID string, err error) {
	fh, err := os.Open(env.Resolve(releasePath))
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", releasePath, err)
	}
	defer fh.Close()
	props := map[string]string{}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		props[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return props["ID"], props["VERSION_ID"], nil
}

// kernelVersion reads Root/proc/sys/kernel/osrelease as major.minor.
func kernelVersion(env *Env) (int, int, error) {
	b, err := os.ReadFile(env.Resolve(unamePath))
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", unamePath, err)
	}
	ver := strings.TrimSpace(string(b))
	if i := strings.IndexByte(ver, '-'); i >= 0 {
		ver = ver[:i]
	}
	ver = strings.TrimSuffix(ver, ".0")
	maj, mino, ok := strings.Cut(ver, ".")
	if !ok {
		return 0, 0, fmt.Errorf("%s: %q: want major.minor", unamePath, strings.TrimSpace(string(b)))
	}
	maji, err := strconv.Atoi(maj)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %q: major is not a number", unamePath, maj)
	}
	mini, err := strconv.Atoi(mino)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %q: minor is not a number", unamePath, mino)
	}
	return maji, mini, nil
}

// kvmHas stats Root/dev/kvm.
func kvmHas(env *Env) (bool, error) {
	if _, err := os.Stat(env.Resolve(kvmPath)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", kvmPath, err)
	}
	return true, nil
}