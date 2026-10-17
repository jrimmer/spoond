// Tests for the host phase's system steps, one test per step, all
// against a temp root with the fake runner. The host files the steps
// read (/proc/meminfo, /etc/default/grub, /sys) are fakes under the
// root; changing commands go through recordRunner, so a test asserts
// on the exact command list.
package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// addReply teaches runner the answer to one command; the key is the
// command line ("name arg1 arg2").
func addReply(runner *recordRunner, key string, out string) {
	if runner.reply == nil {
		runner.reply = map[string]fakeReply{}
	}
	runner.reply[key] = fakeReply{out: []byte(out)}
}

// installed lists the packages dpkg-query should answer present for.
func installed(runner *recordRunner, pkgs ...string) {
	for _, p := range pkgs {
		addReply(runner, "dpkg-query -W -f=${Status} "+p, "install ok installed")
	}
}

// TestHostPackagesInstallsMissingInOneAptGet: the missing packages go
// through one apt-get install after apt-get update; the present ones
// report kept.
func TestHostPackagesInstallsMissingInOneAptGet(t *testing.T) {
	env, _, runner := testEnv(t)
	runner.reply = map[string]fakeReply{}
	installed(runner, "iptables", "nftables")
	addReply(runner, "apt-get update", "")
	addReply(runner, "apt-get install -y --no-install-recommends rsync e2fsprogs iproute2 util-linux curl ca-certificates docker.io docker-buildx containerd runc", "")

	if err := hostPackages(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{}); err != nil {
		t.Fatal(err)
	}
	apt := aptCalls(runner)
	if len(apt) != 2 {
		t.Fatalf("apt calls = %v, want update then one install", apt)
	}
	if apt[0] != "apt-get update" {
		t.Errorf("first apt call %q, want the apt-get update", apt[0])
	}
	want := "apt-get install -y --no-install-recommends rsync e2fsprogs iproute2 util-linux curl ca-certificates docker.io docker-buildx containerd runc"
	if apt[1] != want {
		t.Errorf("install call:\n %s\nwant:\n %s", apt[1], want)
	}
}

