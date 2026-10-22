// The spoond phase's item 9: copying the running binary to
// /opt/spoond/spoond, through a small hook so tests pass a fake
// executable and assert on the copy without a real one.
package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// execPath and copyExec are the binary-copy hooks (item 9): the source
// is the running executable (os.Executable) and the copy goes at 0755.
// Both are package vars so a test passes a fake executable path and
// asserts on the copy without a real one.
var (
	// execPath is the running executable's path.
	execPath = os.Executable

	// copyExec copies the running binary to dst at mode 0755 and
	// answers "this copy changed the target".
	copyExec = func(dst string) (bool, error) {
		src, err := execPath()
		if err != nil {
			return false, fmt.Errorf("finding the running binary: %w", err)
		}
		return spoondCopyFile(src, dst, modeBin)
	}
)

// modeBin is the installed binary's mode (item 9: 0755).
const modeBin = 0o755

// copyBinary installs the running binary at /opt/spoond/spoond (item 9)
// unless it is already byte-identical (sha256), and adds the row. A dry
// run reports the row it would write after a same-bytes check and runs
// no copy (a copy is a changing action, exec-shaped here).
func copyBinary(env *Env, r *Reporter) (bool, error) {
	dst := env.Resolve(spoondBinPath)
	exe, err := execPath()
	if err != nil {
		return false, fmt.Errorf("finding the running binary: %w", err)
	}
	dstSHA, srcSHA := fileSHA256(dst), fileSHA256(exe)
	if dstSHA != "" && dstSHA == srcSHA {
		r.Add(Kept, spoondBinPath, "already the running binary")
		return false, nil
	}
	if env.DryRun {
		r.Add(Changed, spoondBinPath, "dry-run: the running binary would be copied here")
		return true, nil
	}
	changed, err := copyExec(dst)
	if err != nil {
		r.Add(Failed, spoondBinPath, err.Error())
		return false, err
	}
	if changed {
		r.Add(Changed, spoondBinPath, "copied the running binary (sha256 updated)")
	} else {
		r.Add(Kept, spoondBinPath, "copied, same bytes")
	}
	return changed, nil
}

// spoondCopyFile copies src to dst (mode through chmod: WriteFile keeps
// an existing file's mode) and answers whether dst is now different —
// true when it did not exist or the bytes changed. The comparison reads
// both files fully; a spoond binary is tens of MB, once per install.
func spoondCopyFile(src, dst string, mode os.FileMode) (bool, error) {
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", src, err)
	}
	old, err := os.ReadFile(dst)
	changed := true
	switch {
	case err == nil:
		changed = !bytes.Equal(old, srcBytes) // same-byte rerun
	case !os.IsNotExist(err):
		return false, fmt.Errorf("read %s: %w", dst, err)
	}
	if err := os.MkdirAll(dirname(dst), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", dirname(dst), err)
	}
	if err := writeAtomic(dst, srcBytes, mode); err != nil {
		return false, err
	}
	return changed, nil
}

// dirname is filepath.Dir with a short name here.
func dirname(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

// fileSHA256 is the binary comparison (item 9): the file's sha256 as
// lowercase hex, "" when it cannot be read (absent target, or an
// unreadable source — the copy itself then reports the read failure).
func fileSHA256(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
