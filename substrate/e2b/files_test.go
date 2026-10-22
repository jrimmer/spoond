package e2b

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/e2b/gen/envd/filesystem"
	"github.com/jrimmer/spoond/v4/substrate/e2b/gen/envd/filesystem/filesystemconnect"
	"github.com/jrimmer/spoond/v4/substrate/e2b/gen/envd/process"
	"github.com/jrimmer/spoond/v4/substrate/e2b/gen/envd/process/processconnect"
)

const filesSandboxID = "i0123456789abcdefghij"

// filesTestEnvd stands in for envd: the HTTP /files endpoint and the
// filesystem Connect service, plus a process service that answers execs so
// the chmod/rmdir calls in the file paths run for real.
type filesTestEnvd struct {
	srv *httptest.Server

	uploads  []uploadRecord
	execs    []string // argv lines the client exec'd (install, chmod, rmdir)
	makeDirs []string // filesystem.MakeDir request paths
	removed  []string // filesystem.Remove request paths
	statReqs []string // filesystem.Stat request paths

	// watchReqs records WatchDir requests as "path recursive=<bool>";
	// the scripted watchEvents are sent after the start event, then the
	// stream ends (watchErr, when set, is the terminal stream error).
	watchReqs   []string
	watchEvents []*filesystem.FilesystemEvent
	watchErr    error

	statEntry     *filesystem.EntryInfo // returned by Stat; nil = CodeNotFound
	protoErr      connect.Code          // nonzero: Stat/MakeDir/Remove fail with it
	protoMsg      string                // the connect error's message ("" = "no such file or directory")
	fileData      []byte                // GET /files body
	fileErr       bool                  // GET /files returns 404
	fileStatus    int                   // nonzero: GET /files answers this status (e.g. a directory 400)
	fileStatusMsg string                // that status's body (envd's InvalidPath shape)
	uploadErr     bool                  // POST /files answers 500
	failPlace     bool                  // the WriteFile place exec exits 1
	failPlaceMsg  string                // the place exec exits 1 carrying this stderr
	execRC        int                   // nonzero: every other exec exits with this code
	execStderr    string                // that exec's stderr
	hangExec      bool                  // execs never exit: the client's timer must kill them
	execEnvs      []map[string]string   // process env of each Start, in order

	listEntries []*filesystem.EntryInfo     // returned by ListDir
	listErr     connect.Code                // nonzero: ListDir fails with it
	listMsg     string                      // the ListDir error's message ("" = "list failed")
	listReqs    []filesystem.ListDirRequest // filesystem.ListDir requests
}

type uploadRecord struct {
	method  string
	path    string // decoded path query parameter
	file    []byte // decoded multipart "file" part
	headers http.Header
}

func (e *filesTestEnvd) close() { e.srv.Close() }

// protoErrMsg is the message the stand-in's Connect refusals carry.
func (e *filesTestEnvd) protoErrMsg() string {
	if e.protoMsg != "" {
		return e.protoMsg
	}
	return "no such file or directory"
}

