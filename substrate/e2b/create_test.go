package e2b

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/jrimmer/spoond/v3/substrate"
	orchestrator "github.com/jrimmer/spoond/v3/substrate/e2b/gen/orchestrator"
)

// recordingOrchestrator is a gRPC stand-in that keeps each Create request
// and answers with a fixed filesystem_boot_applied.
type recordingOrchestrator struct {
	orchestrator.UnimplementedSandboxServiceServer

	applied bool

	mu      sync.Mutex
	creates []*orchestrator.SandboxCreateRequest
	deletes []string
}

func (r *recordingOrchestrator) Create(_ context.Context, req *orchestrator.SandboxCreateRequest) (*orchestrator.SandboxCreateResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates = append(r.creates, req)
	return &orchestrator.SandboxCreateResponse{FilesystemBootApplied: r.applied}, nil
}

func (r *recordingOrchestrator) Delete(_ context.Context, req *orchestrator.SandboxDeleteRequest) (*emptypb.Empty, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, req.GetSandboxId())
	return &emptypb.Empty{}, nil
}

// newRecordingClient points a Client at r over in-process gRPC and at an
// envd proxy whose /health always answers 200, so Create completes.
func newRecordingClient(t *testing.T, r *recordingOrchestrator) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	orchestrator.RegisterSandboxServiceServer(srv, r)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)

	c, err := New(Config{
		GRPCAddr:       lis.Addr().String(),
		ProxyURL:       proxy.URL,
		TokenSeed:      []byte("0123456789abcdef0123456789abcdef"),
		CreateTimeout:  time.Second,
		DeleteTimeout:  time.Second,
		ControlTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestCreateFilesystemBoot: a FilesystemBoot create sends filesystem_boot
// and reports the orchestrator's filesystem_boot_applied; a plain create
// leaves the field unset so the orchestrator resumes the snapshot.
func TestCreateFilesystemBoot(t *testing.T) {
	r := &recordingOrchestrator{applied: true}
	c := newRecordingClient(t, r)
	id := "i0123456789abcdefghij"

	sb, err := c.Create(t.Context(), substrate.CreateRequest{SandboxID: id, VCPU: 4, MemoryMB: 8192, FilesystemBoot: true, EndAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !sb.FilesystemBootApplied {
		t.Fatal("FilesystemBootApplied = false, want true")
	}
	got := r.creates[0]
	if got.FilesystemBoot == nil || !*got.FilesystemBoot {
		t.Fatalf("filesystem_boot = %v, want true", got.FilesystemBoot)
	}
	if got.GetSandbox().GetVcpu() != 4 || got.GetSandbox().GetRamMb() != 8192 {
		t.Fatalf("size = %d vCPU / %d MB, want 4 / 8192", got.GetSandbox().GetVcpu(), got.GetSandbox().GetRamMb())
	}

	if _, err := c.Create(t.Context(), substrate.CreateRequest{SandboxID: id, EndAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fb := r.creates[1].FilesystemBoot; fb != nil {
		t.Fatalf("plain create sent filesystem_boot = %v, want unset", *fb)
	}
}

// TestCreateFilesystemBootNotApplied: an orchestrator that ignores
// filesystem_boot would start the snapshot at its own size. Create fails
// and deletes the sandbox instead of handing it out.
func TestCreateFilesystemBootNotApplied(t *testing.T) {
	r := &recordingOrchestrator{applied: false}
	c := newRecordingClient(t, r)
	id := "i0123456789abcdefghij"

	_, err := c.Create(t.Context(), substrate.CreateRequest{SandboxID: id, VCPU: 4, MemoryMB: 8192, FilesystemBoot: true, EndAt: time.Now().Add(time.Hour)})
	if err == nil || !strings.Contains(err.Error(), "filesystem boot") {
		t.Fatalf("Create err = %v, want a filesystem boot error", err)
	}
	if len(r.deletes) != 1 || r.deletes[0] != id {
		t.Fatalf("deletes = %v, want [%s]", r.deletes, id)
	}
}
