package dirwalk

import (
	"context"
	"errors"
	"path"

	"github.com/jrimmer/spoond/v3/substrate"
)

// Substrate adapts a substrate.Substrate's file methods (ListDir, Stat,
// ReadFile) to the walk's readFS, depth 1 at a time.
type Substrate struct {
	Sub substrate.Substrate
	ID  string // the sandbox id
}

// List lists one directory level. A missing directory or a file in its
// place is a failure (err carries it; the API 404s / 409s). The walk
// skips such a failure below its root (a directory that vanished or
// refuses a listing is not fatal there), so the error is passed
// through with its substrate sentinel intact.
func (s Substrate) List(ctx context.Context, dir string) ([]substrate.DirEntry, bool, error) {
	entries, err := s.Sub.ListDir(ctx, s.ID, dir)
	if err != nil {
		return nil, false, err
	}
	return entries, true, nil
}

// StatStats returns the path's metadata through Stat, for the API to
// refuse a file with 409 before the walk.
func (s Substrate) Stat(ctx context.Context, fpath string) (substrate.FileInfo, error) {
	return s.Sub.Stat(ctx, s.ID, fpath)
}

// ReadFile reads an ignore file; a missing file (or one a file sits in
// the way of, or one over the size cap) maps to errIgnoreMissing. A
// permission refusal surfaces as substrate.ErrPermission, which the
// walk skips below its root.
func (s Substrate) ReadFile(ctx context.Context, fpath string, max int64) ([]byte, error) {
	data, err := s.Sub.ReadFile(ctx, s.ID, fpath, max)
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			return nil, errIgnoreMissing
		}
		if errors.Is(err, substrate.ErrNotDir) {
			// /f.txt/.gitignore: a file where a directory was wanted.
			return nil, errIgnoreMissing
		}
		if errors.Is(err, substrate.ErrTooLarge) {
			// An over-size ignore file is skipped, not fatal.
			return nil, errIgnoreMissing
		}
		return nil, err
	}
	return data, nil
}

// ProbeGit reports whether dir holds a .git and whether it names a
// directory: Stat is one call, no List (ancestors of the listed path
// must not be listed). Only a directory makes a work tree; a .git
// file (the exotic gitfile form) confirms nothing.
func (s Substrate) ProbeGit(ctx context.Context, dir string) (found bool, isDir bool, err error) {
	info, err := s.Sub.Stat(ctx, s.ID, path.Join(dir, ".git"))
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			return false, false, nil
		}
		if errors.Is(err, substrate.ErrNotDir) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, info.IsDir, nil
}