// newFilesClient returns a Client pointed at an arbitrary proxy URL, for
// tests that do not need the full stand-in.
func newFilesClient(t *testing.T, proxyURL string) *Client {
	t.Helper()
	c, err := New(Config{
		GRPCAddr:  "127.0.0.1:1",
		ProxyURL:  proxyURL,
		TokenSeed: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// newFilesEnvd starts the stand-in and returns a Client pointed at it.
func newFilesEnvd(t *testing.T, e *filesTestEnvd) *Client {
	t.Helper()
	mux := http.NewServeMux()

	// HTTP /files: GET downloads, POST takes one multipart part named "file"
	// and creates the file's parents.
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		rec := uploadRecord{method: r.Method, path: r.URL.Query().Get("path"), headers: r.Header.Clone()}
		if r.Method == http.MethodPost {
			if q := r.URL.Query().Get("username"); q != "" {
				t.Errorf("username query = %q, want the Basic header instead", q)
			}
			file, err := decodeMultipartFile(t, r)
			if err != nil {
				t.Errorf("decode upload: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			rec.file = file
		}
		e.uploads = append(e.uploads, rec)
		if r.Method == http.MethodPost && e.uploadErr {
			http.Error(w, "upload failed", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodGet {
			if e.fileStatus != 0 {
				msg := e.fileStatusMsg
				if msg == "" {
					msg = "error"
				}
				http.Error(w, msg, e.fileStatus)
				return
			}
			if e.fileErr {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(e.fileData)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.Handle(filesystemconnect.NewFilesystemHandler(&fsHandler{e: e}))
	mux.Handle(processconnect.NewProcessHandler(&execHandler{e: e}))

	e.srv = httptest.NewServer(mux)
	c, err := New(Config{
		GRPCAddr:  "127.0.0.1:1", // never dialed by the file paths under test
		ProxyURL:  e.srv.URL,
		TokenSeed: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func decodeMultipartFile(t *testing.T, r *http.Request) ([]byte, error) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	if mediaType != "multipart/form-data" {
		return nil, errors.New("content type " + mediaType + ", want multipart/form-data")
	}
	form, err := multipart.NewReader(r.Body, params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		return nil, err
	}
	defer form.RemoveAll()
	parts := form.File["file"]
	if len(parts) != 1 {
		return nil, errors.New("file parts = " + strconv.Itoa(len(parts)) + ", want 1")
	}
	fh, err := parts[0].Open()
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return io.ReadAll(fh)
}

// fsHandler adapts filesTestEnvd to the filesystem Connect service.
type fsHandler struct {
	*filesystemconnect.UnimplementedFilesystemHandler
	e *filesTestEnvd
}

func (h *fsHandler) Stat(ctx context.Context, r *connect.Request[filesystem.StatRequest]) (*connect.Response[filesystem.StatResponse], error) {
	h.e.statReqs = append(h.e.statReqs, r.Msg.Path)
	if h.e.protoErr != 0 {
		return nil, connect.NewError(h.e.protoErr, errors.New(h.e.protoErrMsg()))
	}
	if h.e.statEntry == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such file or directory"))
	}
	return connect.NewResponse(&filesystem.StatResponse{Entry: h.e.statEntry}), nil
}

func (h *fsHandler) ListDir(ctx context.Context, r *connect.Request[filesystem.ListDirRequest]) (*connect.Response[filesystem.ListDirResponse], error) {
	h.e.listReqs = append(h.e.listReqs, filesystem.ListDirRequest{Path: r.Msg.Path, Depth: r.Msg.Depth})
	if h.e.listErr != 0 {
		msg := h.e.listMsg
		if msg == "" {
			msg = "list failed"
		}
		return nil, connect.NewError(h.e.listErr, errors.New(msg))
	}
	return connect.NewResponse(&filesystem.ListDirResponse{Entries: h.e.listEntries}), nil
}

func (h *fsHandler) MakeDir(ctx context.Context, r *connect.Request[filesystem.MakeDirRequest]) (*connect.Response[filesystem.MakeDirResponse], error) {
	h.e.makeDirs = append(h.e.makeDirs, r.Msg.Path)
	if h.e.protoErr != 0 {
		return nil, connect.NewError(h.e.protoErr, errors.New(h.e.protoErrMsg()))
	}
	return connect.NewResponse(&filesystem.MakeDirResponse{Entry: &filesystem.EntryInfo{Name: "x"}}), nil
}

// WatchDir records the request, sends the start event and then each
// scripted filesystem event, and returns (ending the stream) — or
// watchErr when the test asked for a terminal error.
func (h *fsHandler) WatchDir(ctx context.Context, r *connect.Request[filesystem.WatchDirRequest], stream *connect.ServerStream[filesystem.WatchDirResponse]) error {
	h.e.watchReqs = append(h.e.watchReqs, fmt.Sprintf("%s recursive=%v", r.Msg.GetPath(), r.Msg.GetRecursive()))
	if err := stream.Send(&filesystem.WatchDirResponse{Event: &filesystem.WatchDirResponse_Start{Start: &filesystem.WatchDirResponse_StartEvent{}}}); err != nil {
		return err
	}
	if h.e.watchErr != nil {
		return h.e.watchErr
	}
	for _, ev := range h.e.watchEvents {
		if err := stream.Send(&filesystem.WatchDirResponse{Event: &filesystem.WatchDirResponse_Filesystem{Filesystem: ev}}); err != nil {
			return err
		}
	}
	return nil
}

func (h *fsHandler) Remove(ctx context.Context, r *connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error) {
	h.e.removed = append(h.e.removed, r.Msg.Path)
	if h.e.protoErr != 0 {
		return nil, connect.NewError(h.e.protoErr, errors.New(h.e.protoErrMsg()))
	}
	return connect.NewResponse(&filesystem.RemoveResponse{}), nil
}

// execHandler is a minimal process.Process service: every Start records the
// command line and answers with a pid and a zero exit.
type execHandler struct {
	*processconnect.UnimplementedProcessHandler
	e *filesTestEnvd
}

func (h *execHandler) Start(ctx context.Context, r *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	args := append([]string{r.Msg.GetProcess().GetCmd()}, r.Msg.GetProcess().GetArgs()...)
	h.e.execEnvs = append(h.e.execEnvs, r.Msg.GetProcess().GetEnvs())
	h.e.execs = append(h.e.execs, strings.Join(args, " "))
	if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 1}}}}); err != nil {
		return err
	}
	if h.e.hangExec {
		// Never exits: the client's own timer must kill it, which is the
		// client's TimedOut path.
		<-ctx.Done()
		return ctx.Err()
	}
	code, stderr := 0, ""
	switch {
	case len(args) > 2 && args[2] == writePlaceScript && h.e.failPlaceMsg != "":
		code, stderr = 1, h.e.failPlaceMsg
	case h.e.failPlace && len(args) > 2 && args[2] == writePlaceScript:
		code = 1
	case h.e.execRC != 0:
		code, stderr = h.e.execRC, h.e.execStderr
	}
	if stderr != "" {
		if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte(stderr)}}}}}); err != nil {
			return err
		}
	}
	return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: int32(code), Exited: true}}}})
}

