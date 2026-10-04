package fake

import (
	"errors"
	"testing"

	"github.com/jrimmer/spoond/v2/substrate"
)

// newFileSandbox returns a Fake with one running sandbox.
func newFileSandbox(t *testing.T) (*Fake, string) {
	t.Helper()
	f := New()
	id := "i0123456789abcdefghij"
	if _, err := f.Create(t.Context(), substrate.CreateRequest{SandboxID: id, TemplateID: "template0000000000000"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return f, id
}

func TestWriteReadStatRoundTrip(t *testing.T) {
	f, id := newFileSandbox(t)

	if err := f.WriteFile(t.Context(), id, "/home/u/hello.txt", []byte("hi"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := f.ReadFile(t.Context(), id, "/home/u/hello.txt", 1<<20)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("ReadFile = %q, want %q", got, "hi")
	}

	info, err := f.Stat(t.Context(), id, "/home/u/hello.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Name != "hello.txt" || info.Size != 2 || info.IsDir {
		t.Fatalf("Stat = %+v", info)
	}
	if info.Mode.Perm() != 0o600 {
		t.Fatalf("Stat mode = %o, want 600", info.Mode.Perm())
	}
	if info.ModTime.IsZero() {
		t.Fatal("Stat ModTime is zero")
	}
}

func TestWriteCreatesParentsAndKeepsTheirModes(t *testing.T) {
	f, id := newFileSandbox(t)

	// Parents are created implicitly; the file's own mode is kept.
	if err := f.WriteFile(t.Context(), id, "/a/b/c/f.txt", []byte("x"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for _, dir := range []string{"/a", "/a/b", "/a/b/c"} {
		info, err := f.Stat(t.Context(), id, dir)
		if err != nil {
			t.Fatalf("Stat %s: %v", dir, err)
		}
		if !info.IsDir {
			t.Fatalf("Stat %s: not a directory", dir)
		}
	}
	if info, _ := f.Stat(t.Context(), id, "/a/b/c/f.txt"); info.Mode.Perm() != 0o640 {
		t.Fatalf("file mode = %o, want 640", info.Mode.Perm())
	}

	// A MakeDir after the fact does not reset an existing parent's mode.
	if err := f.MakeDir(t.Context(), id, "/a", 0o700); err != nil {
		t.Fatalf("MakeDir /a: %v", err)
	}
	if info, _ := f.Stat(t.Context(), id, "/a"); info.Mode.Perm() != 0o755 {
		t.Fatalf("MakeDir reset an existing dir's mode: %o", info.Mode.Perm())
	}

	// MakeDir creates missing parents with the requested mode.
	if err := f.MakeDir(t.Context(), id, "/x/y/z", 0o750); err != nil {
		t.Fatalf("MakeDir /x/y/z: %v", err)
	}
	for _, dir := range []string{"/x", "/x/y", "/x/y/z"} {
		info, err := f.Stat(t.Context(), id, dir)
		if err != nil {
			t.Fatalf("Stat %s: %v", dir, err)
		}
		if info.Mode.Perm() != 0o750 {
			t.Fatalf("Stat %s mode = %o, want 750", dir, info.Mode.Perm())
		}
	}
}

func TestWriteOverwritesAndTruncates(t *testing.T) {
	f, id := newFileSandbox(t)

	if err := f.WriteFile(t.Context(), id, "/f", []byte("longer content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := f.WriteFile(t.Context(), id, "/f", []byte("short"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := f.ReadFile(t.Context(), id, "/f", 1<<20)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "short" {
		t.Fatalf("ReadFile = %q, want %q", got, "short")
	}
	info, _ := f.Stat(t.Context(), id, "/f")
	if info.Size != 5 {
		t.Fatalf("Stat size = %d, want 5", info.Size)
	}
}

func TestFileErrors(t *testing.T) {
	f, id := newFileSandbox(t)

	if err := f.WriteFile(t.Context(), id, "/dir/f", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Missing paths are ErrNotFound everywhere.
	if _, err := f.ReadFile(t.Context(), id, "/nope", 1<<20); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("ReadFile missing: %v", err)
	}
	if _, err := f.Stat(t.Context(), id, "/nope"); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Stat missing: %v", err)
	}
	if err := f.Remove(t.Context(), id, "/nope", false); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Remove missing: %v", err)
	}

	// A parent that is a file.
	if _, err := f.Stat(t.Context(), id, "/dir/f/sub"); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Stat under a file: %v", err)
	}
	if err := f.WriteFile(t.Context(), id, "/dir/f/sub", []byte("x"), 0o644); !errors.Is(err, substrate.ErrNotDir) {
		t.Fatalf("WriteFile under a file: %v", err)
	}
	if err := f.MakeDir(t.Context(), id, "/dir/f/sub", 0o755); !errors.Is(err, substrate.ErrNotDir) {
		t.Fatalf("MakeDir under a file: %v", err)
	}

	// Reading a directory.
	if _, err := f.ReadFile(t.Context(), id, "/dir", 1<<20); !errors.Is(err, substrate.ErrInvalidOp) {
		t.Fatalf("ReadFile on a dir: %v", err)
	}

	// Over-size reads.
	if _, err := f.ReadFile(t.Context(), id, "/dir/f", 0); !errors.Is(err, substrate.ErrTooLarge) {
		t.Fatalf("ReadFile max 0: %v", err)
	}
	if got, err := f.ReadFile(t.Context(), id, "/dir/f", 1); err != nil || string(got) != "x" {
		t.Fatalf("ReadFile max 1 = %q, %v", got, err)
	}
}

func TestRemoveSemantics(t *testing.T) {
	f, id := newFileSandbox(t)

	if err := f.WriteFile(t.Context(), id, "/d/e/f", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Non-recursive remove of a non-empty directory refuses.
	if err := f.Remove(t.Context(), id, "/d", false); !errors.Is(err, substrate.ErrNotEmpty) {
		t.Fatalf("Remove non-recursive non-empty: %v", err)
	}
	if _, err := f.Stat(t.Context(), id, "/d/e/f"); err != nil {
		t.Fatalf("contents must survive: %v", err)
	}

	// Files and empty directories remove non-recursively.
	if err := f.Remove(t.Context(), id, "/d/e/f", false); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if err := f.Remove(t.Context(), id, "/d/e", false); err != nil {
		t.Fatalf("Remove empty dir: %v", err)
	}

	// Recursive removes a populated tree.
	if err := f.WriteFile(t.Context(), id, "/d/e/g", []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := f.Remove(t.Context(), id, "/d", true); err != nil {
		t.Fatalf("Remove recursive: %v", err)
	}
	if _, err := f.Stat(t.Context(), id, "/d"); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Stat after recursive remove: %v", err)
	}
}

func TestFilesDieWithTheSandbox(t *testing.T) {
	f, id := newFileSandbox(t)

	if err := f.WriteFile(t.Context(), id, "/f", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := f.Delete(t.Context(), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.ReadFile(t.Context(), id, "/f", 1<<20); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("ReadFile after Delete: %v", err)
	}
	if _, err := f.Stat(t.Context(), id, "/f"); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Stat after Delete: %v", err)
	}

	// An unknown sandbox is ErrNotFound for every operation.
	if err := f.WriteFile(t.Context(), id, "/f", []byte("x"), 0o644); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("WriteFile unknown sandbox: %v", err)
	}
	if err := f.MakeDir(t.Context(), id, "/d", 0o755); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("MakeDir unknown sandbox: %v", err)
	}
	if err := f.Remove(t.Context(), id, "/f", true); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Remove unknown sandbox: %v", err)
	}
}

func TestModeZeroDefaults(t *testing.T) {
	f, id := newFileSandbox(t)

	if err := f.WriteFile(t.Context(), id, "/f", []byte("x"), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if info, _ := f.Stat(t.Context(), id, "/f"); info.Mode.Perm() != 0o644 {
		t.Fatalf("file mode = %o, want the 644 default", info.Mode.Perm())
	}
	if err := f.MakeDir(t.Context(), id, "/d", 0); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if info, _ := f.Stat(t.Context(), id, "/d"); info.Mode.Perm() != 0o755 {
		t.Fatalf("dir mode = %o, want the 755 default", info.Mode.Perm())
	}
}