// TestHostPackagesAllPresent: every package installed, so no apt call
// runs at all.
func TestHostPackagesAllPresent(t *testing.T) {
	env, _, runner := testEnv(t)
	runner.reply = map[string]fakeReply{}
	installed(runner, basePackages...)

	if err := hostPackages(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range runner.calls {
		if strings.HasPrefix(c, "apt-get") {
			t.Errorf("ran %q with every package present, want no apt call", c)
		}
	}
}

// TestHostPackagesStoreDiskZfs: with --store-disk the ZFS set joins
// the list.
func TestHostPackagesStoreDiskZfs(t *testing.T) {
	env, _, runner := testEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	runner.reply = map[string]fakeReply{}
	installed(runner, basePackages...)
	addReply(runner, "apt-cache policy zfs-dkms", "zfs-dkms:\n  Installed: (none)\n  Candidate: 2.2.6-1\n")
	addReply(runner, "apt-get update", "")
	addReply(runner, "apt-get install -y --no-install-recommends linux-headers-amd64 zfs-dkms zfsutils-linux zfs-zed", "")

	if err := hostPackages(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, want := range []string{"apt-get update", "linux-headers-amd64 zfs-dkms zfsutils-linux zfs-zed", "apt-cache policy zfs-dkms"} {
		if !strings.Contains(joined, want) {
			t.Errorf("calls lack %q:\n%s", want, joined)
		}
	}
}

// TestHostPackagesMissingContrib: zfs-dkms has no candidate (contrib
// not enabled) and the error says so.
func TestHostPackagesMissingContrib(t *testing.T) {
	env, _, runner := testEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	runner.reply = map[string]fakeReply{}
	installed(runner, basePackages...)
	addReply(runner, "apt-get update", "")
	addReply(runner, "apt-cache policy zfs-dkms", "zfs-dkms:\n  Installed: (none)\n  Candidate: (none)\n")

	err := hostPackages(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
	if err == nil {
		t.Fatal("want the contrib failure")
	}
	for _, want := range []string{"zfs-dkms", "contrib", "/etc/apt/sources.list.d/debian.sources"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, c := range runner.calls {
		if strings.HasPrefix(c, "apt-get install") {
			t.Errorf("ran %q despite the missing candidate", c)
		}
	}
}

// aptCalls returns the runner's apt-get calls in order.
func aptCalls(runner *recordRunner) []string {
	var out []string
	for _, c := range runner.calls {
		if strings.HasPrefix(c, "apt-get") {
			out = append(out, c)
		}
	}
	return out
}

// writeRootFile writes content to a path under the env root, creating
// the directories; the fakes for /proc, /sys and /etc files go through
// it.
func writeRootFile(t *testing.T, env *Env, path, content string) {
	t.Helper()
	full := env.Resolve(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// kernelSim stands in for the kernel and the alternatives database:
// every command goes through the inner recordRunner (whose reply table
// still answers), a successful modprobe leaves the module markers the
// step reads back, and a successful update-alternatives --set flips
// the --query answer to the legacy path.
type kernelSim struct {
	t     *testing.T
	env   *Env
	inner *recordRunner
}

func (k *kernelSim) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := k.inner.Run(ctx, name, args...)
	if err != nil {
		return out, err
	}
	switch {
	case name == "modprobe" && len(args) > 0 && args[0] == "nbd":
		writeRootFile(k.t, k.env, nbdParamPath, "256\n")
	case name == "modprobe" && len(args) > 0 && args[0] == "tun":
		writeRootFile(k.t, k.env, tunModulePath, "")
	case name == "update-alternatives" && len(args) == 3 && args[0] == "--set":
		legacy := legacyPath(args[1])
		k.inner.reply["update-alternatives --query "+args[1]] =
			fakeReply{out: []byte("Value: " + legacy + "\n")}
	}
	return out, nil
}

// modprobeReplies teaches runner the two modprobes and seeds the
// loaded-module fakes. loaded=false leaves /sys without the module
// dirs, so the step decides a modprobe is needed.
func modprobeReplies(t *testing.T, env *Env, runner *recordRunner, loaded bool, nbdsMax string) {
	t.Helper()
	runner.reply = map[string]fakeReply{}
	addReply(runner, "modprobe nbd nbds_max=256 max_part=16", "")
	addReply(runner, "modprobe tun", "")
	if loaded {
		writeRootFile(t, env, nbdParamPath, nbdsMax+"\n")
		writeRootFile(t, env, tunModulePath, "")
	}
}

// TestHostModulesWritesConfigsAndLoads: both configs land with the
// wanted content, the missing modules load, and the running nbds_max
// is reported.
func TestHostModulesWritesConfigsAndLoads(t *testing.T) {
	env, _, runner := testEnv(t)
	modprobeReplies(t, env, runner, false, "")
	env.Run = &kernelSim{t: t, env: env, inner: runner}

	r := &Reporter{Phase: PhaseHost}
	if err := hostModules(context.Background(), env, r, &hostState{}); err != nil {
		t.Fatal(err)
	}
	if b, mode := read(t, env, "/etc/modules-load.d/e2b.conf"); string(b) != "nbd\ntun\nkvm\n" || mode != 0o644 {
		t.Errorf("e2b.conf = %q, mode %#o; want the module list at 0644", b, mode)
	}
	if b, _ := read(t, env, "/etc/modprobe.d/e2b-nbd.conf"); string(b) != "options nbd nbds_max=256 max_part=16\n" {
		t.Errorf("e2b-nbd.conf = %q, want the nbd option line", b)
	}
	want := []string{"modprobe nbd nbds_max=256 max_part=16", "modprobe tun"}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", runner.calls, want)
	}
	for i := range want {
		if runner.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, runner.calls[i], want[i])
		}
	}
	var kept bool
	for _, it := range r.Items() {
		if it.Name == "nbd" && it.Result == Kept {
			kept = true
		}
	}
	if !kept {
		t.Errorf("rows = %+v, want a kept row for the loaded nbd", r.Items())
	}
}

// TestHostModulesLowNbdsMaxFails: nbd already loaded with 16 devices;
// the error carries the rmmod/reboot fix and no modprobe runs.
func TestHostModulesLowNbdsMaxFails(t *testing.T) {
	env, _, runner := testEnv(t)
	modprobeReplies(t, env, runner, true, "16")

	err := hostModules(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
	if err == nil {
		t.Fatal("want the nbds_max failure")
	}
	for _, want := range []string{"nbds_max=16", "rmmod nbd", "reboot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, c := range runner.calls {
		if strings.HasPrefix(c, "modprobe") {
			t.Errorf("ran %q despite the loaded module", c)
		}
	}
}

// TestHostModulesSecondRunKeeps: with the configs written and the
// modules loaded in the wanted state, the rerun reports kept only and
// runs no changing command.
func TestHostModulesSecondRunKeeps(t *testing.T) {
	env, _, runner := testEnv(t)
	modprobeReplies(t, env, runner, false, "")
	env.Run = &kernelSim{t: t, env: env, inner: runner}

	st := &hostState{}
	if err := hostModules(context.Background(), env, &Reporter{Phase: PhaseHost}, st); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil

	r := &Reporter{Phase: PhaseHost}
	if err := hostModules(context.Background(), env, r, st); err != nil {
		t.Fatal(err)
	}
	for _, it := range r.Items() {
		if it.Result != Kept {
			t.Errorf("rerun row %+v, want kept only", it)
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("rerun calls = %v, want none", runner.calls)
	}
}

// TestHostModulesDryRunRunsNoModprobe: the configs are planned, the
// modprobes are changing commands and do not run in a dry run; the
// row for the loading is skipped, not shown as done.
func TestHostModulesDryRunRunsNoModprobe(t *testing.T) {
	env, out, runner := testEnv(t)
	env.DryRun = true
	modprobeReplies(t, env, runner, false, "")

	r := &Reporter{Phase: PhaseHost, Out: out}
	if err := hostModules(context.Background(), env, r, &hostState{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("dry run calls = %v, want none", runner.calls)
	}
	mustNotExist(t, env, "/etc/modules-load.d/e2b.conf")
	mustNotExist(t, env, "/etc/modprobe.d/e2b-nbd.conf")
}
func TestHostUdevWritesRule(t *testing.T) {
	env, _, _ := testEnv(t)
	r := &Reporter{Phase: PhaseHost}
	if err := hostUdev(context.Background(), env, r, &hostState{}); err != nil {
		t.Fatal(err)
	}
	want := "KERNEL==\"nbd*\", GROUP=\"disk\", MODE=\"0660\"\n"
	if b, mode := read(t, env, "/etc/udev/rules.d/97-nbd-device.rules"); string(b) != want || mode != 0o644 {
		t.Errorf("rule = %q, mode %#o; want %q at 0644", b, mode, want)
	}
	if len(r.Items()) != 1 || r.Items()[0].Result != Created {
		t.Errorf("rows = %+v, want one created row", r.Items())
	}

	r = &Reporter{Phase: PhaseHost}
	if err := hostUdev(context.Background(), env, r, &hostState{}); err != nil {
		t.Fatal(err)
	}
	if len(r.Items()) != 1 || r.Items()[0].Result != Kept {
		t.Errorf("rerun rows = %+v, want one kept row", r.Items())
	}
}

// TestHostPackagesContribCheckedBeforeInstall pins the order: the
// candidate check runs after the lists refresh and before the one
// install.
func TestHostPackagesContribCheckedBeforeInstall(t *testing.T) {
	env, _, runner := testEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	runner.reply = map[string]fakeReply{}
	installed(runner, basePackages...)
	addReply(runner, "apt-get update", "")
	addReply(runner, "apt-cache policy zfs-dkms", "zfs-dkms:\n  Installed: (none)\n  Candidate: 2.2.6-1\n")
	addReply(runner, "apt-get install -y --no-install-recommends linux-headers-amd64 zfs-dkms zfsutils-linux zfs-zed", "")

	if err := hostPackages(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{}); err != nil {
		t.Fatal(err)
	}
	apt := aptCalls(runner)
	if len(apt) != 2 || apt[0] != "apt-get update" {
		t.Fatalf("apt calls = %v, want the update then the install", apt)
	}
	if runner.calls[len(runner.calls)-2] != "apt-cache policy zfs-dkms" {
		t.Errorf("call before the install %q, want the candidate check", runner.calls[len(runner.calls)-2])
	}
	if runner.calls[len(runner.calls)-1] != "apt-get install -y --no-install-recommends linux-headers-amd64 zfs-dkms zfsutils-linux zfs-zed" {
		t.Errorf("last call %q, want the one install", runner.calls[len(runner.calls)-1])
	}
}

// meminfo builds a /proc/meminfo fake with the given totals (bytes)
// and hugepage counts (pages).
func meminfo(totalBytes int64, hugepagesTotal int) string {
	s := fmt.Sprintf("MemTotal:       %d kB\n", totalBytes>>10)
	if hugepagesTotal >= 0 {
		s += fmt.Sprintf("HugePages_Total:    %d\n", hugepagesTotal)
	}
	return s + "MemAvailable:   12345678 kB\n"
}

// grubBody is a /etc/default/grub fake with the common lines around
// the command line.
const grubBody = `# If you change this file, run 'update-grub' afterwards.
GRUB_DEFAULT=0
GRUB_TIMEOUT=5
GRUB_CMDLINE_LINUX_DEFAULT="quiet splash"
GRUB_CMDLINE_LINUX=""
`

// TestHugepagesForFormula pins the auto formula: the host keeps
// max(6 GiB, 17%) and the rest splits into 2 MiB pages, rounded down.
func TestHugepagesForFormula(t *testing.T) {
	// 16 GiB: keep 6 GiB, 10 GiB in pages = 5120.
	pages, err := hugepagesFor(16 << 30)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 5120 {
		t.Errorf("16 GiB -> %d pages, want 5120", pages)
	}
	// 128 GiB: keep 17% (21.76 GiB), the rest in pages, rounded down.
	want := int((int64(128)<<30)*83/100) / (2 << 20)
	pages, err = hugepagesFor(128 << 30)
	if err != nil {
		t.Fatal(err)
	}
	if pages != want {
		t.Errorf("128 GiB -> %d pages, want the formula's %d", pages, want)
	}
	// 8 GiB: keep 6 GiB leaves 2 GiB, below the 6 GiB lease floor.
	_, err = hugepagesFor(8 << 30)
	if err == nil {
		t.Fatal("8 GiB should fail")
	}
	if !strings.Contains(err.Error(), "not enough memory for leases: 2048 MiB") {
		t.Errorf("8 GiB error = %q, want the memory note", err)
	}
}

// TestHostHugepagesExplicitCount: --hugepages 4096 is used as given,
// no MemTotal read involved (there is no meminfo fake here).
func TestHostHugepagesExplicitCount(t *testing.T) {
	env, _, runner := testEnv(t)
	env.Opt.Hugepages = "4096"
	runner.reply = map[string]fakeReply{}
	addReply(runner, "sysctl -p /etc/sysctl.d/90-e2b.conf", "")
	addReply(runner, "update-grub", "")
	writeRootFile(t, env, grubPath, grubBody)
	writeRootFile(t, env, meminfoPath, meminfo(16<<30, 0))

	st := &hostState{}
	if err := hostHugepages(context.Background(), env, &Reporter{Phase: PhaseHost}, st); err != nil {
		t.Fatal(err)
	}
	if st.Hugepages != 4096 {
		t.Errorf("st.Hugepages = %d, want 4096 as given", st.Hugepages)
	}
	b, _ := read(t, env, sysctlE2BPath)
	if !strings.Contains(string(b), "vm.nr_hugepages = 4096\n") {
		t.Errorf("sysctl conf = %q, want the 4096 reservation", b)
	}
}

// TestHostHugepagesAutoAndGrub walks the auto path over a real grub
// body: 16 GiB yields 5120; the existing hugepages= token is replaced
// and the other tokens survive; update-grub runs only when the file
// changed.
func TestHostHugepagesAutoAndGrub(t *testing.T) {
	env, _, runner := testEnv(t)
	runner.reply = map[string]fakeReply{}
	addReply(runner, "sysctl -p /etc/sysctl.d/90-e2b.conf", "")
	addReply(runner, "update-grub", "")
	writeRootFile(t, env, meminfoPath, meminfo(16<<30, 0))
	// An existing token and other tokens that must survive.
	writeRootFile(t, env, grubPath, strings.Replace(grubBody,
		`GRUB_CMDLINE_LINUX_DEFAULT="quiet splash"`,
		`GRUB_CMDLINE_LINUX_DEFAULT="quiet hugepages=12288"`, 1))

	st := &hostState{}
	if err := hostHugepages(context.Background(), env, &Reporter{Phase: PhaseHost}, st); err != nil {
		t.Fatal(err)
	}
	if st.Hugepages != 5120 {
		t.Errorf("st.Hugepages = %d, want 5120 for 16 GiB", st.Hugepages)
	}
	b, _ := read(t, env, grubPath)
	if !strings.Contains(string(b), `GRUB_CMDLINE_LINUX_DEFAULT="quiet hugepages=5120"`) {
		t.Errorf("grub = %q, want the token replaced inside the line", b)
	}
	if strings.Contains(string(b), "12288") {
		t.Errorf("grub = %q, the old token is still there", b)
	}
	// 5120 pages = 10 GiB; the fake kernel reserved none, so the
	// phase asks for the reboot but does not fail.
	if !st.RebootNeeded {
		t.Error("HugePages_Total 0 below 5120: st.RebootNeeded not set")
	}

	// The reservation file, the same run.
	sysctl, _ := read(t, env, sysctlE2BPath)
	want := "vm.nr_hugepages = 5120\nvm.max_map_count = 1048576\nnet.ipv4.ip_forward = 1\n"
	if string(sysctl) != want {
		t.Errorf("sysctl conf = %q, want %q", sysctl, want)
	}
	for _, c := range runner.calls {
		if c == "update-grub" {
			return // ran, the grub file changed: right
		}
	}
	t.Errorf("calls = %v, want update-grub after the grub change", runner.calls)
}

// TestHostHugepagesGrubKeepsTokensAndSkipsUpdateGrub: a second run
// changes nothing, so update-grub does not run again; the non-grub
// lines stay byte for byte.
func TestHostHugepagesGrubKeepsTokensAndSkipsUpdateGrub(t *testing.T) {
	env, _, runner := testEnv(t)
	runner.reply = map[string]fakeReply{}
	addReply(runner, "sysctl -p /etc/sysctl.d/90-e2b.conf", "")
	addReply(runner, "update-grub", "")
	// The kernel has the pages now: no reboot.
	writeRootFile(t, env, meminfoPath, meminfo(16<<30, 5120))
	writeRootFile(t, env, grubPath, strings.Replace(grubBody,
		`GRUB_CMDLINE_LINUX_DEFAULT="quiet splash"`,
		`GRUB_CMDLINE_LINUX_DEFAULT="quiet hugepages=5120"`, 1))
	writeRootFile(t, env, sysctlE2BPath,
		"vm.nr_hugepages = 5120\nvm.max_map_count = 1048576\nnet.ipv4.ip_forward = 1\n")

	st := &hostState{}
	if err := hostHugepages(context.Background(), env, &Reporter{Phase: PhaseHost}, st); err != nil {
		t.Fatal(err)
	}
	if st.RebootNeeded {
		t.Error("HugePages_Total 5120 covers the want: no reboot")
	}
	for _, c := range runner.calls {
		if c == "update-grub" || c == "sysctl -p /etc/sysctl.d/90-e2b.conf" {
			t.Errorf("second run ran %q with nothing to change", c)
		}
	}
	b, _ := read(t, env, grubPath)
	if !strings.Contains(string(b), "GRUB_DEFAULT=0") || !strings.Contains(string(b), `GRUB_CMDLINE_LINUX_DEFAULT="quiet hugepages=5120"`) {
		t.Errorf("grub = %q, want the untouched body with the token kept", b)
	}
}

// TestHostHugepagesShortMemoryFails: 8 GiB auto is below the lease
// floor; the error carries the fix.
func TestHostHugepagesShortMemoryFails(t *testing.T) {
	env, _, _ := testEnv(t)
	writeRootFile(t, env, meminfoPath, meminfo(8<<30, 0))
	err := hostHugepages(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
	if err == nil {
		t.Fatal("8 GiB should fail")
	}
	if !strings.Contains(err.Error(), "not enough memory for leases: 2048 MiB") {
		t.Errorf("error = %q, want the memory note", err)
	}
}

// TestSetGrubHugepagesTable covers the token surgery directly: append
// when absent, replace when present, keep everything else, and report
// changed honestly.
func TestSetGrubHugepagesTable(t *testing.T) {
	cases := []struct {
		name    string
		in, out string
		changed bool
	}{
		{
			name:    "append",
			in:      `GRUB_CMDLINE_LINUX_DEFAULT="quiet splash"` + "\n",
			out:     `GRUB_CMDLINE_LINUX_DEFAULT="quiet splash hugepages=5120"` + "\n",
			changed: true,
		},
		{
			name:    "replace",
			in:      `GRUB_CMDLINE_LINUX_DEFAULT="quiet hugepages=12288"` + "\n",
			out:     `GRUB_CMDLINE_LINUX_DEFAULT="quiet hugepages=5120"` + "\n",
			changed: true,
		},
		{
			name:    "same",
			in:      `GRUB_CMDLINE_LINUX_DEFAULT="hugepages=5120"` + "\n",
			out:     `GRUB_CMDLINE_LINUX_DEFAULT="hugepages=5120"` + "\n",
			changed: false,
		},
		{
			name:    "other lines untouched",
			in:      "GRUB_TIMEOUT=5\nGRUB_CMDLINE_LINUX_DEFAULT=\"quiet hugepages=9\"\nGRUB_CMDLINE_LINUX=\"\"",
			out:     "GRUB_TIMEOUT=5\nGRUB_CMDLINE_LINUX_DEFAULT=\"quiet hugepages=5120\"\nGRUB_CMDLINE_LINUX=\"\"",
			changed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := setGrubHugepages(tc.in, 5120)
			if out != tc.out || changed != tc.changed {
				t.Errorf("setGrubHugepages(%q) = %q, %v; want %q, %v", tc.in, out, changed, tc.out, tc.changed)
			}
		})
	}
}

// dpkgQueries lists the package check commands: read-only, so a
// settled second run may make them.
func dpkgQueries() []string {
	var out []string
	for _, p := range basePackages {
		out = append(out, "dpkg-query -W -f=${Status} "+p)
	}
	return out
}

// altQuery builds `update-alternatives --query` output for a Value.
func altQuery(value string) string { return "Alternative: " + value + "\nValue: " + value + "\n" }

// TestHostIptablesSwitchesToLegacy: both alternatives are on nft; the
// step sets both and says what they were.
func TestHostIptablesSwitchesToLegacy(t *testing.T) {
	env, _, runner := testEnv(t)
	runner.reply = map[string]fakeReply{}
	addReply(runner, "update-alternatives --query iptables", altQuery("/usr/sbin/iptables-nft"))
	addReply(runner, "update-alternatives --query ip6tables", altQuery("/usr/sbin/ip6tables-nft"))
	addReply(runner, "update-alternatives --set iptables /usr/sbin/iptables-legacy", "")
	addReply(runner, "update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy", "")

	r := &Reporter{Phase: PhaseHost}
	if err := hostIptables(context.Background(), env, r, &hostState{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"update-alternatives --query iptables",
		"update-alternatives --set iptables /usr/sbin/iptables-legacy",
		"update-alternatives --query ip6tables",
		"update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy",
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", runner.calls, want)
	}
	for i := range want {
		if runner.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, runner.calls[i], want[i])
		}
	}
	if got := len(r.Items()); got != 2 {
		t.Fatalf("rows = %+v, want one per alternative", r.Items())
	}
	for _, it := range r.Items() {
		if it.Result != Changed {
			t.Errorf("row %+v, want changed", it)
		}
	}
}

// TestHostIptablesAlreadyLegacy: both on legacy, so only the two
// queries run and the rows are kept.
func TestHostIptablesAlreadyLegacy(t *testing.T) {
	env, _, runner := testEnv(t)
	runner.reply = map[string]fakeReply{}
	addReply(runner, "update-alternatives --query iptables", altQuery("/usr/sbin/iptables-legacy"))
	addReply(runner, "update-alternatives --query ip6tables", altQuery("/usr/sbin/ip6tables-legacy"))

	r := &Reporter{Phase: PhaseHost}
	if err := hostIptables(context.Background(), env, r, &hostState{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Errorf("calls = %v, want only the two queries", runner.calls)
	}
	for _, it := range r.Items() {
		if it.Result != Kept {
			t.Errorf("row %+v, want kept", it)
		}
	}
}

// TestHostIptablesQueryFails: without the alternatives database the
// error names the package that provides it.
func TestHostIptablesQueryFails(t *testing.T) {
	env, _, runner := testEnv(t)
	runner.reply = map[string]fakeReply{} // every command errors
	err := hostIptables(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
	if err == nil {
		t.Fatal("want the query failure")
	}
	if !strings.Contains(err.Error(), "iptables package") {
		t.Errorf("error = %q, want the package hint", err)
	}
}

// TestHostSystemStepsSecondRunQuiet runs every system step twice over
// the same root: the second pass reports kept only and runs no
// changing command (queries and modprobe-free reads aside).
func TestHostSystemStepsSecondRunQuiet(t *testing.T) {
	type stepCase struct {
		name string
		run  func(t *testing.T, env *Env, runner *recordRunner) error
		// noChangeCalls counts the runner calls the settled second run
		// may still make (read-only ones).
		allowed []string
	}
	cases := []stepCase{
		{
			name: "packages",
			run: func(t *testing.T, env *Env, runner *recordRunner) error {
				for _, p := range basePackages {
					addReply(runner, "dpkg-query -W -f=${Status} "+p, "install ok installed")
				}
				return hostPackages(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
			},
			allowed: dpkgQueries(),
		},
		{
			name: "modules",
			run: func(t *testing.T, env *Env, runner *recordRunner) error {
				return hostModules(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
			},
			allowed: []string{},
		},
		{
			name: "udev",
			run: func(t *testing.T, env *Env, runner *recordRunner) error {
				return hostUdev(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
			},
			allowed: []string{},
		},
		{
			name: "hugepages",
			run: func(t *testing.T, env *Env, runner *recordRunner) error {
				return hostHugepages(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
			},
			allowed: []string{},
		},
		{
			name: "iptables",
			run: func(t *testing.T, env *Env, runner *recordRunner) error {
				return hostIptables(context.Background(), env, &Reporter{Phase: PhaseHost}, &hostState{})
			},
			allowed: []string{
				"update-alternatives --query iptables",
				"update-alternatives --query ip6tables",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _, runner := testEnv(t)
			runner.reply = map[string]fakeReply{}
			// A host the first run settles: packages installed,
			// modules absent then loaded by the sim, hugepages
			// reserved by the "kernel" at 16 GiB/5120, legacy
			// iptables.
			for _, p := range append([]string{}, basePackages...) {
				addReply(runner, "dpkg-query -W -f=${Status} "+p, "install ok installed")
			}
			addReply(runner, "sysctl -p /etc/sysctl.d/90-e2b.conf", "")
			addReply(runner, "update-grub", "")
			addReply(runner, "update-alternatives --query iptables", altQuery("/usr/sbin/iptables-nft"))
			addReply(runner, "update-alternatives --query ip6tables", altQuery("/usr/sbin/ip6tables-nft"))
			addReply(runner, "update-alternatives --set iptables /usr/sbin/iptables-legacy", "")
			addReply(runner, "update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy", "")
			// modprobe runs through the sim, but the recordRunner it
			// wraps still needs the reply entries.
			addReply(runner, "modprobe nbd nbds_max=256 max_part=16", "")
			addReply(runner, "modprobe tun", "")
			writeRootFile(t, env, meminfoPath, meminfo(16<<30, 0))
			writeRootFile(t, env, grubPath, grubBody)
			kr := &kernelSim{t: t, env: env, inner: runner}
			env.Run = kr

			// The first pass runs the step over the fresh fake host;
			// settle then fakes what the real host would keep.
			if err := tc.run(t, env, runner); err != nil {
				t.Fatalf("first run: %v", err)
			}
			settle(t, env, tc.name, runner)
			runner.calls = nil
			if err := tc.run(t, env, runner); err != nil {
				t.Fatalf("second run: %v", err)
			}
			for _, c := range runner.calls {
				allowed := false
				for _, a := range tc.allowed {
					if c == a {
						allowed = true
					}
				}
				if !allowed {
					t.Errorf("second run ran changing command %q", c)
				}
			}
		})
	}
}

// settle fakes the host state the step's first run left behind, where
// the file fakes cannot follow the commands: the hugepages "kernel"
// reserves what the sysctl file asked for.
func settle(t *testing.T, env *Env, step string, runner *recordRunner) {
	t.Helper()
	if step != "hugepages" {
		return
	}
	b, err := os.ReadFile(env.Resolve(sysctlE2BPath))
	if err != nil {
		t.Fatal(err)
	}
	const tag = "vm.nr_hugepages = "
	pages := 0
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, tag); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatal(err)
			}
			pages = n
		}
	}
	writeRootFile(t, env, meminfoPath, meminfo(16<<30, pages))
}