func (h *execHandler) SendInput(ctx context.Context, r *connect.Request[process.SendInputRequest]) (*connect.Response[process.SendInputResponse], error) {
	return connect.NewResponse(&process.SendInputResponse{}), nil
}

func (h *execHandler) SendSignal(ctx context.Context, r *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error) {
	return connect.NewResponse(&process.SendSignalResponse{}), nil
}

// TestRmdirError: rmdir's "Directory not empty" is ErrNotEmpty (409 at
// the files API), its "Permission denied" is ErrPermission (403 EACCES);
// other failures are not sentinels at all.
func TestRmdirError(t *testing.T) {
	err := rmdirError(filesSandboxID, "/d", 1, "rmdir: failed to remove '/d': Directory not empty\n")
	if !errors.Is(err, substrate.ErrNotEmpty) {
		t.Fatalf("not-empty rmdir = %v, want ErrNotEmpty", err)
	}
	err = rmdirError(filesSandboxID, "/d", 1, "rmdir: failed to remove '/d': Permission denied\n")
	if !errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("permission rmdir = %v, want ErrPermission", err)
	}
	if errors.Is(err, substrate.ErrNotEmpty) {
		t.Fatalf("permission rmdir must not be ErrNotEmpty: %v", err)
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("permission rmdir lost the message: %v", err)
	}
	err = rmdirError(filesSandboxID, "/d", 1, "rmdir: failed to remove '/d': Input/output error\n")
	if errors.Is(err, substrate.ErrNotEmpty) || errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("plain rmdir = %v, want no sentinel", err)
	}
}

// TestGuestOpError: the guest stderr's "Permission denied" is
// ErrPermission (the files API's EACCES); other text is a plain failure.
func TestGuestOpError(t *testing.T) {
	err := guestOpError("chmod: changing permissions of '/f': Operation not permitted\n")
	if errors.Is(err, substrate.ErrPermission) || errors.Is(err, substrate.ErrIsDir) {
		t.Fatalf("non-permission stderr = %v, want no sentinel", err)
	}
	err = guestOpError("rm: cannot remove '/f': Permission denied\n")
	if !errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("permission stderr = %v, want ErrPermission", err)
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("permission stderr lost the message: %v", err)
	}
	// The empty stderr of a killed command is a plain failure, not a
	// sentinel.
	if err := guestOpError(""); errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("empty stderr = %v, want no sentinel", err)
	}
}

