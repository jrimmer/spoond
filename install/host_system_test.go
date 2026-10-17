// Tests for the host phase's system steps, one test per step, all
// against a temp root with the fake runner. The host files the steps
// read (/proc/meminfo, /etc/default/grub, /sys) are fakes under the
// root; changing commands go through recordRunner, so a test asserts
// on the exact command list.
package install

import (
	"context"
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
