package e2b

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b/gen/envd/filesystem"
)

func TestWriteFileRequestShape(t *testing.T) {
	e := &filesTestEnvd{}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.WriteFile(context.Background(), filesSandboxID, "/home/u/f.txt", []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// One upload, multipart with the part named "file" and the target path
	// in the query.
	if len(e.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(e.uploads))
	}
	up := e.uploads[0]
	if up.method != http.MethodPost {
		t.Errorf("upload method = %s, want POST", up.method)
	}
	if up.path != "/home/u/f.txt" {
		t.Errorf("upload path = %q, want /home/u/f.txt", up.path)
	}
	if string(up.file) != "hello" {
		t.Errorf("uploaded content = %q, want hello", up.file)
	}

	// Routing and auth match the exec path's headers.
	if got, want := up.headers.Get("E2b-Sandbox-Id"), filesSandboxID; got != want {
		t.Errorf("E2b-Sandbox-Id = %q, want %q", got, want)
	}
	if got, want := up.headers.Get("E2b-Sandbox-Port"), strconv.Itoa(envdPort); got != want {
		t.Errorf("E2b-Sandbox-Port = %q, want %q", got, want)
	}
	if got, want := up.headers.Get("X-Access-Token"), c.EnvdToken(filesSandboxID); got != want {
		t.Errorf("X-Access-Token = %q, want %q", got, want)
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("root:"))
	if got := up.headers.Get("Authorization"); got != auth {
		t.Errorf("Authorization = %q, want %q", got, auth)
	}

	// The file is created empty with its mode (and parents) before the
	// upload, and the mode is re-applied with a chmod afterwards. Both are
	// argv execs, no guest shell involved.
	if len(e.execs) != 2 {
		t.Fatalf("execs = %v, want an install and a chmod", e.execs)
	}
	if want := "/usr/bin/install -D -m 600 -- /dev/null /home/u/f.txt"; e.execs[0] != want {
		t.Errorf("first exec = %q, want %q (the pre-create)", e.execs[0], want)
	}
	if e.execs[1] != "/bin/chmod 600 -- /home/u/f.txt" {
		t.Errorf("second exec = %q, want the 600 chmod of the file", e.execs[1])
	}
}

func TestWriteFileRootPath(t *testing.T) {
	e := &filesTestEnvd{}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.WriteFile(context.Background(), filesSandboxID, "/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(e.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(e.uploads))
	}
	if got, want := e.uploads[0].path, "/f.txt"; got != want {
		t.Errorf("upload path = %q, want %q", got, want)
	}
	if len(e.execs) != 2 || !strings.HasPrefix(e.execs[0], "/usr/bin/install") || !strings.HasPrefix(e.execs[1], "/bin/chmod") {
		t.Errorf("execs = %v, want an install and a chmod", e.execs)
	}
}

func TestWriteFileUploadFails(t *testing.T) {
	// Point the client at a closed port: the upload fails and the sandbox is
	// not listed (no orchestrator), so the error wraps ErrNotFound.
	c := newFilesClient(t, "http://127.0.0.1:1")
	err := c.WriteFile(context.Background(), filesSandboxID, "/f", []byte("x"), 0o644)
	if !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("WriteFile on a dead sandbox: %v", err)
	}
}

func TestReadFileRequestShape(t *testing.T) {
	e := &filesTestEnvd{fileData: []byte("file-bytes")}
	c := newFilesEnvd(t, e)
	defer e.close()

	data, err := c.ReadFile(context.Background(), filesSandboxID, "/home/u/f.txt", 1<<20)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "file-bytes" {
		t.Fatalf("ReadFile = %q, want file-bytes", data)
	}
	if len(e.uploads) != 1 {
		t.Fatalf("GETs = %d, want 1", len(e.uploads))
	}
	get := e.uploads[0]
	if get.method != http.MethodGet {
		t.Errorf("download method = %s, want GET", get.method)
	}
	if got, want := get.path, "/home/u/f.txt"; got != want {
		t.Errorf("download path = %q, want %q", got, want)
	}
	if got, want := get.headers.Get("X-Access-Token"), c.EnvdToken(filesSandboxID); got != want {
		t.Errorf("X-Access-Token = %q, want %q", got, want)
	}
	user, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(get.headers.Get("Authorization"), "Basic "))
	if err != nil || string(user) != "root:" {
		t.Errorf("Basic user = %q (%v), want root:", user, err)
	}
}

