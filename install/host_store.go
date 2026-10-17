// The host phase's store, Docker and AppArmor steps (hostOrder rows
// "store", "docker", "apparmor"): the snapshot store on its ZFS pool
// or an existing directory, Docker ordered after the store mount with
// the local registry, and the AppArmor Unix-socket check. The manual
// counterparts are steps 4 and 9 of deploy/e2b/host-setup.sh.
package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

func init() {
	hostSteps["store"] = storeStep
	hostSteps["docker"] = dockerStep
	hostSteps["apparmor"] = apparmorStep
}

// The pool and dataset the store step manages; the pool mounts at
// /forkdcache and the dataset at the store dir (default the dataset
// mountpoint below).
const (
	storePool    = "forkdcache"
	storeDataset = "forkdcache/e2b"
)

// zfsBcloneParam is the ZFS module parameter that says block cloning
// is on; snapshots need it, so the pool is only created when it reads 1.
const zfsBcloneParam = "/sys/module/zfs/parameters/zfs_bclone_enabled"

// storeSubdirs is the directory layout under the store: one entry per
// directory, created 0755 unless mode says otherwise (tmp holds
// sandbox sockets and stays 0700). Ordered so parents come first.
var storeSubdirs = []struct {
	rel  string
	mode os.FileMode
}{
	{"orchestrator", 0o755},
	{"storage/templates", 0o755},
	{"storage/build-cache", 0o755},
	{"tmp", 0o700},
	{"registry", 0o755},
}

// storeStep lays out the snapshot store: with --store-disk the
// forkdcache ZFS pool and its dataset, without it a check of the
// existing store directory and its filesystem. Either way the
// directory layout under the store dir.
func storeStep(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	dir := env.Opt.storeDir()
	if disk := env.Opt.StoreDisk; disk != "" {
		if err := storeOnDisk(ctx, env, r, dir, disk); err != nil {
			return err
		}
		return makeStoreDirs(env, r, dir)
	}
	if err := storeOnDir(ctx, env, r, dir); err != nil {
		return err
	}
	return makeStoreDirs(env, r, dir)
}

