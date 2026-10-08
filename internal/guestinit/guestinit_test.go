// Package guestinit tests the real images/guest/spoond-guest-init script
// by running it inside a throwaway root. The script's whole job is
// absolute paths (/etc/spoond/guest-dns in, /etc/resolv.conf out), so
// the only faithful way to exercise it is a temp root and chroot. The
// test is skipped where chroot or the handful of host binaries it needs
// are unavailable.
package guestinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot resolves the checkout root from this file's location.
var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file))) // internal/guestinit/x.go -> root
}()

// buildRoot assembles a minimal root filesystem in dir: the shell, the
// few applets the script calls, their shared libraries, the script
// itself and an empty /dev/null. sleep is a stub that exits, so the
// script's trailing `exec sleep infinity` ends instead of hanging.
func buildRoot(t *testing.T, dir string) {
	t.Helper()
	for _, d := range []string{
		"bin", "dev", "etc/spoond/init.d", "run", "usr/local/bin", "code",
	} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "dev", "null"), nil, 0o666); err != nil {
		t.Fatalf("create dev/null: %v", err)
	}

	// Copy each host binary into /bin and every shared object ldd names
	// for it. The script calls cat/rm/touch/chmod by bare name, so they
	// must sit on the chroot's PATH; the shell goes to /bin/sh.
	bins := []string{"sh", "cat", "rm", "touch", "chmod"}
	for _, name := range bins {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("guest-init test needs %s: %v", name, err)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			t.Fatalf("abs %s: %v", name, err)
		}
		if err := copyFile(filepath.Join(dir, "bin", name), path); err != nil {
			t.Fatalf("copy %s: %v", path, err)
		}
		for _, lib := range lddLibs(t, path) {
			if err := copyInto(dir, lib); err != nil {
				t.Fatalf("copy %s: %v", lib, err)
			}
		}
	}

	// A completed stub exposes the script's exit path without an
	// external sleep applet.
	stub := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "bin", "sleep"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write sleep stub: %v", err)
	}

	script, err := os.ReadFile(filepath.Join(repoRoot, "images", "guest", "spoond-guest-init"))
	if err != nil {
		t.Fatalf("read guest-init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usr", "local", "bin", "spoond-guest-init"), script, 0o755); err != nil {
		t.Fatalf("install guest-init: %v", err)
	}
}

// lddLibs returns the shared objects ldd reports for path. Missing ldd
// or a static binary yields no libraries, which is not an error.
func lddLibs(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("ldd", path).Output()
	if err != nil {
		return nil
	}
	var libs []string
	for _, line := range strings.Split(string(out), "\n") {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "/") {
				libs = append(libs, field)
			}
		}
	}
	return libs
}

// copyInto copies a host file to the same absolute path under dir,
// creating parent directories.
func copyInto(dir, path string) error {
	return copyFile(filepath.Join(dir, path), path)
}

// copyFile copies src to dst, creating parent directories and keeping
// the source's permission bits.
func copyFile(dst, src string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

// runGuestInit runs the real script in a fresh temp root with the given
// /etc/spoond/guest-dns contents and returns the resulting
// /etc/resolv.conf.
func runGuestInit(t *testing.T, dns string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("guest-init test needs chroot (GOOS=%s)", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		t.Skip("guest-init test needs root for chroot")
	}
	chrootBin, err := exec.LookPath("chroot")
	if err != nil {
		for _, p := range []string{"/usr/sbin/chroot", "/sbin/chroot"} {
			if _, statErr := os.Stat(p); statErr == nil {
				chrootBin, err = p, nil
				break
			}
		}
	}
	if err != nil || chrootBin == "" {
		t.Skipf("guest-init test needs chroot: %v", err)
	}

	root := t.TempDir()
	buildRoot(t, root)
	if err := os.WriteFile(filepath.Join(root, "etc", "spoond", "guest-dns"), []byte(dns+"\n"), 0o644); err != nil {
		t.Fatalf("write guest-dns: %v", err)
	}

	cmd := exec.Command(chrootBin, root, "/bin/sh", "/usr/local/bin/spoond-guest-init")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run guest-init: %v\n%s", err, out)
	}

	resolv, err := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	return string(resolv)
}

func TestGuestInitSingleResolver(t *testing.T) {
	got := runGuestInit(t, "10.1.0.2")
	want := "nameserver 10.1.0.2\noptions timeout:2 attempts:3 rotate\n"
	if got != want {
		t.Fatalf("resolv.conf =\n%q\nwant\n%q", got, want)
	}
}

func TestGuestInitTwoResolvers(t *testing.T) {
	got := runGuestInit(t, "10.1.0.2,10.1.0.3")
	want := "nameserver 10.1.0.2\nnameserver 10.1.0.3\noptions timeout:2 attempts:3 rotate\n"
	if got != want {
		t.Fatalf("resolv.conf =\n%q\nwant\n%q", got, want)
	}
}

// TestGuestInitTrimsAndSkipsBlank pins the forgiving parse: whitespace
// around an address is trimmed and a trailing empty entry is dropped, so
// a hand-edited value still produces one line per real resolver.
func TestGuestInitTrimsAndSkipsBlank(t *testing.T) {
	got := runGuestInit(t, "10.1.0.2, 10.1.0.3 ,")
	want := "nameserver 10.1.0.2\nnameserver 10.1.0.3\noptions timeout:2 attempts:3 rotate\n"
	if got != want {
		t.Fatalf("resolv.conf =\n%q\nwant\n%q", got, want)
	}
}
