package e2b

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jrimmer/spoond/v3/substrate"
	"github.com/jrimmer/spoond/v3/substrate/e2b/gen/envd/filesystem"
)

func TestWriteFileRequestShape(t *testing.T) {
	e := &filesTestEnvd{}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.WriteFile(context.Background(), filesSandboxID, "/home/u/f.txt", []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// One upload, multipart with the part named "file", to a fresh name in
	// a private directory beside the target (never the target itself: envd
	// would reopen an existing file there, which protected_regular refuses
	// in a sticky directory).
	if len(e.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(e.uploads))
	}
	up := e.uploads[0]
	if up.method != http.MethodPost {
		t.Errorf("upload method = %s, want POST", up.method)
	}
	tmpDir := strings.TrimSuffix(up.path, "/f.txt")
	if !strings.HasPrefix(tmpDir, "/home/u/.spoond-upload-") || tmpDir == up.path || len(tmpDir) != len("/home/u/.spoond-upload-")+16 {
		t.Errorf("upload path = %q, want /home/u/.spoond-upload-<16 hex>/f.txt", up.path)
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

	// Before the upload one exec stages the parents and the 0700 upload
	// directory; after it one exec applies the mode and renames the file
	// into place. The paths travel as positional arguments to a constant
	// script, never as shell code.
	if len(e.execs) != 2 {
		t.Fatalf("execs = %v, want a stage and a place", e.execs)
	}
	if want := "/bin/sh -c " + writeStageScript + " sh /home/u/f.txt /home/u " + tmpDir; e.execs[0] != want {
		t.Errorf("first exec = %q, want %q (the stage)", e.execs[0], want)
	}
	if want := "/bin/sh -c " + writePlaceScript + " sh 600 " + up.path + " /home/u/f.txt " + tmpDir; e.execs[1] != want {
		t.Errorf("second exec = %q, want %q (the place)", e.execs[1], want)
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
	if got := e.uploads[0].path; !strings.HasPrefix(got, "/.spoond-upload-") || !strings.HasSuffix(got, "/f.txt") {
		t.Errorf("upload path = %q, want /.spoond-upload-<hex>/f.txt", got)
	}
	if len(e.execs) != 2 || !strings.Contains(e.execs[0], " sh /f.txt / /.spoond-upload-") || !strings.Contains(e.execs[1], " sh 644 /.spoond-upload-") {
		t.Errorf("execs = %v, want a stage and a place for /f.txt", e.execs)
	}
}

func TestWriteFileUploadFails(t *testing.T) {
	// Point the client at a closed port: the upload fails and the
	// orchestrator is unreachable, so the failure is unknown rather than a
	// confirmed absence. It must wrap ErrUnavailable (retryable) and never
	// ErrNotFound (which the API answers as 410 lease_lost).
	c := newFilesClient(t, "http://127.0.0.1:1")
	err := c.WriteFile(context.Background(), filesSandboxID, "/f", []byte("x"), 0o644)
	if !errors.Is(err, substrate.ErrUnavailable) {
		t.Fatalf("WriteFile on an unreachable orchestrator: %v, want ErrUnavailable", err)
	}
	if errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("WriteFile on an unreachable orchestrator must not be ErrNotFound: %v", err)
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

// TestReadFileDirectoryIsEnvdInvalidPath pins the e2b read of a
// directory: envd's GET /files answers 400 "path … is a directory" (the
// A2 spec's InvalidPath example), and the client maps it to ErrIsDir
// (409 EISDIR at the files API), never a plain unexpected-status failure
// (502 EIO). Range reads share the switch.
func TestReadFileDirectoryIsEnvdInvalidPath(t *testing.T) {
	e := &filesTestEnvd{fileStatus: http.StatusBadRequest, fileStatusMsg: "{\"message\":\"path '/d' is a directory\",\"code\":400\"}"}
	c := newFilesEnvd(t, e)
	defer e.close()

	_, err := c.ReadFile(context.Background(), filesSandboxID, "/d", 10)
	if !errors.Is(err, substrate.ErrIsDir) {
		t.Fatalf("ReadFile of a directory = %v, want ErrIsDir", err)
	}
	if _, err = c.ReadFileRange(context.Background(), filesSandboxID, "/d", 0, 10); !errors.Is(err, substrate.ErrIsDir) {
		t.Fatalf("ReadFileRange of a directory = %v, want ErrIsDir", err)
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

func TestWriteFilePathSplit(t *testing.T) {
	for _, tc := range []struct{ path, dir, base string }{
		{"/f", "/", "f"},
		{"/tmp/x.txt", "/tmp", "x.txt"},
		{"/a/b/c", "/a/b", "c"},
		{"/a/", "/a", ""},
	} {
		if d, b := splitPath(tc.path); d != tc.dir || b != tc.base {
			t.Errorf("splitPath(%q) = %q, %q; want %q, %q", tc.path, d, b, tc.dir, tc.base)
		}
	}
	if got := joinPath("/", "x"); got != "/x" {
		t.Errorf(`joinPath("/", "x") = %q`, got)
	}
}

func TestWriteFileRefusesDirectoryPath(t *testing.T) {
	e := &filesTestEnvd{}
	c := newFilesEnvd(t, e)
	defer e.close()
	err := c.WriteFile(context.Background(), filesSandboxID, "/tmp/dir/", []byte("x"), 0o644)
	if !errors.Is(err, substrate.ErrInvalidOp) {
		t.Fatalf("WriteFile to a trailing-slash path = %v, want ErrInvalidOp", err)
	}
	if len(e.execs) != 0 || len(e.uploads) != 0 {
		t.Fatalf("a refused path touched the guest: execs %v, uploads %d", e.execs, len(e.uploads))
	}
}

// TestWriteFilePlaceErrorDiscards: when the final rename fails, the
// upload directory is removed rather than left holding the content.
func TestWriteFilePlaceErrorDiscards(t *testing.T) {
	e := &filesTestEnvd{failPlace: true}
	c := newFilesEnvd(t, e)
	defer e.close()
	if err := c.WriteFile(context.Background(), filesSandboxID, "/tmp/x.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile with a failing place succeeded")
	}
	tmpDir := strings.TrimSuffix(e.uploads[0].path, "/x.txt")
	if want := "/bin/rm -rf -- " + tmpDir; len(e.execs) != 3 || e.execs[2] != want {
		t.Fatalf("execs = %v, want stage, place, then %q", e.execs, want)
	}
}

// TestWriteFileUploadErrorGoneSandbox: a failed upload whose sandbox the
// orchestrator cannot confirm answers the probe's error without a cleanup
// exec (there is nothing reachable to clean).
func TestWriteFileUploadErrorGoneSandbox(t *testing.T) {
	e := &filesTestEnvd{uploadErr: true}
	c := newFilesEnvd(t, e)
	defer e.close()
	if err := c.WriteFile(context.Background(), filesSandboxID, "/tmp/x.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile with a failing upload succeeded")
	}
	if len(e.execs) != 1 {
		t.Fatalf("execs = %v, want only the stage", e.execs)
	}
}

// TestWriteFileScripts runs the stage and place scripts for real (/bin/sh,
// a temp dir standing in for the guest): the upload dir is private, the
// mode lands, an existing name or a directory target is refused, and
// nothing is left behind.
func TestWriteFileScripts(t *testing.T) {
	root := t.TempDir()
	sh := func(script string, args ...string) (int, string) {
		cmd := exec.Command("/bin/sh", append([]string{"-c", script, "sh"}, args...)...)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		return 0, string(out)
	}
	target := root + "/a/b/f.txt"
	dir := root + "/a/b"
	tmpDir := dir + "/.spoond-upload-0123456789abcdef"
	if rc, out := sh(writeStageScript, target, dir, tmpDir); rc != 0 {
		t.Fatalf("stage: exit %d: %s", rc, out)
	}
	if st, err := os.Stat(tmpDir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("upload dir = %v, %v; want mode 700", st, err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm()&0o055 != 0o055 {
		t.Fatalf("parent = %v, %v; want it traversable by others", st, err)
	}
	// A planted name (or symlink) makes the stage fail instead of reusing it.
	if rc, _ := sh(writeStageScript, target, dir, tmpDir); rc == 0 {
		t.Fatal("stage reused an existing upload directory")
	}
	if err := os.WriteFile(tmpDir+"/f.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc, out := sh(writePlaceScript, "600", tmpDir+"/f.txt", target, tmpDir); rc != 0 {
		t.Fatalf("place: exit %d: %s", rc, out)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "hello" {
		t.Fatalf("target = %q, %v", b, err)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %o, want 600", st.Mode().Perm())
	}
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("upload dir left behind: %v", err)
	}
	// A directory target: the stage refuses it with the is-a-directory
	// status, and so does the place if it appeared after the stage.
	if rc, _ := sh(writeStageScript, dir, root+"/a", root+"/a/.spoond-upload-x"); rc != errIsDirExit {
		t.Fatalf("stage of a directory target: exit %d, want %d", rc, errIsDirExit)
	}
	d2 := root + "/a/.spoond-upload-y"
	if err := os.Mkdir(d2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d2+"/b", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc, _ := sh(writePlaceScript, "644", d2+"/b", dir, d2); rc != errIsDirExit {
		t.Fatalf("place onto a directory: exit %d, want %d", rc, errIsDirExit)
	}
	if _, err := os.Stat(dir + "/b"); !os.IsNotExist(err) {
		t.Fatal("place wrote into a directory target")
	}
}

// Watch request shape and relaying: the Stat precondition runs first, the
// WatchDir request carries the path and recursion, the start event
// becomes WatchReady and each filesystem event one WatchChange carrying
// the watch-root-joined path.
func TestWatchRequestShape(t *testing.T) {
	e := &filesTestEnvd{
		statEntry: &filesystem.EntryInfo{Name: "work", Type: filesystem.FileType_FILE_TYPE_DIRECTORY},
		watchEvents: []*filesystem.FilesystemEvent{
			{Name: "a.txt", Type: filesystem.EventType_EVENT_TYPE_CREATE},
			{Name: "deep/b.txt", Type: filesystem.EventType_EVENT_TYPE_WRITE},
		},
	}
	c := newFilesEnvd(t, e)
	defer e.close()

	events, err := c.Watch(context.Background(), filesSandboxID, "/work", true)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if len(e.statReqs) != 1 || e.statReqs[0] != "/work" {
		t.Fatalf("Stat requests = %v, want [/work]", e.statReqs)
	}
	if len(e.watchReqs) != 1 || e.watchReqs[0] != "/work recursive=true" {
		t.Fatalf("WatchDir requests = %v, want [/work recursive=true]", e.watchReqs)
	}
	if ev := waitE2BWatch(t, events); ev.Kind != substrate.WatchReady {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	if ev := waitE2BWatch(t, events); ev.Kind != substrate.WatchChange || len(ev.Paths) != 1 || ev.Paths[0] != "/work/a.txt" {
		t.Fatalf("first change = %+v, want /work/a.txt", ev)
	}
	if ev := waitE2BWatch(t, events); ev.Kind != substrate.WatchChange || len(ev.Paths) != 1 || ev.Paths[0] != "/work/deep/b.txt" {
		t.Fatalf("second change = %+v, want /work/deep/b.txt", ev)
	}
	if _, ok := <-events; ok {
		t.Fatal("the channel stayed open after the stream ended")
	}
}

// Watch preconditions: a missing path is ErrNotFound and a path that is a
// file is ErrNotDir, both before any WatchDir stream opens.
func TestWatchPreconditions(t *testing.T) {
	e := &filesTestEnvd{} // Stat answers CodeNotFound
	c := newFilesEnvd(t, e)
	defer e.close()
	if _, err := c.Watch(context.Background(), filesSandboxID, "/nope", true); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Watch missing = %v, want ErrNotFound", err)
	}
	if len(e.watchReqs) != 0 {
		t.Fatalf("WatchDir opened for a missing path: %v", e.watchReqs)
	}

	e2 := &filesTestEnvd{statEntry: &filesystem.EntryInfo{Name: "f", Type: filesystem.FileType_FILE_TYPE_FILE}}
	c2 := newFilesEnvd(t, e2)
	defer e2.close()
	if _, err := c2.Watch(context.Background(), filesSandboxID, "/f", true); !errors.Is(err, substrate.ErrNotDir) {
		t.Fatalf("Watch a file = %v, want ErrNotDir", err)
	}
}

// Watch's stream error: envd names it and the channel closes after the
// WatchError event.
func TestWatchStreamError(t *testing.T) {
	e := &filesTestEnvd{
		statEntry: &filesystem.EntryInfo{Name: "work", Type: filesystem.FileType_FILE_TYPE_DIRECTORY},
		watchErr:  connect.NewError(connect.CodeInternal, errors.New("inotify overflow")),
	}
	c := newFilesEnvd(t, e)
	defer e.close()
	events, err := c.Watch(context.Background(), filesSandboxID, "/work", false)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if ev := waitE2BWatch(t, events); ev.Kind != substrate.WatchReady {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	ev := waitE2BWatch(t, events)
	if ev.Kind != substrate.WatchError || !strings.Contains(ev.Err, "inotify overflow") {
		t.Fatalf("terminal event = %+v, want a WatchError naming the stream error", ev)
	}
	if _, ok := <-events; ok {
		t.Fatal("the channel stayed open after the WatchError")
	}
}

// waitE2BWatch reads one event or fails after a bounded wait.
func waitE2BWatch(t *testing.T, events <-chan substrate.WatchEvent) substrate.WatchEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("watch channel closed before the expected event")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("watch event did not arrive within 5s")
		return substrate.WatchEvent{}
	}
}
