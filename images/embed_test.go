// Tests for the embedded guest pieces (embed.go): guest/ rides in the
// binary, nothing else does — no image definitions, and not README.md
// (operator docs).
package images

import (
	"io/fs"
	"testing"
)

func TestContextHasGuestInit(t *testing.T) {
	b, err := fs.ReadFile(Context, "guest/spoond-guest-init")
	if err != nil {
		t.Fatalf("Context: guest/spoond-guest-init: %v", err)
	}
	if len(b) == 0 {
		t.Error("Context: guest/spoond-guest-init is empty")
	}
}

// TestContextShipsNoCatalog guards the bare install: spoond ships no
// image definitions, only the guest pieces.
func TestContextShipsNoCatalog(t *testing.T) {
	entries, err := fs.ReadDir(Context, ".")
	if err != nil {
		t.Fatalf("read Context: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "guest" {
			t.Errorf("Context has %s; only guest/ ships", e.Name())
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
