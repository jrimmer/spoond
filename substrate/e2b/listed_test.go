package e2b

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/jrimmer/spoond/v2/substrate"
	orchestrator "github.com/jrimmer/spoond/v2/substrate/e2b/gen/orchestrator"
)

// listTestOrchestrator is a SandboxService whose List is controllable: it
// either returns the configured sandboxes, fails with the configured
// error, or blocks until the RPC context is cancelled. It counts List
// calls so a test can pin the bounded retry.
type listTestOrchestrator struct {
	orchestrator.UnimplementedSandboxServiceServer

	mu     sync.Mutex
	calls  int
	ids    []string
	err    error // returned by every List when non-nil
	blocks bool  // wait for the RPC context instead of answering
}

func (o *listTestOrchestrator) List(ctx context.Context, _ *emptypb.Empty) (*orchestrator.SandboxListResponse, error) {
	o.mu.Lock()
	o.calls++
	ids, err, blocks := o.ids, o.err, o.blocks
	o.mu.Unlock()
	if blocks {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	resp := &orchestrator.SandboxListResponse{}
	for _, id := range ids {
		resp.Sandboxes = append(resp.Sandboxes, &orchestrator.RunningSandbox{SandboxId: id})
	}
	return resp, nil
}

func (o *listTestOrchestrator) callCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

// newListClient starts an in-process gRPC server backed by o and returns a
// Client pointed at it with a short List bound so the retry does not wait
// production timeouts.
func newListClient(t *testing.T, o orchestrator.SandboxServiceServer) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	orchestrator.RegisterSandboxServiceServer(srv, o)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := New(Config{
		GRPCAddr:       lis.Addr().String(),
		TokenSeed:      []byte("0123456789abcdef0123456789abcdef"),
		ControlTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestSandboxListedAbsentIsNotFound: a List that succeeds and does not
// name the sandbox is the one case that resolves to ErrNotFound (which
// the API answers as 410 lease_lost).
func TestSandboxListedAbsentIsNotFound(t *testing.T) {
	o := &listTestOrchestrator{ids: []string{"i0000000000000000001"}}
	c := newListClient(t, o)
	err := c.sandboxListed(context.Background(), "i0123456789abcdefghij")
	if !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("sandboxListed absent = %v, want ErrNotFound", err)
	}
	if errors.Is(err, substrate.ErrUnavailable) {
		t.Fatalf("sandboxListed absent = %v, must not be ErrUnavailable", err)
	}
	if o.callCount() != 1 {
		t.Fatalf("List calls = %d, want 1 (no retry after a successful list)", o.callCount())
	}
}

// TestSandboxListedPresentIsTransient: a List that names the sandbox
// resolves to nil (the operation failure is transient, not an absence).
func TestSandboxListedPresentIsTransient(t *testing.T) {
	const id = "i0123456789abcdefghij"
	o := &listTestOrchestrator{ids: []string{id}}
	c := newListClient(t, o)
	if err := c.sandboxListed(context.Background(), id); err != nil {
		t.Fatalf("sandboxListed present = %v, want nil", err)
	}
	if o.callCount() != 1 {
		t.Fatalf("List calls = %d, want 1", o.callCount())
	}
}

// TestSandboxListedListErrorIsUnavailable: a List that keeps failing is
// retried a bounded number of times and then reported as ErrUnavailable,
// never ErrNotFound. A stream drop during an orchestrator stall must not
// become a claim that the sandbox is gone.
func TestSandboxListedListErrorIsUnavailable(t *testing.T) {
	o := &listTestOrchestrator{err: status.Error(codes.DeadlineExceeded, "list timed out")}
	c := newListClient(t, o)
	err := c.sandboxListed(context.Background(), "i0123456789abcdefghij")
	if !errors.Is(err, substrate.ErrUnavailable) {
		t.Fatalf("sandboxListed on a list error = %v, want ErrUnavailable", err)
	}
	if errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("sandboxListed on a list error must not be ErrNotFound: %v", err)
	}
	if got := o.callCount(); got != listProbeAttempts {
		t.Fatalf("List calls = %d, want the bounded retry of %d", got, listProbeAttempts)
	}
}

// TestSandboxListedListErrorRecovers: a List that fails once and then
// succeeds and names the sandbox resolves to nil, so a brief orchestrator
// stall does not poison the decision.
func TestSandboxListedListErrorRecovers(t *testing.T) {
	const id = "i0123456789abcdefghij"
	o := &recoveringListOrchestrator{listTestOrchestrator: &listTestOrchestrator{ids: []string{id}}, failFirst: 1}
	c := newListClient(t, o)
	if err := c.sandboxListed(context.Background(), id); err != nil {
		t.Fatalf("sandboxListed after one failed list = %v, want nil", err)
	}
	if got := o.callCount(); got != 2 {
		t.Fatalf("List calls = %d, want 2 (one failure, one success)", got)
	}
}

// recoveringListOrchestrator fails the first failFirst List calls with a
// deadline and then answers the configured list.
type recoveringListOrchestrator struct {
	*listTestOrchestrator
	failFirst int
}

func (o *recoveringListOrchestrator) List(ctx context.Context, e *emptypb.Empty) (*orchestrator.SandboxListResponse, error) {
	o.mu.Lock()
	n := o.calls
	o.mu.Unlock()
	if n < o.failFirst {
		o.mu.Lock()
		o.calls++
		o.mu.Unlock()
		return nil, status.Error(codes.DeadlineExceeded, "list timed out")
	}
	return o.listTestOrchestrator.List(ctx, e)
}

// TestSandboxListedHungListIsUnavailable: a List that never answers is
// bounded per attempt and reported as ErrUnavailable, not hung forever.
func TestSandboxListedHungListIsUnavailable(t *testing.T) {
	o := &listTestOrchestrator{blocks: true}
	c := newListClient(t, o)
	start := time.Now()
	err := c.sandboxListed(context.Background(), "i0123456789abcdefghij")
	if !errors.Is(err, substrate.ErrUnavailable) {
		t.Fatalf("sandboxListed on a hung list = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("sandboxListed took %s, want the per-attempt bound", elapsed)
	}
}
