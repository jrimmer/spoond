package e2b

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b/gen/envd/filesystem"
	"github.com/jrimmer/spoond/v2/substrate/e2b/gen/envd/filesystem/filesystemconnect"
	"github.com/jrimmer/spoond/v2/substrate/e2b/gen/envd/process"
	"github.com/jrimmer/spoond/v2/substrate/e2b/gen/envd/process/processconnect"
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

	statEntry *filesystem.EntryInfo // returned by Stat; nil = CodeNotFound
	protoErr  connect.Code          // nonzero: Stat/MakeDir/Remove fail with it
	fileData  []byte                // GET /files body
	fileErr   bool                  // GET /files returns 404
}

type uploadRecord struct {
	method  string
	path    string // decoded path query parameter
	file    []byte // decoded multipart "file" part
	headers http.Header
}

func (e *filesTestEnvd) close() { e.srv.Close() }

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
		if r.Method == http.MethodGet {
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
		return nil, connect.NewError(h.e.protoErr, errors.New("no such file or directory"))
	}
	if h.e.statEntry == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such file or directory"))
	}
	return connect.NewResponse(&filesystem.StatResponse{Entry: h.e.statEntry}), nil
}

func (h *fsHandler) MakeDir(ctx context.Context, r *connect.Request[filesystem.MakeDirRequest]) (*connect.Response[filesystem.MakeDirResponse], error) {
	h.e.makeDirs = append(h.e.makeDirs, r.Msg.Path)
	if h.e.protoErr != 0 {
		return nil, connect.NewError(h.e.protoErr, errors.New("no such file or directory"))
	}
	return connect.NewResponse(&filesystem.MakeDirResponse{Entry: &filesystem.EntryInfo{Name: "x"}}), nil
}

func (h *fsHandler) Remove(ctx context.Context, r *connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error) {
	h.e.removed = append(h.e.removed, r.Msg.Path)
	if h.e.protoErr != 0 {
		return nil, connect.NewError(h.e.protoErr, errors.New("no such file or directory"))
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
	h.e.execs = append(h.e.execs, strings.Join(args, " "))
	if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 1}}}}); err != nil {
		return err
	}
	return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}}}})
}

func (h *execHandler) SendInput(ctx context.Context, r *connect.Request[process.SendInputRequest]) (*connect.Response[process.SendInputResponse], error) {
	return connect.NewResponse(&process.SendInputResponse{}), nil
}

func (h *execHandler) SendSignal(ctx context.Context, r *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error) {
	return connect.NewResponse(&process.SendSignalResponse{}), nil
}

// TestRmdirError: rmdir's "Directory not empty" is ErrNotEmpty (409 at
// the files API); other failures are not.
func TestRmdirError(t *testing.T) {
	err := rmdirError(filesSandboxID, "/d", 1, "rmdir: failed to remove '/d': Directory not empty\n")
	if !errors.Is(err, substrate.ErrNotEmpty) {
		t.Fatalf("not-empty rmdir = %v, want ErrNotEmpty", err)
	}
	err = rmdirError(filesSandboxID, "/d", 1, "rmdir: failed to remove '/d': Permission denied\n")
	if errors.Is(err, substrate.ErrNotEmpty) || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("permission rmdir = %v", err)
	}
}
