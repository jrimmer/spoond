// Tests for the check phase (check.go): a temp Root with fake /proc,
// /etc and /dev files plus faked port and free-space probes, so the
// real checkPhase logic runs end to end. Each test pins one failure
// row, its fix text and the error return, and the passing host once
// over — including the non-fatal warnings (not Debian 13) and the
// store-does-not-exist-yet nearest-ancestor measurement.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setArch pins the Env's arch for one test call: fn sees arch and is
// restored (to the real GOARCH answer) when the test ends.
func setArch(t *testing.T, env *Env, arch string) {
	t.Helper()
	env.Arch = arch
	t.Cleanup(func() { env.Arch = "" })
}

// fakeProber answers the port probe from a table: "" means free, a
// string means the error that ListenOK reports (a busy error for a
// taken port, anything else is a probe failure).
type fakeProber struct {
	busy    map[string]bool
	fillAll bool // every probe is a "not free" verdict without an error
	err     error
}

func (f fakeProber) ListenOK(port string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.fillAll {
		return false, nil
	}
	return !f.busy[port], nil
}

// fakeSpacer answers the free-space probe with a fixed byte count or a
// fixed error.
type fakeSpacer struct {
	free uint64
	err  error
}

func (f fakeSpacer) FreeBytes(string) (uint64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.free, nil
}

// swapProbes fakes the probe fields of env for the test and clears
// them; the check phase reads its probes from env, so every fake is
// scoped to one environment and no package state changes hands.
func swapProbes(t *testing.T, env *Env, p Prober, s Spacer) {
	t.Helper()
	env.Prober, env.Spacer = p, s
	t.Cleanup(func() { env.Prober, env.Spacer = nil, nil })
}

// meminfoLine renders a /proc/meminfo MemTotal line for a size in GiB.
func meminfoLine(gib int) []byte {
	return []byte(fmt.Sprintf("MemTotal:       %d kB\n", gib<<20))
}