func TestReadFileLimit(t *testing.T) {
	e := &filesTestEnvd{fileData: []byte("0123456789")}
	c := newFilesEnvd(t, e)
	defer e.close()

	ctx := context.Background()
	data, err := c.ReadFile(ctx, filesSandboxID, "/f", 10)
	if err != nil || string(data) != "0123456789" {
		t.Fatalf("ReadFile at the limit = %q, %v", data, err)
	}
	if _, err := c.ReadFile(ctx, filesSandboxID, "/f", 9); !errors.Is(err, substrate.ErrTooLarge) {
		t.Fatalf("ReadFile over the limit: %v", err)
	}
}

func TestReadFileNotFound(t *testing.T) {
	e := &filesTestEnvd{fileErr: true}
	c := newFilesEnvd(t, e)
	defer e.close()

	if _, err := c.ReadFile(context.Background(), filesSandboxID, "/f", 10); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("ReadFile missing: %v", err)
	}
}

func TestStatRequestShape(t *testing.T) {
	e := &filesTestEnvd{statEntry: &filesystem.EntryInfo{
		Name:         "f.txt",
		Type:         filesystem.FileType_FILE_TYPE_FILE,
		Path:         "/home/u/f.txt",
		Size:         3,
		Mode:         0o600,
		ModifiedTime: timestamppb.New(time.Unix(1700000000, 0).UTC()),
	}}
	c := newFilesEnvd(t, e)
	defer e.close()

	info, err := c.Stat(context.Background(), filesSandboxID, "/home/u/f.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if len(e.statReqs) != 1 || e.statReqs[0] != "/home/u/f.txt" {
		t.Errorf("Stat request paths = %v, want [/home/u/f.txt]", e.statReqs)
	}
	if info.Name != "f.txt" || info.Size != 3 || info.IsDir {
		t.Errorf("Stat = %+v", info)
	}
	if got, want := info.Mode.Perm(), os.FileMode(0o600); got != want {
		t.Errorf("Stat mode = %o, want %o", got, want)
	}
	if got, want := info.ModTime.Unix(), int64(1700000000); got != want {
		t.Errorf("Stat ModTime = %d, want %d", got, want)
	}
}

func TestStatDir(t *testing.T) {
	e := &filesTestEnvd{statEntry: &filesystem.EntryInfo{
		Name: "d", Type: filesystem.FileType_FILE_TYPE_DIRECTORY, Mode: 0o755,
	}}
	c := newFilesEnvd(t, e)
	defer e.close()

	info, err := c.Stat(context.Background(), filesSandboxID, "/d")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir || info.Mode&os.ModeDir == 0 {
		t.Fatalf("Stat = %+v, want a directory", info)
	}
}

func TestStatNotFound(t *testing.T) {
	e := &filesTestEnvd{} // no statEntry: the handler answers CodeNotFound
	c := newFilesEnvd(t, e)
	defer e.close()

	if _, err := c.Stat(context.Background(), filesSandboxID, "/nope"); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Stat missing: %v", err)
	}
}

func TestMakeDirRequestShape(t *testing.T) {
	e := &filesTestEnvd{}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.MakeDir(context.Background(), filesSandboxID, "/a/b/z", 0o750); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if len(e.makeDirs) != 1 || e.makeDirs[0] != "/a/b/z" {
		t.Errorf("MakeDir requests = %v, want [/a/b/z]", e.makeDirs)
	}
	// envd sets no mode, so the mode lands with a chmod exec.
	if len(e.execs) != 1 || e.execs[0] != "/bin/chmod 750 -- /a/b/z" {
		t.Errorf("execs = %v, want [/bin/chmod 750 -- /a/b/z]", e.execs)
	}
}

func TestMakeDirNotFound(t *testing.T) {
	e := &filesTestEnvd{protoErr: connect.CodeNotFound}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.MakeDir(context.Background(), filesSandboxID, "/d", 0o755); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("MakeDir missing: %v", err)
	}
}

