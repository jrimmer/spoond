package install

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// withHostSteps swaps the step table for a test and restores it after.
func withHostSteps(t *testing.T, steps map[string]hostStep) {
	t.Helper()
	saved := hostSteps
	hostSteps = steps
	t.Cleanup(func() { hostSteps = saved })
}

func TestHostPhaseRunsStepsInOrder(t *testing.T) {
	var ran []string
	steps := map[string]hostStep{}
	for _, name := range hostOrder {
		name := name
		steps[name] = func(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
			ran = append(ran, name)
			return nil
		}
	}
	withHostSteps(t, steps)
	env, _, _ := testEnv(t)
	if _, err := hostPhase(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(ran) != len(hostOrder) {
		t.Fatalf("ran %v, want %v", ran, hostOrder)
	}
	for i := range ran {
		if ran[i] != hostOrder[i] {
			t.Fatalf("ran %v, want %v", ran, hostOrder)
		}
	}
}

func TestHostPhaseRebootAfterAllSteps(t *testing.T) {
	var ran int
	steps := map[string]hostStep{}
	for _, name := range hostOrder {
		name := name
		steps[name] = func(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
			ran++
			if name == "hugepages" {
				st.Hugepages = 5120
				st.RebootNeeded = true
			}
			return nil
		}
	}
	withHostSteps(t, steps)
	env, _, _ := testEnv(t)
	_, err := hostPhase(context.Background(), env)
	if !errors.Is(err, ErrRebootNeeded) {
		t.Fatalf("err %v, want ErrRebootNeeded", err)
	}
	if ran != len(hostOrder) {
		t.Fatalf("%d steps ran before the reboot error, want all %d", ran, len(hostOrder))
	}
}

func TestHostPhaseMissingStep(t *testing.T) {
	withHostSteps(t, map[string]hostStep{})
	env, _, _ := testEnv(t)
	if _, err := hostPhase(context.Background(), env); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("err %v, want ErrNotImplemented", err)
	}
}

// TestHostPhaseContainerRunsThePrepare walks the whole host phase with
// --container, as the compose prepare service runs it: modules, udev,
// hugepages and the store dir check do their work under Root, the
// packages, iptables, docker and apparmor steps are skipped rows, the
// grub change asks for the host-side update-grub and the reboot, and
// no apt-get, update-alternatives, update-grub, zpool, docker or
// systemctl command runs.
func TestHostPhaseContainerRunsThePrepare(t *testing.T) {
	env, _, runner := testEnv(t)
	env.Opt.Container = true
	runner.reply = map[string]fakeReply{}
	addReply(runner, "modprobe nbd nbds_max=256 max_part=16", "")
	addReply(runner, "modprobe tun", "")
	addReply(runner, "sysctl -p "+env.Resolve(sysctlE2BPath), "")
	for k, v := range storeReplies(false, false, "xfs") {
		runner.reply[k] = v
	}
	if err := os.MkdirAll(env.Resolve(DefaultStoreDir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRootFile(t, env, meminfoPath, meminfo(16<<30, 0))
	writeRootFile(t, env, grubPath, grubBody)
	env.Run = &kernelSim{t: t, env: env, inner: runner}

	items, err := hostPhase(context.Background(), env)
	if !errors.Is(err, ErrRebootNeeded) {
		t.Fatalf("err %v, want ErrRebootNeeded (the grub change defers to the host)", err)
	}

	// Only the shared-kernel modprobes, the sysctl through the
	// resolved path and the store's read-only findmnt ran.
	allowed := map[string]bool{
		"modprobe nbd nbds_max=256 max_part=16":         true,
		"modprobe tun":                                  true,
		"sysctl -p " + env.Resolve(sysctlE2BPath):       true,
		"findmnt -n -o FSTYPE --target /forkdcache/e2b": true,
	}
	for _, c := range runner.calls {
		if !allowed[c] {
			t.Errorf("--container ran %q, want only modprobe, the resolved sysctl -p and findmnt", c)
		}
		for _, banned := range []string{"apt-get", "update-alternatives", "update-grub", "zpool", "docker", "systemctl"} {
			if strings.HasPrefix(c, banned+" ") {
				t.Errorf("--container ran the %s command %q", banned, c)
			}
		}
	}

	// The host files landed under Root.
	for _, p := range []string{
		"/etc/modules-load.d/e2b.conf",
		"/etc/modprobe.d/e2b-nbd.conf",
		"/etc/udev/rules.d/97-nbd-device.rules",
		sysctlE2BPath,
	} {
		if _, err := os.Stat(env.Resolve(p)); err != nil {
			t.Errorf("%s did not land under Root: %v", p, err)
		}
	}
	b, _ := read(t, env, grubPath)
	if !strings.Contains(string(b), "hugepages=5120") {
		t.Errorf("grub under Root = %q, want the hugepages token", b)
	}

	// The report says what the operator does on the host.
	named := map[string]Item{}
	for _, it := range items {
		named[it.Name] = it
	}
	for _, name := range []string{"packages", "iptables", "docker", "apparmor", "update-grub"} {
		it, ok := named[name]
		if !ok {
			t.Errorf("no row named %s in:\n%+v", name, items)
			continue
		}
		if it.Result != Skipped {
			t.Errorf("row %s = %s, want skipped (%q)", name, it.Result, it.Note)
		}
	}
	if it := named["store filesystem"]; it.Result != Kept || !strings.Contains(it.Note, "xfs") {
		t.Errorf("store filesystem row = %+v, want kept on xfs", it)
	}
}