// writeCheckTree fills the fake host files check reads. kvm=false
// leaves /dev/kvm out; gib is total memory; osID/osVersion are written
// into /etc/os-release (empty skips the file, forcing its read
// failure); kernel is the /proc/sys/kernel/osrelease value.
func writeCheckTree(t *testing.T, env *Env, kvm bool, gib int, osID, osVersion, kernel string) {
	t.Helper()
	if err := os.MkdirAll(env.Resolve("/etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if osID != "" {
		content := fmt.Sprintf("ID=%s\nVERSION_ID=%s\nPRETTY_NAME=\"%s %s\"\n", osID, osVersion, osID, osVersion)
		if err := os.WriteFile(env.Resolve(releasePath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(env.Resolve(filepath.Dir(meminfoPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(meminfoPath), meminfoLine(gib), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(env.Resolve(filepath.Dir(unamePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(unamePath), []byte(kernel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if kvm {
		if err := os.MkdirAll(env.Resolve(filepath.Dir(kvmPath)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(env.Resolve(kvmPath), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// goodTree is the passing host: Debian 13, 128 GiB, kernel 6.1.x, KVM
// present, 200 GiB free on the store, all ports free.
func goodTree(t *testing.T, env *Env) {
	t.Helper()
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0-13-amd64")
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
}

// firstFailedRow returns the single Failed row of a failed run, after
// asserting there is exactly one (the check stops at the first
// failure).
func firstFailedRow(t *testing.T, items []Item) Item {
	t.Helper()
	var failed []Item
	for _, it := range items {
		if it.Result == Failed {
			failed = append(failed, it)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("want exactly one failed row, got %+v", failed)
	}
	return failed[0]
}

// runCheck runs the real checkPhase against env and hands back the
// rows, the joined output and the error.
func runCheck(t *testing.T, env *Env) ([]Item, string, error) {
	t.Helper()
	items, err := RunPhase(context.Background(), env, PhaseCheck, checkPhase)
	return items, env.Out.(*bytes.Buffer).String(), err
}

// TestCheckPassingHost walks the happy path once: every probe is kept,
// the phase returns no error, and the rows name all seven ports.
func TestCheckPassingHost(t *testing.T) {
	env, _, _ := testEnv(t)
	goodTree(t, env)
	items, text, err := runCheck(t, env)
	if err != nil {
		t.Fatalf("check on a good host: %v\n%s", err, text)
	}
	var names []string
	for _, it := range items {
		if it.Result != Kept && it.Result != Skipped {
			t.Errorf("row %+v: want kept (or skipped for a warning) on a good host", it)
		}
		names = append(names, it.Name)
	}
	joined := strings.Join(names, "\n")
	for _, want := range []string{"root", "arch: x86-64", "/dev/kvm", "memory", "os", "kernel", "store free space"} {
		if !strings.Contains(joined, want) {
			t.Errorf("report does not cover %q:\n%s", want, joined)
		}
	}
	for _, port := range requiredPorts {
		if !strings.Contains(joined, ":"+port) {
			t.Errorf("report does not probe port %s:\n%s", port, joined)
		}
	}
	if !strings.Contains(text, "200 GiB free") {
		t.Errorf("store row should say the free space:\n%s", text)
	}
	if !strings.Contains(text, "Debian 13") {
		t.Errorf("os row should say Debian 13:\n%s", text)
	}
}

// TestCheckRootRequiredOnRealHost: production Root (/) without root
// fails with ErrNotRoot.
func TestCheckRootRequiredOnRealHost(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the not-root check is skipped on this host")
	}
	env := &Env{Root: "/", Out: &bytes.Buffer{}}
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "root" {
		t.Errorf("failed row = %q, want root", row.Name)
	}
	if !errors.Is(err, ErrNotRoot) {
		t.Errorf("err = %v, want ErrNotRoot", err)
	}
	if !strings.Contains(err.Error(), ErrNotRoot.Error()) {
		t.Errorf("error %q should carry the run-as-root fix", err)
	}
}

// TestCheckRootSkippedUnderTempRoot: with a non-/ Root the check skips
// root and never refuses a non-root test run.
func TestCheckRootSkippedUnderTempRoot(t *testing.T) {
	env, out, _ := testEnv(t) // temp root
	goodTree(t, env)
	items, text, err := runCheck(t, env)
	if err != nil {
		t.Fatalf("err: %v\n%s", err, text)
	}
	for _, it := range items {
		if it.Name == "root" {
			if it.Result != Skipped {
				t.Errorf("root row under a temp root = %s, want skipped", it.Result)
			}
			if !strings.Contains(it.Note, "root not required") {
				t.Errorf("root note should say why: %q", it.Note)
			}
		}
	}
	if strings.Contains(out.String(), "must run as root") {
		t.Errorf("temp-root run must not complain about root:\n%s", text)
	}
}

// TestCheckArch: a non-amd64 build is refused with what to do.
func TestCheckArch(t *testing.T) {
	env, out, _ := testEnv(t)
	goodTree(t, env)
	defer setArch(t, env, "amd64")
	setArch(t, env, "arm64")
	items, text, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "arch" {
		t.Fatalf("failed row = %q, want arch\n%s", row.Name, text)
	}
	if !strings.Contains(row.Note, "amd64 only") {
		t.Errorf("fix text should name the amd64 substrate: %q", row.Note)
	}
	if err == nil || !strings.Contains(err.Error(), "amd64 only") {
		t.Errorf("err = %v, want the amd64 message", err)
	}
	if strings.Contains(out.String(), "kvm") {
		t.Errorf("the arch failure should stop before the KVM probe:\n%s", text)
	}
}

// TestCheckKVMMissing: the commonest fresh-VM failure; the fix names
// virtualisation in the host or hypervisor.
func TestCheckKVMMissing(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, false, 128, "debian", "13", "6.1.0-13-amd64")
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "kvm" {
		t.Fatalf("failed row = %q, want kvm", row.Name)
	}
	if !strings.Contains(row.Note, "/dev/kvm is missing") || !strings.Contains(row.Note, "BIOS/UEFI") {
		t.Errorf("fix text should tell the operator where to enable KVM: %q", row.Note)
	}
	if err == nil || !strings.Contains(err.Error(), "/dev/kvm is missing") {
		t.Errorf("err = %v, want the kvm message", err)
	}
}

// TestCheckKVMProbeError: a stat failure is a check failure, not a
// "missing" finding, and it names the error.
func TestCheckKVMProbeError(t *testing.T) {
	env, _, _ := testEnv(t)
	// /dev itself is a plain file, so stat Root/dev/kvm fails with
	// ENOTDIR — not ErrNotExist, the "probe failure" path, distinct
	// from a clean miss. /proc, /etc and /dev all sit under the same
	// (absent) parent after this, so the files check needs are
	// created before /dev is replaced.
	writeCheckTree(t, env, false, 128, "debian", "13", "6.1.0-13-amd64")
	devFile := filepath.Join(env.Root, "dev-file")
	if err := os.WriteFile(devFile, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(env.Resolve("/dev")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(devFile, env.Resolve("/dev")); err != nil {
		t.Fatal(err)
	}
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "kvm" {
		t.Fatalf("failed row = %q, want kvm", row.Name)
	}
	if !strings.Contains(row.Note, "check failed") {
		t.Errorf("a probe failure should say so: %q", row.Note)
	}
	if err == nil {
		t.Error("want an error")
	}
}

// TestCheckMemoryLow and TestCheckMemoryUnreadable.
func TestCheckMemoryLow(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 8, "debian", "13", "6.1.0-13-amd64")
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "memory" {
		t.Fatalf("failed row = %q, want memory", row.Name)
	}
	if !strings.Contains(row.Note, "8 GiB total, want >= 16") {
		t.Errorf("fix text should repeat the numbers: %q", row.Note)
	}
	if !strings.Contains(row.Note, "add memory") {
		t.Errorf("fix text should say what to do: %q", row.Note)
	}
	if err == nil || !strings.Contains(err.Error(), "want >= 16") {
		t.Errorf("err = %v, want the memory message", err)
	}
}

func TestCheckMemoryUnreadable(t *testing.T) {
	env, _, _ := testEnv(t)
	// No /proc/meminfo at all: the read fails, the row says so.
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0")
	if err := os.Remove(env.Resolve(meminfoPath)); err != nil {
		t.Fatal(err)
	}
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "memory" {
		t.Fatalf("failed row = %q, want memory", row.Name)
	}
	if !strings.Contains(row.Note, "check failed") || !strings.Contains(row.Note, meminfoPath) {
		t.Errorf("fix text should name the unreadable file: %q", row.Note)
	}
	if err == nil {
		t.Error("want an error")
	}
}

// TestCheckOSWarnsOnOtherDistribution: not Debian 13 is a warning, the
// row is skipped and the install continues.
func TestCheckOSWarnsOnOtherDistribution(t *testing.T) {
	env, out, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "ubuntu", "24.04", "6.8.0-40-generic")
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	if err != nil {
		t.Fatalf("a non-Debian host only warns; err = %v", err)
	}
	if !strings.Contains(out.String(), "not Debian 13") {
		t.Errorf("report should warn:\n%s", out.String())
	}
	found := false
	for _, it := range items {
		if it.Result == Skipped && strings.Contains(it.Name, "ubuntu") {
			found = true
		}
	}
	if !found {
		t.Errorf("rows should name the distribution found: %+v", items)
	}
}

// TestCheckOSMissingID: an os-release with no ID also warns.
func TestCheckOSMissingID(t *testing.T) {
	env, out, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "", "", "6.1.0")
	if err := os.WriteFile(env.Resolve(releasePath), []byte("PRETTY_NAME=\"A Distro\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	if err != nil {
		t.Fatalf("an unrecognised os-release only warns; err = %v", err)
	}
	found := false
	for _, it := range items {
		if it.Result == Skipped && it.Name == "os" {
			found = true
		}
	}
	if !found {
		t.Errorf("want a skipped os row: %+v", items)
	}
	if !strings.Contains(out.String(), "no ID in /etc/os-release") {
		t.Errorf("warning should say what is missing:\n%s", out.String())
	}
}

// TestCheckOSReadFails: an unreadable os-release is a failure.
func TestCheckOSReadFails(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0")
	if err := os.Remove(env.Resolve(releasePath)); err != nil {
		t.Fatal(err)
	}
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "os" {
		t.Fatalf("failed row = %q, want os", row.Name)
	}
	if !strings.Contains(row.Note, "check failed") || !strings.Contains(row.Note, releasePath) {
		t.Errorf("fix text should name the unreadable file: %q", row.Note)
	}
	if err == nil || !strings.Contains(err.Error(), "read /etc/os-release") {
		t.Errorf("err = %v, want the os-release read failure", err)
	}
}

// TestCheckKernelOld and TestCheckKernelUnreadable.
func TestCheckKernelOld(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "debian", "13", "5.10.0-26-amd64")
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "kernel" {
		t.Fatalf("failed row = %q, want kernel", row.Name)
	}
	if !strings.Contains(row.Note, "5.10, want >= 6.1") {
		t.Errorf("fix text should repeat the numbers: %q", row.Note)
	}
	if err == nil || !strings.Contains(err.Error(), "want >= 6.1") {
		t.Errorf("err = %v, want the kernel message", err)
	}
}

func TestCheckKernelUnreadable(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0")
	if err := os.Remove(env.Resolve(unamePath)); err != nil {
		t.Fatal(err)
	}
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "kernel" {
		t.Fatalf("failed row = %q, want kernel", row.Name)
	}
	if !strings.Contains(row.Note, "check failed") || !strings.Contains(row.Note, unamePath) {
		t.Errorf("fix text should name the unreadable file: %q", row.Note)
	}
	if err == nil {
		t.Error("want an error")
	}
}

// TestCheckKernelParsesPlainVersions: a bare major.minor and a
// trailing -build suffix both read; a value the parser cannot
// understand is a check failure with the file named.
func TestCheckKernelParsesPlainVersions(t *testing.T) {
	for _, tc := range []struct {
		kernel string
		wantOK bool
	}{
		{"6.1.0-13-amd64", true},
		{"6.12.11", true},
		{"6.2", true},
		{"6.1", true},
		{"4.19.0", false},
		{"not-a-version", false},
	} {
		t.Run(tc.kernel, func(t *testing.T) {
			env, _, _ := testEnv(t)
			writeCheckTree(t, env, true, 128, "debian", "13", tc.kernel)
			swapProbes(t, env, fakeProber{}, fakeSpacer{free: 200 << 30})
			items, _, err := runCheck(t, env)
			var kerr error
			for _, it := range items {
				if it.Name == "kernel" && it.Result == Failed {
					kerr = fmt.Errorf("%s", it.Note)
				}
			}
			if tc.wantOK {
				if kerr != nil {
					t.Errorf("kernel %q: unexpected failure %q", tc.kernel, kerr)
				}
				if err != nil && strings.Contains(err.Error(), "kernel") {
					t.Errorf("kernel %q: err = %v", tc.kernel, err)
				}
				return
			}
			row := firstFailedRow(t, items)
			if row.Name != "kernel" {
				t.Errorf("kernel %q: failed row = %q", tc.kernel, row.Name)
			}
			if !strings.Contains(row.Note, unamePath) && !strings.Contains(row.Note, "want") {
				t.Errorf("kernel %q: fix text should name the file or the floor: %q", tc.kernel, row.Note)
			}
		})
	}
}

// TestCheckPortBusy: the first busy port fails with the settings to
// move, and only that port is probed.
func TestCheckPortBusy(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0-13-amd64")
	swapProbes(t, env, fakeProber{busy: map[string]bool{"2222": true}}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "port :2222" {
		t.Fatalf("failed row = %q, want port :2222", row.Name)
	}
	for _, want := range []string{"in use", "BIND_ADDR", "GATEWAY_ADDR"} {
		if !strings.Contains(row.Note, want) {
			t.Errorf("fix text should say %q: %q", want, row.Note)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("err = %v, want the port message", err)
	}
}

// TestCheckPortProbeError: a probe error is distinct from a busy port
// and is reported as a check failure.
func TestCheckPortProbeError(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0-13-amd64")
	swapProbes(t, env, fakeProber{err: errors.New("no such network")}, fakeSpacer{free: 200 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "port :8890" {
		t.Fatalf("failed row = %q, want the first probed port", row.Name)
	}
	if !strings.Contains(row.Note, "check failed") || !strings.Contains(row.Note, "no such network") {
		t.Errorf("fix text should carry the probe error: %q", row.Note)
	}
	if err == nil {
		t.Error("want an error")
	}
}

// TestCheckStoreFreeLow: below 100 GiB the store row fails and names
// the flag to move it elsewhere.
func TestCheckStoreFreeLow(t *testing.T) {
	env, out, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0-13-amd64")
	swapProbes(t, env, fakeProber{}, fakeSpacer{free: 40 << 30})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "store free space" {
		t.Fatalf("failed row = %q, want store free space", row.Name)
	}
	if !strings.Contains(row.Note, "40 GiB free, want >= 100") {
		t.Errorf("fix text should repeat the numbers: %q", row.Note)
	}
	if !strings.Contains(row.Note, "--store-disk") {
		t.Errorf("fix text should name the flag: %q", row.Note)
	}
	if err == nil || !strings.Contains(err.Error(), "want >= 100") {
		t.Errorf("err = %v, want the store message", err)
	}
	if !strings.Contains(out.String(), env.Resolve(defaultStore)) {
		t.Fatalf("the row should name the store path under this Root:\n%s", out.String())
	}
}

// TestCheckStoreMissingMeasuresAncestor: before the host phase creates
// the store — on a fresh host its parents do not exist either — the
// check measures the nearest existing ancestor instead and the row
// still reports against the store's path. The test pins the fallback's
// shape (the ancestor actually asked, the store's path in the row),
// never a real statfs number.
func TestCheckStoreMissingMeasuresAncestor(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0-13-amd64")
	// The store and every parent under Root are absent: nothing is
	// mkdir'd here. asked records the ancestor the fallback lands
	// on — the walk must stop at Root itself (the temp dir, the
	// first existing directory), not go past it.
	root := env.Resolve("/")
	store := env.Resolve(defaultStore)
	var asked []string
	spacer := recordingSpacer{root: root, dirs: &asked, rest: fakeSpacer{free: 1 << 30}}
	swapProbes(t, env, fakeProber{}, spacer)
	// The spacer answers not-exist for everything under Root, so the
	// walk lands on Root and measures the faked 1 GiB — far below the
	// bar, so the check fails with the space row's fix.
	items, _, err := runCheck(t, env)
	if err == nil || !strings.Contains(err.Error(), "1 GiB free, want >= 100") {
		t.Fatalf("the faked ancestor measurement should fail the space bar: %v", err)
	}
	row := firstFailedRow(t, items)
	if row.Name != "store free space" {
		t.Errorf("failed row = %q, want store free space", row.Name)
	}
	if !strings.Contains(row.Note, "1 GiB free, want >= 100") {
		t.Errorf("the row should report the faked ancestor measurement:\n%s", row.Note)
	}
	if !strings.Contains(row.Note, "--store-disk") {
		t.Errorf("the row should name the flag to move the store:\n%s", row.Note)
	}
	if !strings.Contains(row.Note, store) {
		t.Errorf("the row should still name the store path:\n%s", row.Note)
	}
	// The spacer saw exactly the walk: the store (absent) and its
	// parents down to Root... and nothing outside Root — the fakes
	// stop long before the real / is asked.
	if len(asked) == 0 || asked[len(asked)-1] != root {
		t.Errorf("the walk should end on Root with real dirs absent: asked %v", asked)
	}
	for _, d := range asked {
		if !strings.HasPrefix(d, root) {
			t.Errorf("the walk escaped Root: asked %s", d)
		}
	}
}

// recordingSpacer answers not-exist for every dir under want and the
// fixed answer otherwise, recording each dir it was asked about.
type recordingSpacer struct {
	root string // dirs under this prefix answer not-exist
	dirs *[]string
	rest fakeSpacer
}

func (m recordingSpacer) FreeBytes(dir string) (uint64, error) {
	*m.dirs = append(*m.dirs, dir)
	if strings.HasPrefix(dir, m.root) && dir != m.root {
		return 0, fmt.Errorf("statfs %s: %w", dir, fs.ErrNotExist)
	}
	return m.rest.FreeBytes(dir)
}

// TestCheckStoreUnprobeable: a spacer error other than not-exist on
// the store is a check failure that names the statfs call; the
// parent probe does not rescue an unexpected error there.
func TestCheckStoreUnprobeable(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 128, "debian", "13", "6.1.0-13-amd64")
	swapProbes(t, env, fakeProber{}, fakeSpacer{err: errors.New("permission denied")})
	items, _, err := runCheck(t, env)
	row := firstFailedRow(t, items)
	if row.Name != "store free space" {
		t.Fatalf("failed row = %q, want store free space", row.Name)
	}
	if !strings.Contains(row.Note, "check failed") || !strings.Contains(row.Note, "permission denied") {
		t.Errorf("fix text should carry the error: %q", row.Note)
	}
	if err == nil {
		t.Error("want an error")
	}
}

// TestCheckStopsAtFirstFailure: the rows before the failure are the
// only report; nothing after the broken probe ran (ports are after
// memory on a host with too little memory).
func TestCheckStopsAtFirstFailure(t *testing.T) {
	env, _, _ := testEnv(t)
	writeCheckTree(t, env, true, 4, "debian", "13", "5.10.0") // memory fails
	swapProbes(t, env, fakeProber{fillAll: true}, fakeSpacer{free: 1})
	items, _, _ := runCheck(t, env)
	for _, it := range items {
		if it.Name == "kernel" {
			t.Errorf("kernel ran after the memory failure already stopped the phase: %+v", items)
		}
	}
	firstFailedRow(t, items) // exactly one failed row, the memory one
}
