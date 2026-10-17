// Tests for the embedded build context (embed.go): the files the
// image builds need ride in the binary — the manifest, the two base
// dockerfiles the minimal catalog builds, and the guest scripts — and
// README.md (operator docs) does not.
package images

import (
	"io/fs"
	"testing"
)

func TestContextHasBuildFiles(t *testing.T) {
	for _, name := range []string{
		"manifest.yaml",
		"py-base.dockerfile",
		"go-base.dockerfile",
	} {
		b, err := fs.ReadFile(Context, name)
		if err != nil {
			t.Errorf("Context: %s: %v", name, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("Context: %s is empty", name)
		}
	}
}

func TestContextShipsGuestRecursively(t *testing.T) {
	var files []string
	err := fs.WalkDir(Context, "guest", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk guest/: %v", err)
	}
	if len(files) < 1 {
		t.Error("Context has no files under guest/, want at least one")
	}
}

func TestContextLeavesREADMEOut(t *testing.T) {
	if _, err := fs.Stat(Context, "README.md"); err == nil {
		t.Error("README.md is embedded; it is operator docs, not build context")
	}
}
