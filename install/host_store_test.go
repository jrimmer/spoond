// Tests for the host phase's store step (host_store.go): the
// --store-disk pool path and the plain-directory path, through the
// fake runner, against a temp root.
package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storeEnv returns an env for store-step tests: a fresh temp root, a
// recording runner with the store queries preloaded, and the store dir
// left at the production-style default so every path goes through
// env.Resolve like it does with Root="/".
func storeEnv(t *testing.T) (*Env, *recordRunner) {
	t.Helper()
	env, _, runner := testEnv(t)
	setZfsBclone(t, env, "1")
	return env, runner
}

// setZfsBclone writes the fake sysfs parameter under the root.
func setZfsBclone(t *testing.T, env *Env, value string) {
	t.Helper()
	path := env.Resolve(zfsBcloneParam)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// storeReplies loads the runner answers a store run needs: the pool
// and dataset queries (present or absent), the findmnt answer and the
// block-cloning feature query, plus the two creates so a successful
// run does not trip the runner's unknown-command error. A test that
// wants the creates to have failed replaces their rows.
func storeReplies(poolPresent, datasetPresent bool, fstype string) map[string]fakeReply {
	reply := map[string]fakeReply{
		"zpool get -H -o value feature@block_cloning forkdcache": {out: []byte("enabled\n")},
		"findmnt -n -o FSTYPE --target /forkdcache/e2b":          {out: []byte(fstype + "\n")},
		"zpool create -o ashift=12 -o autotrim=on -O atime=off -O compression=lz4 -O xattr=sa -O mountpoint=/forkdcache forkdcache /dev/sdb": {out: nil},
		"zfs create -o mountpoint=/forkdcache/e2b forkdcache/e2b":                                                                            {out: nil},
	}
	if poolPresent {
		reply["zpool list -H -o name forkdcache"] = fakeReply{out: []byte("forkdcache\n")}
	}
	if datasetPresent {
		reply["zfs list -H forkdcache/e2b"] = fakeReply{out: []byte("forkdcache/e2b\n")}
	}
	return reply
}

func runStore(t *testing.T, env *Env) ([]Item, error) {
	t.Helper()
	r := &Reporter{Phase: PhaseHost}
	err := storeStep(context.Background(), env, r, &hostState{})
	return r.Items(), err
}

// storeRow finds the report row named name.
func storeRow(t *testing.T, items []Item, name string) Item {
	t.Helper()
	for _, it := range items {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("no row named %s in:\n%+v", name, items)
	return Item{}
}

func TestStoreDiskCreatesPoolAndDataset(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	runner.reply = storeReplies(false, false, "zfs")

	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("store step: %v", err)
	}

	// The pool create carries exactly the wanted options, the dataset
	// create points at the store dir, and the feature is read back.
	var sawPool, sawDataset, sawFeature bool
	for _, call := range runner.calls {
		switch {
		case call == "zpool create -o ashift=12 -o autotrim=on -O atime=off -O compression=lz4 -O xattr=sa -O mountpoint=/forkdcache forkdcache /dev/sdb":
			sawPool = true
		case call == "zfs create -o mountpoint=/forkdcache/e2b forkdcache/e2b":
			sawDataset = true
		case call == "zpool get -H -o value feature@block_cloning forkdcache":
			sawFeature = true
		}
	}
	if !sawPool || !sawDataset || !sawFeature {
		t.Fatalf("calls:\n%s", strings.Join(runner.calls, "\n"))
	}

	if it := storeRow(t, items, "zpool forkdcache"); it.Result != Created {
		t.Errorf("pool row = %+v, want created", it)
	}
	if it := storeRow(t, items, "zfs forkdcache/e2b"); it.Result != Created {
		t.Errorf("dataset row = %+v, want created", it)
	}
	if it := storeRow(t, items, "zfs block cloning"); it.Result != Kept {
		t.Errorf("block cloning row = %+v, want kept", it)
	}

	// The directories under the store exist, tmp 0700.
	for _, sub := range []string{"orchestrator", "storage/templates", "storage/build-cache", "tmp", "registry"} {
		fi, err := os.Stat(env.Resolve(DefaultStoreDir + "/" + sub))
		if err != nil {
			t.Fatalf("store subdir %s: %v", sub, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
	if fi, err := os.Stat(env.Resolve(DefaultStoreDir + "/tmp")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("tmp mode = %v (%v), want 0700", fi.Mode().Perm(), err)
	}
}

func TestStoreDiskPoolPresentIsKept(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	runner.reply = storeReplies(true, true, "zfs")

	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("store step: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "zpool create") || strings.Contains(call, "zfs create") {
			t.Errorf("a create ran although pool and dataset exist: %s", call)
		}
	}
	if it := storeRow(t, items, "zpool forkdcache"); it.Result != Kept {
		t.Errorf("pool row = %+v, want kept", it)
	}
	if it := storeRow(t, items, "zfs forkdcache/e2b"); it.Result != Kept {
		t.Errorf("dataset row = %+v, want kept", it)
	}
	if it := storeRow(t, items, "zfs block cloning"); it.Result != Kept {
		t.Errorf("block cloning row = %+v, want kept (checked even on an old pool)", it)
	}
}

func TestStoreDiskFailsWhenBlockCloningOff(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	setZfsBclone(t, env, "0")
	runner.reply = storeReplies(false, false, "zfs")

	_, err := runStore(t, env)
	if err == nil {
		t.Fatal("store step with zfs_bclone_enabled=0, want an error")
	}
	if !strings.Contains(err.Error(), "/etc/modprobe.d") {
		t.Errorf("error %q does not carry the modprobe.d fix", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "zpool create") {
			t.Errorf("the pool was created although block cloning is off: %s", call)
		}
	}
}

func TestStoreDiskFailsWhenFeatureDisabled(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	reply := storeReplies(false, false, "zfs")
	reply["zpool get -H -o value feature@block_cloning forkdcache"] = fakeReply{out: []byte("disabled\n")}
	runner.reply = reply

	items, err := runStore(t, env)
	if err == nil {
		t.Fatal("store step with a disabled block_cloning feature, want an error")
	}
	if !strings.Contains(err.Error(), "block cloning") {
		t.Errorf("error %q does not name block cloning", err)
	}
	if it := storeRow(t, items, "zfs block cloning"); it.Result != Failed {
		t.Errorf("block cloning row = %+v, want failed", it)
	}
}

// TestStoreDiskDryRunDoesNotFailOnThePlannedPool: a dry run on a
// fresh host plans zpool create and zfs create; the block-cloning
// feature query would fail (the pool it reads does not exist yet), so
// the step skips it instead of failing the plan.
func TestStoreDiskDryRunDoesNotFailOnThePlannedPool(t *testing.T) {
	env, runner := storeEnv(t)
	env.DryRun = true
	env.Opt.StoreDisk = "/dev/sdb"
	reply := storeReplies(false, false, "zfs")
	// A fresh host has no pool, so the feature query fails like the
	// create it was declined in the dry run.
	delete(reply, "zpool get -H -o value feature@block_cloning forkdcache")
	runner.reply = reply

	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("fresh-host dry run with --store-disk must plan, not fail: %v", err)
	}
	if it := storeRow(t, items, "zpool forkdcache"); it.Result != Created {
		t.Errorf("pool row = %+v, want created (the plan)", it)
	}
	if it := storeRow(t, items, "zfs forkdcache/e2b"); it.Result != Created {
		t.Errorf("dataset row = %+v, want created (the plan)", it)
	}
	if it := storeRow(t, items, "zfs block cloning"); it.Result != Skipped {
		t.Errorf("block cloning row = %+v, want skipped (the planned pool does not exist to query)", it)
	}
	if !strings.Contains(storeRow(t, items, "zfs block cloning").Note, "dry-run") {
		t.Errorf("block cloning note %q should say why it was skipped", storeRow(t, items, "zfs block cloning").Note)
	}
}

// TestStoreDiskFeatureQueryFailureCarriesTheFix: a real run whose
// feature query fails (pool present but unreadable) fails the step
// with what to do next, not a bare error.
func TestStoreDiskFeatureQueryFailureCarriesTheFix(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	reply := storeReplies(true, true, "zfs")
	reply["zpool get -H -o value feature@block_cloning forkdcache"] = fakeReply{err: errors.New("cannot open 'forkdcache': no such pool")}
	runner.reply = reply

	items, err := runStore(t, env)
	if err == nil {
		t.Fatal("store step with an unreadable pool, want an error")
	}
	if !strings.Contains(err.Error(), "zpool status") {
		t.Errorf("error %q does not carry the fix", err)
	}
	if it := storeRow(t, items, "zfs block cloning"); it.Result != Failed {
		t.Errorf("block cloning row = %+v, want failed", it)
	}
}

func TestStoreDirMissingFails(t *testing.T) {
	env, runner := storeEnv(t)
	runner.reply = map[string]fakeReply{}

	items, err := runStore(t, env)
	if err == nil {
		t.Fatal("store step on a missing store dir, want an error")
	}
	want := "create it on an XFS (reflink) or ZFS filesystem, or pass --store-disk"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q, want the %q text", err, want)
	}
	if len(items) == 0 || items[len(items)-1].Result != Failed {
		t.Errorf("rows = %+v, want a failed row last", items)
	}
}

func TestStoreDirFilesystemKeptOrWarned(t *testing.T) {
	cases := []struct {
		fstype string
		want   Result
	}{
		{"zfs", Kept},
		{"xfs", Kept},
		{"ext4", Skipped},
	}
	for _, tc := range cases {
		t.Run(tc.fstype, func(t *testing.T) {
			env, runner := storeEnv(t)
			if err := os.MkdirAll(env.Resolve(DefaultStoreDir), 0o755); err != nil {
				t.Fatal(err)
			}
			runner.reply = storeReplies(false, false, tc.fstype)

			items, err := runStore(t, env)
			if err != nil {
				t.Fatalf("store step: %v", err)
			}
			it := storeRow(t, items, "store filesystem")
			if it.Result != tc.want {
				t.Errorf("%s row = %s, want %s (note %q)", tc.fstype, it.Result, tc.want, it.Note)
			}
			if !strings.Contains(it.Note, tc.fstype) {
				t.Errorf("row note %q does not name the filesystem", it.Note)
			}
		})
	}
}

func TestStoreDirExt4WarningNotFailure(t *testing.T) {
	env, runner := storeEnv(t)
	if err := os.MkdirAll(env.Resolve(DefaultStoreDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runner.reply = storeReplies(false, false, "ext4")

	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("store step on ext4 must not fail: %v", err)
	}
	it := storeRow(t, items, "store filesystem")
	if it.Result == Failed {
		t.Errorf("ext4 row is a failure: %+v", it)
	}
	if !strings.Contains(it.Note, "ext4") {
		t.Errorf("warning row %+v does not name ext4", it)
	}
	// The directories are still laid out.
	if _, err := os.Stat(env.Resolve(DefaultStoreDir + "/orchestrator")); err != nil {
		t.Errorf("orchestrator dir: %v", err)
	}
}

func TestStoreDirCreatesSubdirs(t *testing.T) {
	env, runner := storeEnv(t)
	if err := os.MkdirAll(env.Resolve(DefaultStoreDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runner.reply = storeReplies(false, false, "xfs")

	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("store step: %v", err)
	}
	var dirRows int
	for _, it := range items {
		if strings.HasPrefix(it.Name, DefaultStoreDir+"/") {
			dirRows++
			if it.Result != Created {
				t.Errorf("row %s = %s, want created", it.Name, it.Result)
			}
		}
	}
	if dirRows != 5 {
		t.Errorf("%d directory rows, want 5:\n%+v", dirRows, items)
	}
	for _, sub := range []string{"orchestrator", "storage/templates", "storage/build-cache", "tmp", "registry"} {
		fi, err := os.Stat(env.Resolve(DefaultStoreDir + "/" + sub))
		if err != nil {
			t.Fatalf("subdir %s: %v", sub, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
	if fi, _ := os.Stat(env.Resolve(DefaultStoreDir + "/tmp")); fi.Mode().Perm() != 0o700 {
		t.Errorf("tmp mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestStoreStepSecondRunKeepsAll(t *testing.T) {
	env, runner := storeEnv(t)
	if err := os.MkdirAll(env.Resolve(DefaultStoreDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runner.reply = storeReplies(false, false, "xfs")
	if _, err := runStore(t, env); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Second run: every row kept, no create command ran again.
	runner.calls = nil
	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, it := range items {
		if it.Result != Kept {
			t.Errorf("second run row %+v, want kept", it)
		}
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " create ") {
			t.Errorf("second run ran a changing command: %s", call)
		}
	}
}

// TestStoreDiskWithContainerFails: the pool cannot be built from
// inside the prepare container; the error says to create the store on
// the host and pass --store-dir, and nothing runs.
func TestStoreDiskWithContainerFails(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.Container = true
	env.Opt.StoreDisk = "/dev/sdb"
	runner.reply = map[string]fakeReply{} // every command would error

	items, err := runStore(t, env)
	if err == nil {
		t.Fatal("--store-disk with --container, want an error")
	}
	for _, want := range []string{"--store-disk is not available with --container", "create the store on the host", "--store-dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %v, want none", runner.calls)
	}
	if len(items) != 1 || items[0].Result != Failed {
		t.Fatalf("rows = %+v, want one failed row", items)
	}
}

// TestStoreDirContainerCheckedUnderRoot: with --container (no
// --store-disk) the store dir is still checked under Root, exactly
// like a native run.
func TestStoreDirContainerCheckedUnderRoot(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.Container = true
	if err := os.MkdirAll(env.Resolve(DefaultStoreDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runner.reply = storeReplies(false, false, "xfs")

	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("store step: %v", err)
	}
	if it := storeRow(t, items, "store filesystem"); it.Result != Kept || !strings.Contains(it.Note, "xfs") {
		t.Errorf("store filesystem row = %+v, want kept on xfs", it)
	}
	if _, err := os.Stat(env.Resolve(DefaultStoreDir + "/orchestrator")); err != nil {
		t.Errorf("orchestrator dir under Root: %v", err)
	}
	// findmnt ran against the store dir as the container sees it.
	if !sawCall(runner, "findmnt -n -o FSTYPE --target /forkdcache/e2b") {
		t.Errorf("calls lack the findmnt query:\n%s", strings.Join(runner.calls, "\n"))
	}
}

// sawCall reports whether the runner recorded exactly this command
// line.
func sawCall(runner *recordRunner, want string) bool {
	for _, c := range runner.calls {
		if c == want {
			return true
		}
	}
	return false
}

// dockerReplies loads the runner answers a docker run needs: the
// registry inspect (running or not) plus the changing commands so a
// successful run does not trip the runner's unknown-command error.
func dockerReplies(registryRunning bool) map[string]fakeReply {
	reply := map[string]fakeReply{
		"systemctl daemon-reload":       {},
		"systemctl enable --now docker": {},
	}
	if registryRunning {
		reply["docker inspect -f {{.State.Running}} spoond-registry"] = fakeReply{out: []byte("true\n")}
	}
	reply["docker run -d --name spoond-registry --restart=always -p 127.0.0.1:5000:5000 -v /forkdcache/e2b/registry:/var/lib/registry registry:2.8.3"] = fakeReply{out: []byte("sha256:deadbeef\n")}
	return reply
}

func runDocker(t *testing.T, env *Env) ([]Item, error) {
	t.Helper()
	r := &Reporter{Phase: PhaseHost}
	err := dockerStep(context.Background(), env, r, &hostState{})
	return r.Items(), err
}

func TestDockerDropinWithoutStoreDisk(t *testing.T) {
	env, runner := storeEnv(t)
	runner.reply = dockerReplies(false)

	items, err := runDocker(t, env)
	if err != nil {
		t.Fatalf("docker step: %v", err)
	}
	b, mode := read(t, env, dockerDropin)
	want := "[Unit]\nRequiresMountsFor=/forkdcache/e2b\n"
	if string(b) != want {
		t.Errorf("drop-in = %q, want %q", b, want)
	}
	if mode != 0o644 {
		t.Errorf("drop-in mode = %#o, want 0644", mode)
	}
	// No zfs-mount.service line without --store-disk.
	if strings.Contains(string(b), "zfs-mount") {
		t.Errorf("drop-in names zfs-mount.service without --store-disk: %q", b)
	}
	// The drop-in was new, so daemon-reload ran.
	sawReload := false
	for _, call := range runner.calls {
		if call == "systemctl daemon-reload" {
			sawReload = true
		}
	}
	if !sawReload {
		t.Errorf("calls have no daemon-reload after a new drop-in:\n%s", strings.Join(runner.calls, "\n"))
	}
	_ = items
}

func TestDockerDropinWithStoreDisk(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.StoreDisk = "/dev/sdb"
	runner.reply = dockerReplies(false)

	if _, err := runDocker(t, env); err != nil {
		t.Fatalf("docker step: %v", err)
	}
	b, _ := read(t, env, dockerDropin)
	want := "[Unit]\nRequiresMountsFor=/forkdcache/e2b\nAfter=zfs-mount.service\n"
	if string(b) != want {
		t.Errorf("drop-in = %q, want %q", b, want)
	}
}

func TestDockerReloadOnlyWhenChanged(t *testing.T) {
	env, runner := storeEnv(t)
	runner.reply = dockerReplies(false)
	if _, err := runDocker(t, env); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Second run: the drop-in is identical, no daemon-reload.
	runner.calls = nil
	items, err := runDocker(t, env)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, call := range runner.calls {
		if call == "systemctl daemon-reload" {
			t.Errorf("second run reloaded the daemon although the drop-in is unchanged")
		}
	}
	if it := storeRow(t, items, "systemd daemon-reload"); it.Result != Kept || !strings.Contains(it.Note, "unchanged") {
		t.Errorf("daemon-reload row = %+v, want kept with an unchanged note", it)
	}

	// A changed drop-in reloads again. The registry inspect answers
	// running this time, so the store-dir change is the only delta.
	env.Opt.StoreDir = "/other/store"
	runner.reply = dockerReplies(true)
	runner.calls = nil
	if _, err := runDocker(t, env); err != nil {
		t.Fatalf("changed run: %v", err)
	}
	sawReload := false
	for _, call := range runner.calls {
		if call == "systemctl daemon-reload" {
			sawReload = true
		}
	}
	if !sawReload {
		t.Errorf("a changed drop-in did not reload the daemon:\n%s", strings.Join(runner.calls, "\n"))
	}

	// A dry run over a stale drop-in reports the planned reload
	// instead of the contradictory "unchanged".
	env.Opt.StoreDir = "/third/store"
	env.DryRun = true
	runner.calls = nil
	items, err = runDocker(t, env)
	if err != nil {
		t.Fatalf("dry run over the stale drop-in: %v", err)
	}
	for _, call := range runner.calls {
		if call == "systemctl daemon-reload" {
			t.Errorf("the dry run reloaded the daemon: %s", call)
		}
	}
	if it := storeRow(t, items, "systemd daemon-reload"); it.Result != Kept || !strings.Contains(it.Note, "dry-run") {
		t.Errorf("daemon-reload row = %+v, want a kept row saying the real run reloads", it)
	}
}

func TestDockerRegistry(t *testing.T) {
	t.Run("running is kept", func(t *testing.T) {
		env, runner := storeEnv(t)
		runner.reply = dockerReplies(true)
		items, err := runDocker(t, env)
		if err != nil {
			t.Fatalf("docker step: %v", err)
		}
		it := storeRow(t, items, "registry spoond-registry")
		if it.Result != Kept {
			t.Errorf("registry row = %+v, want kept", it)
		}
		for _, call := range runner.calls {
			if strings.Contains(call, "docker run") {
				t.Errorf("docker run executed although the registry runs: %s", call)
			}
		}
	})
	t.Run("missing runs the registry", func(t *testing.T) {
		env, runner := storeEnv(t)
		runner.reply = dockerReplies(false)
		items, err := runDocker(t, env)
		if err != nil {
			t.Fatalf("docker step: %v", err)
		}
		it := storeRow(t, items, "registry spoond-registry")
		if it.Result != Created {
			t.Errorf("registry row = %+v, want created", it)
		}
		want := "docker run -d --name spoond-registry --restart=always -p 127.0.0.1:5000:5000 -v /forkdcache/e2b/registry:/var/lib/registry registry:2.8.3"
		saw := false
		for _, call := range runner.calls {
			if call == want {
				saw = true
			}
		}
		if !saw {
			t.Errorf("calls lack the registry line:\n%s", strings.Join(runner.calls, "\n"))
		}
	})
}

func TestDockerStepSecondRunKeepsAll(t *testing.T) {
	env, runner := storeEnv(t)
	runner.reply = dockerReplies(false)
	if _, err := runDocker(t, env); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Second run: the registry container now exists, so the inspect
	// answers true and nothing changes.
	runner.reply = dockerReplies(true)
	runner.calls = nil
	items, err := runDocker(t, env)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, it := range items {
		if it.Result == Created || it.Result == Changed {
			t.Errorf("second run row %+v, want kept or skipped", it)
		}
	}
	for _, call := range runner.calls {
		if call == "systemctl daemon-reload" {
			t.Errorf("second run reloaded the daemon")
		}
		if strings.Contains(call, "docker run") {
			t.Errorf("second run re-ran the registry: %s", call)
		}
	}
}

func TestDockerDryRunWritesNothingQueriesOnly(t *testing.T) {
	env, runner := storeEnv(t)
	env.DryRun = true
	runner.reply = dockerReplies(false)

	items, err := runDocker(t, env)
	if err != nil {
		t.Fatalf("docker dry run: %v", err)
	}
	mustNotExist(t, env, dockerDropin)
	if it := storeRow(t, items, dockerDropin); it.Result != Created {
		t.Errorf("drop-in row = %+v, want created (the plan still shows it)", it)
	}
	for _, call := range runner.calls {
		switch {
		case call == "docker inspect -f {{.State.Running}} spoond-registry":
			// query: runs in a dry run.
		default:
			t.Errorf("dry run ran a changing command: %s", call)
		}
	}
}

// setApparmor writes the fake AppArmor sysfs switch under the root;
// absent means the file is not written at all.
func setApparmor(t *testing.T, env *Env, value string) {
	t.Helper()
	if value == "" {
		return
	}
	path := env.Resolve(apparmorParam)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// apparmorReplies loads the runner answers an apparmor run needs: the
// changing docker run so a passing check does not trip the runner's
// unknown-command error. A test that wants the check to fail replaces
// the row with an error.
func apparmorReplies() map[string]fakeReply {
	return map[string]fakeReply{
		"docker run --rm python:3.13-slim python3 -c import socket; s=socket.socket(socket.AF_UNIX); s.bind(\"/tmp/s\"); print(\"unix ok\")": {out: []byte("unix ok\n")},
	}
}

func runApparmor(t *testing.T, env *Env) ([]Item, error) {
	t.Helper()
	r := &Reporter{Phase: PhaseHost}
	err := apparmorStep(context.Background(), env, r, &hostState{})
	return r.Items(), err
}

func TestApparmorSkippedWhenDisabledOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"absent", ""},
		{"N", "N"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, runner := storeEnv(t)
			setApparmor(t, env, tc.value)
			runner.reply = apparmorReplies()

			items, err := runApparmor(t, env)
			if err != nil {
				t.Fatalf("apparmor step: %v", err)
			}
			if len(items) != 1 || items[0].Result != Skipped {
				t.Fatalf("rows = %+v, want one skipped row", items)
			}
			for _, call := range runner.calls {
				if strings.Contains(call, "docker run") {
					t.Errorf("the check container ran although AppArmor is off: %s", call)
				}
			}
		})
	}
}

func TestApparmorCheckPasses(t *testing.T) {
	env, runner := storeEnv(t)
	setApparmor(t, env, "Y")
	runner.reply = apparmorReplies()

	items, err := runApparmor(t, env)
	if err != nil {
		t.Fatalf("apparmor step: %v", err)
	}
	if len(items) != 1 || items[0].Result != Kept {
		t.Fatalf("rows = %+v, want one kept row", items)
	}
	saw := false
	want := `docker run --rm python:3.13-slim python3 -c import socket; s=socket.socket(socket.AF_UNIX); s.bind("/tmp/s"); print("unix ok")`
	for _, call := range runner.calls {
		if call == want {
			saw = true
		}
	}
	if !saw {
		t.Errorf("calls have the check container:\n%s", strings.Join(runner.calls, "\n"))
	}
}

func TestApparmorCheckFailsThePhase(t *testing.T) {
	env, runner := storeEnv(t)
	setApparmor(t, env, "Y")
	reply := apparmorReplies()
	checkCall := `docker run --rm python:3.13-slim python3 -c import socket; s=socket.socket(socket.AF_UNIX); s.bind("/tmp/s"); print("unix ok")`
	reply[checkCall] = fakeReply{err: errors.New("docker: apparmor denied")}
	runner.reply = reply

	items, err := runApparmor(t, env)
	if err == nil {
		t.Fatal("apparmor step with a failing check, want an error")
	}
	if !strings.Contains(err.Error(), "Docker's AppArmor profile denies Unix sockets in containers; see the AppArmor section of docs/install.md") {
		t.Errorf("error %q does not carry the docs text", err)
	}
	if len(items) != 1 || items[0].Result != Failed {
		t.Fatalf("rows = %+v, want one failed row", items)
	}
	if !strings.Contains(items[0].Note, "docs/install.md") {
		t.Errorf("failed row note %q does not point at the docs", items[0].Note)
	}
}

func TestApparmorDryRunSkipsTheCheck(t *testing.T) {
	env, runner := storeEnv(t)
	env.DryRun = true
	setApparmor(t, env, "Y")
	runner.reply = apparmorReplies()

	items, err := runApparmor(t, env)
	if err != nil {
		t.Fatalf("apparmor dry run: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("rows = %+v, want one row", items)
	}
	// The check container is env.exec, so a dry run never ran it: the
	// row must not claim the sockets work.
	if items[0].Result != Skipped {
		t.Errorf("row = %+v, want skipped (the dry run did not run the probe)", items[0])
	}
	if !strings.Contains(items[0].Note, "dry-run") {
		t.Errorf("row note %q should say the check did not run", items[0].Note)
	}
}

// TestDockerContainerSkipsComposeRunsIt: with --container the step is
// one skipped row and runs no systemctl or docker command.
func TestDockerContainerSkipsComposeRunsIt(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.Container = true
	runner.reply = map[string]fakeReply{} // every command would error

	items, err := runDocker(t, env)
	if err != nil {
		t.Fatalf("docker step: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %v, want none", runner.calls)
	}
	mustNotExist(t, env, dockerDropin)
	if len(items) != 1 || items[0].Result != Skipped {
		t.Fatalf("rows = %+v, want one skipped row", items)
	}
	if items[0].Name != "docker" || !strings.Contains(items[0].Note, "docker compose runs the registry") {
		t.Errorf("row = %+v, want the compose note", items[0])
	}
}

// TestApparmorContainerSkipsTheProbe: with --container the step is one
// skipped row pointing at the host-side check, and runs no docker.
func TestApparmorContainerSkipsTheProbe(t *testing.T) {
	env, runner := storeEnv(t)
	env.Opt.Container = true
	setApparmor(t, env, "Y") // even with AppArmor on, the probe is the host's job
	runner.reply = apparmorReplies()

	items, err := runApparmor(t, env)
	if err != nil {
		t.Fatalf("apparmor step: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "docker run") {
			t.Errorf("the check container ran although the step is skipped: %s", call)
		}
	}
	if len(items) != 1 || items[0].Result != Skipped {
		t.Fatalf("rows = %+v, want one skipped row", items)
	}
	for _, want := range []string{"--container", "run the AppArmor check on the host", "docs/install.md"} {
		if !strings.Contains(items[0].Note, want) {
			t.Errorf("note %q does not mention %q", items[0].Note, want)
		}
	}
}

func TestStoreAndDockerStepsRegisteredInHostOrder(t *testing.T) {
	for _, name := range []string{"store", "docker", "apparmor"} {
		if hostSteps[name] == nil {
			t.Errorf("hostSteps[%q] is not registered", name)
		}
	}
	// The registry volume lands inside the store dir the store step
	// creates: host_store.go's layout must keep the registry entry.
	found := false
	for _, sub := range storeSubdirs {
		if sub.rel == "registry" {
			found = true
		}
	}
	if !found {
		t.Errorf("storeSubdirs has no registry entry, the docker step's volume would dangle")
	}
}

func TestStoreDryRunWritesNoDirsQueriesOnly(t *testing.T) {
	env, runner := storeEnv(t)
	env.DryRun = true
	if err := os.MkdirAll(env.Resolve(DefaultStoreDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runner.reply = storeReplies(false, false, "xfs")

	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("store dry run: %v", err)
	}
	// Nothing new on disk: the pre-existing store dir is all there is.
	for _, sub := range []string{"orchestrator", "storage/templates", "storage/build-cache", "tmp", "registry"} {
		if _, err := os.Stat(env.Resolve(DefaultStoreDir + "/" + sub)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the dry run created the subdirectory %s (stat: %v)", sub, err)
		}
	}
	// The report still shows the plan.
	var dirRows int
	for _, it := range items {
		if strings.HasPrefix(it.Name, DefaultStoreDir+"/") {
			dirRows++
			if it.Result != Created {
				t.Errorf("dry-run row %s = %s, want created (the plan)", it.Name, it.Result)
			}
			if !strings.Contains(it.Note, "dry-run") {
				t.Errorf("dry-run row %s should say nothing was written: %q", it.Name, it.Note)
			}
		}
	}
	if dirRows != 5 {
		t.Errorf("%d directory rows in the dry-run plan, want 5:\n%+v", dirRows, items)
	}
	// Only queries ran: findmnt; no zpool/zfs create.
	for _, call := range runner.calls {
		if strings.Contains(call, "zpool create") || strings.Contains(call, "zfs create") {
			t.Errorf("dry run ran a changing command: %s", call)
		}
	}
}

func TestStoreDiskDryRunPlansThePoolQueriesOnly(t *testing.T) {
	env, runner := storeEnv(t)
	env.DryRun = true
	env.Opt.StoreDisk = "/dev/sdb"
	runner.reply = storeReplies(false, false, "zfs")

	items, err := runStore(t, env)
	if err != nil {
		t.Fatalf("store dry run: %v", err)
	}
	it := storeRow(t, items, "zpool forkdcache")
	if it.Result != Created {
		t.Errorf("pool row = %+v, want created (the plan)", it)
	}
	if !strings.Contains(it.Note, "dry-run") {
		t.Errorf("pool row note %q should say nothing was run", it.Note)
	}
	for _, call := range runner.calls {
		switch call {
		case "zpool list -H -o name forkdcache", "zfs list -H forkdcache/e2b",
			"zpool get -H -o value feature@block_cloning forkdcache":
			// queries: fine
		default:
			t.Errorf("dry run ran a changing command: %s", call)
		}
	}
}

func TestDockerDryRunSecondRunKeptPlan(t *testing.T) {
	env, runner := storeEnv(t)
	// Real first run lays the drop-in down.
	runner.reply = dockerReplies(true)
	if _, err := runDocker(t, env); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Dry run over the unchanged host: no changing command runs. The
	// docker service row plans the enable (nothing ran), the rest are
	// kept.
	env.DryRun = true
	runner.calls = nil
	items, err := runDocker(t, env)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, it := range items {
		if it.Result == Changed {
			t.Errorf("dry run over a converged host plans %+v, want kept", it)
		}
	}
	if it := storeRow(t, items, "docker service"); it.Result != Created || !strings.Contains(it.Note, "dry-run") {
		t.Errorf("docker service row = %+v, want a created row saying nothing ran", it)
	}
	for _, call := range runner.calls {
		switch {
		case call == "docker inspect -f {{.State.Running}} spoond-registry":
		case call == "systemctl enable --now docker":
			t.Errorf("dry run ran a changing command: %s", call)
		}
	}
}

// A registry container that exists but is stopped is started, not
// re-created (docker run would fail on the name).
func TestDockerRegistryStoppedIsStarted(t *testing.T) {
	env, runner := storeEnv(t)
	runner.reply = dockerReplies(false)
	runner.reply["docker inspect -f {{.State.Running}} spoond-registry"] = fakeReply{out: []byte("false\n")}
	runner.reply["docker start spoond-registry"] = fakeReply{}
	if _, err := runDocker(t, env); err != nil {
		t.Fatalf("docker step: %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	if !strings.Contains(joined, "docker start spoond-registry") {
		t.Errorf("no docker start in %v", runner.calls)
	}
	if strings.Contains(joined, "docker run -d --name spoond-registry") {
		t.Errorf("re-created a stopped registry: %v", runner.calls)
	}
}