// storeOnDisk brings the pool up when --store-disk names a disk: an
// existing forkdcache is kept, a new one is created with the store
// tunings after the ZFS module's block cloning is confirmed on (a pool
// created without the feature never gains it), the dataset is created
// unless it exists, and the pool's block-cloning feature is confirmed
// enabled or active at the end.
func storeOnDisk(ctx context.Context, env *Env, r *Reporter, dir, disk string) error {
	// poolCreated says this run brought the pool up (or, in a dry run,
	// plans to): the feature query at the end can only succeed after a
	// real create, so a dry run skips it instead of failing the plan.
	poolCreated := false
	if _, err := env.query(ctx, "zpool", "list", "-H", "-o", "name", storePool); err == nil {
		r.Add(Kept, "zpool "+storePool, "pool exists")
	} else {
		// The pool would be new: block cloning must be on in the
		// running module before the first pool is created.
		b, err := os.ReadFile(env.Resolve(zfsBcloneParam))
		if err != nil {
			note := fmt.Sprintf("cannot read %s (is the ZFS module loaded? run modprobe zfs, with `options zfs zfs_bclone_enabled=1` in /etc/modprobe.d/zfs.conf): %v", zfsBcloneParam, err)
			r.Add(Failed, "zfs block cloning", note)
			return errors.New(note)
		}
		if v := strings.TrimSpace(string(b)); v != "1" {
			note := fmt.Sprintf("ZFS block cloning is off (%s is %q) — write `options zfs zfs_bclone_enabled=1` to /etc/modprobe.d/zfs.conf and reload the zfs module (export the pool first, or reboot), then rerun", zfsBcloneParam, v)
			r.Add(Failed, "zfs block cloning", note)
			return errors.New(note)
		}
		if err := env.exec(ctx, "zpool", "create", "-o", "ashift=12", "-o", "autotrim=on",
			"-O", "atime=off", "-O", "compression=lz4", "-O", "xattr=sa", "-O", "mountpoint=/forkdcache",
			storePool, disk); err != nil {
			note := fmt.Sprintf("zpool create %s on %s failed — check the disk is whole and unused (lsblk, wipefs -n): %v", storePool, disk, err)
			r.Add(Failed, "zpool "+storePool, note)
			return errors.New(note)
		}
		r.Add(Created, "zpool "+storePool, dryRunNote(env, "on "+disk+", mountpoint /forkdcache"))
		poolCreated = true
	}

	if _, err := env.query(ctx, "zfs", "list", "-H", storeDataset); err != nil {
		if err := env.exec(ctx, "zfs", "create", "-o", "mountpoint="+dir, storeDataset); err != nil {
			note := fmt.Sprintf("zfs create %s failed: %v", storeDataset, err)
			r.Add(Failed, "zfs "+storeDataset, note)
			return errors.New(note)
		}
		r.Add(Created, "zfs "+storeDataset, dryRunNote(env, "mountpoint "+dir))
	} else {
		r.Add(Kept, "zfs "+storeDataset, "dataset exists")
	}

	out, err := env.query(ctx, "zpool", "get", "-H", "-o", "value", "feature@block_cloning", storePool)
	if err != nil {
		if env.DryRun && poolCreated {
			// The planned zpool create never ran, so the pool is not
			// there to query; the real run reads the feature off the
			// pool it just created.
			r.Add(Skipped, "zfs block cloning", "dry-run: the planned pool does not exist yet, feature@block_cloning is unreadable until the create runs")
			return nil
		}
		note := fmt.Sprintf("cannot read feature@block_cloning on %s — the pool exists but is unreadable; check `zpool status %s` and that the ZFS tools match the loaded module: %v", storePool, storePool, err)
		r.Add(Failed, "zfs block cloning", note)
		return errors.New(note)
	}
	if v := strings.TrimSpace(string(out)); v != "enabled" && v != "active" {
		note := fmt.Sprintf("%s reports feature@block_cloning=%q, want enabled or active — snapshots need block cloning; create the pool on a ZFS build with the feature (zfs-dkms from contrib)", storePool, v)
		r.Add(Failed, "zfs block cloning", note)
		return errors.New(note)
	}
	r.Add(Kept, "zfs block cloning", "feature@block_cloning enabled")
	return nil
}

// storeOnDir checks an existing store directory when no --store-disk
// was given: the directory must exist, and its filesystem is reported
// — zfs and xfs are kept, anything else is a warning (reflink copies
// degrade to full copies), never a failure.
func storeOnDir(ctx context.Context, env *Env, r *Reporter, dir string) error {
	full := env.Resolve(dir)
	fi, err := os.Stat(full)
	switch {
	case errors.Is(err, os.ErrNotExist):
		note := fmt.Sprintf("%s does not exist — create it on an XFS (reflink) or ZFS filesystem, or pass --store-disk", dir)
		r.Add(Failed, dir, note)
		return errors.New(note)
	case err != nil:
		note := fmt.Sprintf("cannot read %s: %v", dir, err)
		r.Add(Failed, dir, note)
		return errors.New(note)
	case !fi.IsDir():
		note := fmt.Sprintf("%s is not a directory — create it on an XFS (reflink) or ZFS filesystem, or pass --store-disk", dir)
		r.Add(Failed, dir, note)
		return errors.New(note)
	}

	out, err := env.query(ctx, "findmnt", "-n", "-o", "FSTYPE", "--target", dir)
	if err != nil {
		r.Add(Skipped, "store filesystem", fmt.Sprintf("could not read the filesystem type under %s (findmnt: %v) — XFS (reflink) or ZFS is wanted", dir, err))
		return nil
	}
	switch fstype := strings.TrimSpace(string(out)); fstype {
	case "zfs", "xfs":
		r.Add(Kept, "store filesystem", fmt.Sprintf("%s is on %s", dir, fstype))
	default:
		if fstype == "" {
			fstype = "unknown"
		}
		r.Add(Skipped, "store filesystem", fmt.Sprintf("%s is on %s — reflink copies want XFS (reflink) or ZFS; snapshot saves and image builds fall back to full copies", dir, fstype))
	}
	return nil
}

