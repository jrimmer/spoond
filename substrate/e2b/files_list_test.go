package e2b

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/e2b/gen/envd/filesystem"
)

func TestListDirRequestShape(t *testing.T) {
	e := &filesTestEnvd{listEntries: []*filesystem.EntryInfo{
		{Name: "b", Type: filesystem.FileType_FILE_TYPE_FILE},
		{Name: "a", Type: filesystem.FileType_FILE_TYPE_DIRECTORY},
		{Name: "l", Type: filesystem.FileType_FILE_TYPE_SYMLINK},
	}}
	c := newFilesEnvd(t, e)
	defer e.close()

	entries, err := c.ListDir(context.Background(), filesSandboxID, "/home/u")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	// Depth is 1: the walk host-side expands directories, so envd must
	// never go deeper.
	if len(e.listReqs) != 1 || e.listReqs[0].Path != "/home/u" || e.listReqs[0].Depth != 1 {
		t.Fatalf("ListDir requests = %+v, want one depth-1 request for /home/u", e.listReqs)
	}
	// envd's order is kept where kinds are equal... the caller sorts; here
	// pin the mapping only (order is the walk's job).
	if len(entries) != 3 {
		t.Fatalf("entries = %v, want three", entries)
	}
	kinds := map[string]string{}
	for _, en := range entries {
		kinds[en.Name] = en.Kind
	}
	if kinds["b"] != substrate.DirFile || kinds["a"] != substrate.DirDirectory || kinds["l"] != substrate.DirSymlink {
		t.Fatalf("kinds = %v, want f/d/l", kinds)
	}
}

func TestListDirSymlinkToFileIsSymlink(t *testing.T) {
	e := &filesTestEnvd{listEntries: []*filesystem.EntryInfo{{
		Name: "ln", Type: filesystem.FileType_FILE_TYPE_FILE, IsSymlink: true,
	}}}
	c := newFilesEnvd(t, e)
	defer e.close()

	entries, err := c.ListDir(context.Background(), filesSandboxID, "/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != substrate.DirSymlink {
		t.Fatalf("entries = %v, want one symlink", entries)
	}
}

// TestListDirSymlinkToDirectoryIsSymlink: envd sets is_symlink with type
// naming the TARGET, so a symlink to a directory arrives as
// FILE_TYPE_DIRECTORY + is_symlink. It must list as a symlink, not a
// directory (the walk would descend into the target otherwise).
func TestListDirSymlinkToDirectoryIsSymlink(t *testing.T) {
	e := &filesTestEnvd{listEntries: []*filesystem.EntryInfo{{
		Name: "dln", Type: filesystem.FileType_FILE_TYPE_DIRECTORY, IsSymlink: true,
	}}}
	c := newFilesEnvd(t, e)
	defer e.close()

	entries, err := c.ListDir(context.Background(), filesSandboxID, "/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0] != (substrate.DirEntry{Name: "dln", Kind: substrate.DirSymlink}) {
		t.Fatalf("entries = %v, want one symlink (is_symlink wins over a target's type)", entries)
	}
}

func TestListDirNotFound(t *testing.T) {
	e := &filesTestEnvd{listErr: connect.CodeNotFound}
	c := newFilesEnvd(t, e)
	defer e.close()

	if _, err := c.ListDir(context.Background(), filesSandboxID, "/nope"); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("ListDir missing: %v, want ErrNotFound", err)
	}
}

func TestListDirNotADirectory(t *testing.T) {
	e := &filesTestEnvd{listErr: connect.CodeFailedPrecondition, listMsg: "stat /f.txt: not a directory"}
	c := newFilesEnvd(t, e)
	defer e.close()

	if _, err := c.ListDir(context.Background(), filesSandboxID, "/f.txt"); !errors.Is(err, substrate.ErrNotDir) {
		t.Fatalf("ListDir on a file: %v, want ErrNotDir", err)
	}
}

// TestListDirPermissionDenied: an unreadable directory wraps
// substrate.ErrPermission, the sentinel the files routes answer with 403
// EACCES, through the same Connect mapping every file call uses.
func TestListDirPermissionDenied(t *testing.T) {
	e := &filesTestEnvd{listErr: connect.CodePermissionDenied}
	c := newFilesEnvd(t, e)
	defer e.close()

	if _, err := c.ListDir(context.Background(), filesSandboxID, "/locked"); !errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("ListDir unreadable: %v, want ErrPermission", err)
	}
}