func TestRemoveRecursiveShape(t *testing.T) {
	e := &filesTestEnvd{statEntry: &filesystem.EntryInfo{
		Name: "dir", Type: filesystem.FileType_FILE_TYPE_DIRECTORY,
	}}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.Remove(context.Background(), filesSandboxID, "/dir", true); err != nil {
		t.Fatalf("Remove recursive: %v", err)
	}
	if len(e.removed) != 1 || e.removed[0] != "/dir" {
		t.Errorf("Remove requests = %v, want [/dir]", e.removed)
	}
	if len(e.execs) != 0 {
		t.Errorf("recursive remove must not exec, got %v", e.execs)
	}
}

func TestRemoveNonRecursiveShape(t *testing.T) {
	e := &filesTestEnvd{statEntry: &filesystem.EntryInfo{
		Name: "dir", Type: filesystem.FileType_FILE_TYPE_DIRECTORY,
	}}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.Remove(context.Background(), filesSandboxID, "/dir", false); err != nil {
		t.Fatalf("Remove non-recursive: %v", err)
	}
	// rmdir refuses non-empty directories, so recursive=false never destroys
	// contents; the Connect Remove is reserved for recursive=true.
	if len(e.removed) != 0 {
		t.Errorf("non-recursive Remove hit the Connect client: %v", e.removed)
	}
	if len(e.execs) != 1 || e.execs[0] != "/bin/rmdir -- /dir" {
		t.Errorf("execs = %v, want [/bin/rmdir -- /dir]", e.execs)
	}
}

func TestRemoveNonRecursiveFile(t *testing.T) {
	e := &filesTestEnvd{statEntry: &filesystem.EntryInfo{
		Name: "f.txt", Type: filesystem.FileType_FILE_TYPE_FILE,
	}}
	c := newFilesEnvd(t, e)
	defer e.close()

	// Files remove non-recursively through the Connect client, no rmdir.
	if err := c.Remove(context.Background(), filesSandboxID, "/f.txt", false); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if len(e.removed) != 1 || e.removed[0] != "/f.txt" {
		t.Errorf("Remove requests = %v, want [/f.txt]", e.removed)
	}
	if len(e.execs) != 0 {
		t.Errorf("file remove must not exec, got %v", e.execs)
	}
}

func TestRemoveMissingStatIsNotFound(t *testing.T) {
	e := &filesTestEnvd{} // Stat answers CodeNotFound
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.Remove(context.Background(), filesSandboxID, "/nope", false); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Remove missing non-recursive: %v", err)
	}
}

func TestRemoveNotFound(t *testing.T) {
	e := &filesTestEnvd{protoErr: connect.CodeNotFound}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.Remove(context.Background(), filesSandboxID, "/nope", true); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Remove missing: %v", err)
	}
}

func TestReadFileLimitOverflow(t *testing.T) {
	e := &filesTestEnvd{fileData: []byte("x")}
	c := newFilesEnvd(t, e)
	defer e.close()

	// A max of MaxInt64 must not overflow the +1 for the over-size check.
	if data, err := c.ReadFile(context.Background(), filesSandboxID, "/f", math.MaxInt64); err != nil || string(data) != "x" {
		t.Fatalf("ReadFile max MaxInt64 = %q, %v", data, err)
	}
}

// TestExecPassesEnvNotArgv pins that Exec hands req.Env to the process
// config's Envs, so envd sets the process environment and the values
// never appear in the command line.
func TestExecPassesEnvNotArgv(t *testing.T) {
	e := &filesTestEnvd{}
	c := newFilesEnvd(t, e)
	defer e.close()

	res, err := c.Exec(context.Background(), filesSandboxID, substrate.ExecRequest{
		Args: []string{"/bin/bash", "-c", `echo hi`},
		Env:  map[string]string{"FOO": "env-secret-value"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	if len(e.execs) != 1 {
		t.Fatalf("execs = %v, want 1", e.execs)
	}
	if len(e.execEnvs) != 1 || e.execEnvs[0]["FOO"] != "env-secret-value" {
		t.Fatalf("process envs = %v, want FOO=env-secret-value", e.execEnvs)
	}
	if strings.Contains(e.execs[0], "FOO") || strings.Contains(e.execs[0], "env-secret-value") {
		t.Fatalf("env leaked into argv: %q", e.execs[0])
	}
}