// TestMapConnectErrorSentinels pins the Connect error mapping the file
// routes' codes ride on: NotFound (ENOENT) and PermissionDenied (EACCES)
// map to their sentinels, FailedPrecondition is classified by its text
// (envd refuses a path operation with it), and other codes pass
// through as plain failures.
func TestMapConnectErrorSentinels(t *testing.T) {
	err := mapConnectError(connect.NewError(connect.CodeNotFound, errors.New("no such file or directory")))
	if !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("NotFound = %v, want ErrNotFound", err)
	}
	err = mapConnectError(connect.NewError(connect.CodePermissionDenied, errors.New("permission denied: /f")))
	if !errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("PermissionDenied = %v, want ErrPermission", err)
	}
	// envd's FailedPrecondition texts, classified by the message:
	if err = mapConnectError(connect.NewError(connect.CodeFailedPrecondition, errors.New("directory not empty"))); !errors.Is(err, substrate.ErrNotEmpty) {
		t.Fatalf("FailedPrecondition not-empty = %v, want ErrNotEmpty", err)
	}
	// The filesystem's own ENOTDIR text ("mkdir …: not a directory") must
	// classify as ErrNotDir (409 ENOTDIR), not be eaten by the bare
	// "directory" (ErrIsDir) check.
	if err = mapConnectError(connect.NewError(connect.CodeFailedPrecondition, errors.New("mkdir /f/child: not a directory"))); !errors.Is(err, substrate.ErrNotDir) {
		t.Fatalf("FailedPrecondition not-a-directory = %v, want ErrNotDir", err)
	}
	if err = mapConnectError(connect.NewError(connect.CodeFailedPrecondition, errors.New("destination is a directory"))); !errors.Is(err, substrate.ErrIsDir) {
		t.Fatalf("FailedPrecondition is-dir = %v, want ErrIsDir", err)
	}
	if err = mapConnectError(connect.NewError(connect.CodeFailedPrecondition, errors.New("file exists"))); !errors.Is(err, substrate.ErrExist) {
		t.Fatalf("FailedPrecondition exists = %v, want ErrExist", err)
	}
	if err = mapConnectError(connect.NewError(connect.CodeFailedPrecondition, errors.New("path in use"))); errors.Is(err, substrate.ErrNotEmpty) || errors.Is(err, substrate.ErrIsDir) || errors.Is(err, substrate.ErrExist) {
		t.Fatalf("unclassified FailedPrecondition = %v, want a plain failure", err)
	}
	// Whole phrases only: a word inside another phrase never classifies.
	if err = mapConnectError(connect.NewError(connect.CodeInternal, errors.New("directory vanished"))); errors.Is(err, substrate.ErrIsDir) {
		t.Fatalf("Internal = %v, want no sentinel", err)
	}
	if err = mapConnectError(connect.NewError(connect.CodeFailedPrecondition, errors.New("open /f: no such file or directory"))); !errors.Is(err, substrate.ErrNotFound) || errors.Is(err, substrate.ErrIsDir) {
		t.Fatalf("no such file or directory = %v, want ErrNotFound only", err)
	}
	if err = mapConnectError(connect.NewError(connect.CodeFailedPrecondition, errors.New("path /f does not exist"))); !errors.Is(err, substrate.ErrNotFound) || errors.Is(err, substrate.ErrExist) {
		t.Fatalf("does not exist = %v, want ErrNotFound only", err)
	}
	// envd's own mkdir refusals (filesystem/dir.go).
	if err = mapConnectError(connect.NewError(connect.CodeAlreadyExists, errors.New("directory already exists: /d"))); !errors.Is(err, substrate.ErrExist) {
		t.Fatalf("AlreadyExists = %v, want ErrExist", err)
	}
	if err = mapConnectError(connect.NewError(connect.CodeInvalidArgument, errors.New("path already exists but it is not a directory: /f"))); !errors.Is(err, substrate.ErrExist) {
		t.Fatalf("mkdir over a file = %v, want ErrExist", err)
	}
	if err = mapConnectError(connect.NewError(connect.CodeInternal, errors.New("error removing file or directory: remove /d: directory not empty"))); !errors.Is(err, substrate.ErrNotEmpty) {
		t.Fatalf("remove of a non-empty directory = %v, want ErrNotEmpty", err)
	}
}