// makeStoreDirs creates the store's directory layout, one row per
// directory: kept when the directory exists with the wanted mode,
// changed when the mode was corrected, created when new.
func makeStoreDirs(env *Env, r *Reporter, dir string) error {
	for _, sub := range storeSubdirs {
		name := dir + "/" + sub.rel
		full := env.Resolve(name)
		res := Created
		if fi, err := os.Stat(full); err == nil {
			if !fi.IsDir() {
				note := fmt.Sprintf("%s exists and is not a directory — remove it and rerun", full)
				r.Add(Failed, name, note)
				return errors.New(note)
			}
			if fi.Mode().Perm() == sub.mode {
				r.Add(Kept, name, "")
				continue
			}
			res = Changed
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", full, err)
		}
		if env.DryRun {
			r.Add(res, name, "dry-run: nothing written")
			continue
		}
		if err := os.MkdirAll(full, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", full, err)
		}
		if err := os.Chmod(full, sub.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", full, err)
		}
		r.Add(res, name, "")
	}
	return nil
}

// dryRunNote is the note for a row whose change env.exec skipped: the
// report still shows what a real run would do.
func dryRunNote(env *Env, note string) string {
	if env.DryRun {
		return "dry-run: nothing run"
	}
	return note
}

// dockerDropin is the systemd drop-in path ordering Docker after the
// store mount, so the registry's volume exists before Docker starts.
const dockerDropin = "/etc/systemd/system/docker.service.d/10-forkdcache.conf"

// changedDropin reports whether this run's drop-in write changed (or,
// in a dry run, plans to change) the file: writeFile's row is the
// signal — created or changed means systemd must reload, a kept row
// means the running configuration is current.
func changedDropin(r *Reporter) bool {
	for _, it := range r.Items() {
		if it.Name == dockerDropin && (it.Result == Created || it.Result == Changed) {
			return true
		}
	}
	return false
}

// registryContainer is the local registry container the docker step
// keeps running: image builds push to 127.0.0.1:5000 and pulls stay
// off the internet.
const registryContainer = "spoond-registry"

// dockerStep writes Docker's drop-in (RequiresMountsFor the store
// dir, After=zfs-mount.service with --store-disk), reloads the daemon
// only when the drop-in changed, enables and starts Docker, and brings
// up the local registry container unless it already runs.
func dockerStep(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	dir := env.Opt.storeDir()
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("RequiresMountsFor=" + dir + "\n")
	if env.Opt.StoreDisk != "" {
		b.WriteString("After=zfs-mount.service\n")
	}
	if err := env.writeFile(r, dockerDropin, []byte(b.String()), 0o644); err != nil {
		return err
	}
	// changedDropin reads writeFile's row for the drop-in: created or
	// changed means systemd must reload; a kept (or dry-run planned)
	// write leaves the running configuration current.
	reloaded := changedDropin(r)
	if reloaded && !env.DryRun {
		if err := env.exec(ctx, "systemctl", "daemon-reload"); err != nil {
			note := fmt.Sprintf("systemctl daemon-reload failed — run it by hand so the drop-in takes effect: %v", err)
			r.Add(Failed, "systemd daemon-reload", note)
			return errors.New(note)
		}
		r.Add(Changed, "systemd daemon-reload", "reloaded for "+dockerDropin)
	} else if reloaded {
		r.Add(Kept, "systemd daemon-reload", "dry-run: the planned drop-in change comes with a daemon-reload on the real run")
	} else {
		r.Add(Kept, "systemd daemon-reload", "drop-in unchanged, no reload")
	}

	enabled := !env.DryRun
	if err := env.exec(ctx, "systemctl", "enable", "--now", "docker"); err != nil {
		note := fmt.Sprintf("systemctl enable --now docker failed — install Docker (apt install docker.io) and check `systemctl status docker`: %v", err)
		r.Add(Failed, "docker service", note)
		return errors.New(note)
	}
	if enabled {
		r.Add(Kept, "docker service", "enabled and running (systemctl enable --now docker)")
	} else {
		r.Add(Created, "docker service", "dry-run: nothing run — the real run does systemctl enable --now docker")
	}

	out, err := env.query(ctx, "docker", "inspect", "-f", "{{.State.Running}}", registryContainer)
	if err == nil && strings.TrimSpace(string(out)) == "true" {
		r.Add(Kept, "registry "+registryContainer, "container is running")
		return nil
	}
	if err == nil && strings.TrimSpace(string(out)) == "false" {
		// The container exists but is stopped: start it; a docker run
		// would fail on the name.
		if err := env.exec(ctx, "docker", "start", registryContainer); err != nil {
			note := fmt.Sprintf("docker start %s failed — check `docker logs %s`: %v", registryContainer, registryContainer, err)
			r.Add(Failed, "registry "+registryContainer, note)
			return errors.New(note)
		}
		r.Add(Changed, "registry "+registryContainer, dryRunNote(env, "started the stopped container"))
		return nil
	}
	runArgs := []string{"run", "-d", "--name", registryContainer, "--restart=always",
		"-p", "127.0.0.1:5000:5000",
		"-v", dir + "/registry:/var/lib/registry",
		"registry:2.8.3"}
	if err := env.exec(ctx, "docker", runArgs...); err != nil {
		note := fmt.Sprintf("docker run %s failed — check `docker logs %s` and that %s/registry exists: %v", registryContainer, registryContainer, dir, err)
		r.Add(Failed, "registry "+registryContainer, note)
		return errors.New(note)
	}
	r.Add(Created, "registry "+registryContainer, dryRunNote(env, "registry:2.8.3 on 127.0.0.1:5000, data in "+dir+"/registry"))
	return nil
}

