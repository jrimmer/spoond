// Tests for the host phase's store step (host_store.go): the
// --store-disk pool path and the plain-directory path, through the
// fake runner, against a temp root.
package install

import (
	"context"
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