// TestMapEnvdFileStatus400: envd's 400 is EISDIR only when its body says
// "is a directory"; a path it could not resolve is a plain failure.
func TestMapEnvdFileStatus400(t *testing.T) {
	resp := func(body string) *http.Response {
		return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: io.NopCloser(strings.NewReader(body))}
	}
	if err := mapEnvdFileStatus("sb", "/d", resp(`{"message":"path '/d' is a directory","code":400}`)); !errors.Is(err, substrate.ErrIsDir) {
		t.Fatalf("directory 400 = %v, want ErrIsDir", err)
	}
	if err := mapEnvdFileStatus("sb", "/x", resp(`{"message":"error expanding and resolving path '/x': bad user","code":400}`)); errors.Is(err, substrate.ErrIsDir) {
		t.Fatalf("resolve 400 = %v, want a plain failure", err)
	}
}

// TestWriteFilePermissionRefused drives WriteFile to a place exec the
// guest refuses with "Permission denied": the answer wraps
// ErrPermission (the files API's 403 EACCES).
func TestWriteFilePermissionRefused(t *testing.T) {
	e := &filesTestEnvd{failPlaceMsg: "mv: preserving permissions for '/f': Permission denied\n"}
	c := newFilesEnvd(t, e)
	defer e.close()

	err := c.WriteFile(context.Background(), filesSandboxID, "/f.txt", []byte("v"), 0o600)
	if !errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("WriteFile refused = %v, want ErrPermission", err)
	}
}

// TestMakeDirPermissionRefused drives MakeDir at a PermissionDenied
// refusal: the answer wraps ErrPermission.
func TestMakeDirPermissionRefused(t *testing.T) {
	e := &filesTestEnvd{protoErr: connect.CodePermissionDenied, protoMsg: " permission denied: /secured"}
	c := newFilesEnvd(t, e)
	defer e.close()

	if err := c.MakeDir(context.Background(), filesSandboxID, "/secured", 0o755); !errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("MakeDir refused = %v, want ErrPermission", err)
	}
}

// TestExecTimedOut pins the substrate's own timeout marker: an exec the
// client's timer kills (the stand-in's process never exits) answers
// exit 124, the human-readable note and TimedOut; an exec that exits on
// its own — even printing about timeouts — sets no flag.
func TestExecTimedOut(t *testing.T) {
	e := &filesTestEnvd{hangExec: true}
	c := newFilesEnvd(t, e)
	defer e.close()

	start := time.Now()
	res, err := c.Exec(context.Background(), filesSandboxID, substrate.ExecRequest{
		Args: []string{"/bin/sleep", "60"}, Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 124 {
		t.Fatalf("exit = %d, want 124", res.ExitCode)
	}
	if !res.TimedOut {
		t.Fatalf("TimedOut = false, want true (the timer killed it)")
	}
	if !strings.Contains(res.Stderr, "exec timed out") {
		t.Fatalf("stderr = %q, want the timeout note", res.Stderr)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("exec ran %s, want the timer to end it", elapsed)
	}

	// A plain context cancel (a client hang-up, no deadline) is not the
	// substrate's own time limit: the answer is the error and no flag.
	ectx, ecancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); ecancel() }()
	res, err = c.Exec(ectx, filesSandboxID, substrate.ExecRequest{Args: []string{"/bin/sleep", "60"}})
	if err == nil {
		t.Fatalf("cancelled exec = %+v, want an error", res)
	}
	if res.TimedOut {
		t.Fatalf("cancelled exec result = %+v, want no TimedOut flag", res)
	}
	if err != context.Canceled {
		t.Fatalf("cancelled exec error = %v, want context.Canceled", err)
	}

	// The command itself ending (even mentioning timeouts) is not a
	// substrate timeout.
	e2 := &filesTestEnvd{execRC: 124, execStderr: "connection timed out\n"}
	c2 := newFilesEnvd(t, e2)
	defer e2.close()
	res, err = c2.Exec(context.Background(), filesSandboxID, substrate.ExecRequest{Args: []string{"cmd"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 124 || res.TimedOut {
		t.Fatalf("self-exit result = %+v, want exit 124 without TimedOut", res)
	}
}