// apparmorParam is the sysfs switch that says the kernel loaded
// AppArmor; absent or N means the check does not apply.
const apparmorParam = "/sys/module/apparmor/parameters/enabled"

// apparmorCheckImage is the image the AppArmor check runs: the same
// base the build images use, so its profile is the one sandboxes get.
const apparmorCheckImage = "python:3.13-slim"

// apparmorCheckScript is the Python one-liner the check runs in the
// container: binding a Unix socket is what Docker's AppArmor profile
// denies when the sandbox's envd socket cannot be created.
const apparmorCheckScript = `import socket; s=socket.socket(socket.AF_UNIX); s.bind("/tmp/s"); print("unix ok")`

// errAppArmorDenies is the failure an open check ends the phase with:
// the fix lives in the AppArmor section of docs/install.md, not in
// anything the installer would edit itself.
var errAppArmorDenies = errors.New("Docker's AppArmor profile denies Unix sockets in containers; see the AppArmor section of docs/install.md")

// apparmorStep probes the Docker-in-AppArmor behaviour only where it
// exists: with the module unloaded (or off) the row is skipped, with
// it loaded the check container must be able to bind a Unix socket.
func apparmorStep(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
	b, err := os.ReadFile(env.Resolve(apparmorParam))
	if err != nil || strings.TrimSpace(string(b)) != "Y" {
		note := fmt.Sprintf("AppArmor is not enabled on this kernel (%s), the check does not apply", apparmorParam)
		if err != nil {
			note = fmt.Sprintf("%s is unreadable (%v), the check does not apply", apparmorParam, err)
		}
		r.Add(Skipped, "apparmor", note)
		return nil
	}
	if err := env.exec(ctx, "docker", "run", "--rm", apparmorCheckImage, "python3", "-c", apparmorCheckScript); err != nil {
		note := fmt.Sprintf("%v — `docker run --rm %s python3 -c '%s'` failed", errAppArmorDenies, apparmorCheckImage, apparmorCheckScript)
		r.Add(Failed, "apparmor", note)
		return errAppArmorDenies
	}
	if env.DryRun {
		r.Add(Skipped, "apparmor", "dry-run: the check container did not run — the real run probes Unix sockets with `docker run --rm "+apparmorCheckImage+"`")
		return nil
	}
	r.Add(Kept, "apparmor", "Docker containers can bind Unix sockets")
	return nil
}
