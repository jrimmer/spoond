package store

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// BuildDiskUsage sums the allocated size (stat blocks × 512) of every
// regular file under dir — a build directory's disk footprint, the
// number stored in size_bytes. A missing directory measures 0 with no
// error (an unwritten build is empty, not failed); any read or stat
// failure — the root itself or one file's Info — is returned together
// with the partial total, so callers can tell a failed measurement from
// a legitimately 0-byte build and must not store the partial number
// (the write-time path stores 0 instead, #125).
func BuildDiskUsage(dir string) (int64, error) {
	var total int64
	var firstErr error
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			switch {
			case path == dir && os.IsNotExist(err):
			case firstErr == nil:
				firstErr = fmt.Errorf("stat %s: %w", path, err)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("stat %s: %w", path, err)
			}
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += int64(st.Blocks) * 512
		}
		return nil
	})
	return total, firstErr
}
