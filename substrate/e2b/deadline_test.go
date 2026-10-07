package e2b

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/jrimmer/spoond/v2/substrate"
	info "github.com/jrimmer/spoond/v2/substrate/e2b/gen/info"
	orchestrator "github.com/jrimmer/spoond/v2/substrate/e2b/gen/orchestrator"
)

// blockingOrchestrator is a gRPC stand-in whose methods never return on
// their own: each waits for the RPC's context to be cancelled, modelling
// a hung orchestrator. The client's per-call timeout is the only thing
// that can release the caller.
type blockingOrchestrator struct {
	orchestrator.UnimplementedSandboxServiceServer
	info.UnimplementedInfoServiceServer

	mu    sync.Mutex
	calls []string
	// entered is closed the first time any method is entered.
	enteredOnce sync.Once
	entered     chan struct{}
}

func newBlockingOrchestrator() *blockingOrchestrator {
	return &blockingOrchestrator{entered: make(chan struct{})}
}

func (b *blockingOrchestrator) block(ctx context.Context, method string) error {
	b.mu.Lock()
	b.calls = append(b.calls, method)
	b.mu.Unlock()
	b.enteredOnce.Do(func() { close(b.entered) })
	<-ctx.Done() // never returns until the RPC context is cancelled
	return ctx.Err()
}

func (b *blockingOrchestrator) Create(ctx context.Context, _ *orchestrator.SandboxCreateRequest) (*orchestrator.SandboxCreateResponse, error) {
	return nil, b.block(ctx, "Create")
}

func (b *blockingOrchestrator) Delete(ctx context.Context, _ *orchestrator.SandboxDeleteRequest) (*emptypb.Empty, error) {
	return nil, b.block(ctx, "Delete")
}

func (b *blockingOrchestrator) Pause(ctx context.Context, _ *orchestrator.SandboxPauseRequest) (*orchestrator.SandboxPauseResponse, error) {
	return nil, b.block(ctx, "Pause")
}

func (b *blockingOrchestrator) Checkpoint(ctx context.Context, _ *orchestrator.SandboxCheckpointRequest) (*orchestrator.SandboxCheckpointResponse, error) {
	return nil, b.block(ctx, "Checkpoint")
}

func (b *blockingOrchestrator) List(ctx context.Context, _ *emptypb.Empty) (*orchestrator.SandboxListResponse, error) {
	return nil, b.block(ctx, "List")
}

func (b *blockingOrchestrator) Update(ctx context.Context, _ *orchestrator.SandboxUpdateRequest) (*emptypb.Empty, error) {
	return nil, b.block(ctx, "Update")
}

func (b *blockingOrchestrator) ServiceInfo(ctx context.Context, _ *emptypb.Empty) (*info.ServiceInfoResponse, error) {
	return nil, b.block(ctx, "ServiceInfo")
}

func (b *blockingOrchestrator) ServiceStatusOverride(ctx context.Context, _ *info.ServiceStatusChangeRequest) (*emptypb.Empty, error) {
	return nil, b.block(ctx, "ServiceStatusOverride")
}

// newBlockingClient starts an in-process gRPC server backed by b and
// returns a Client pointed at it with all per-call timeouts collapsed so
// the test does not wait for production values.
func newBlockingClient(t *testing.T, b *blockingOrchestrator) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	orchestrator.RegisterSandboxServiceServer(srv, b)
	info.RegisterInfoServiceServer(srv, b)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := New(Config{
		GRPCAddr:          lis.Addr().String(),
		TokenSeed:         []byte("0123456789abcdef0123456789abcdef"),
		CreateTimeout:     100 * time.Millisecond,
		PauseTimeout:      100 * time.Millisecond,
		CheckpointTimeout: 100 * time.Millisecond,
		DeleteTimeout:     100 * time.Millisecond,
		NodeInfoTimeout:   100 * time.Millisecond,
		ControlTimeout:    100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestPerCallDeadlines: each bounded RPC returns a DeadlineExceeded
// within its configured timeout when the orchestrator hangs. A hung RPC
// must never hold the caller (and so the loop, the busy flag and the
// snapshot limiter) forever.
func TestPerCallDeadlines(t *testing.T) {
	b := newBlockingOrchestrator()
	c := newBlockingClient(t, b)
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"Create", func() error {
			_, err := c.Create(ctx, substrate.CreateRequest{SandboxID: "i0123456789abcdefghij", EndAt: time.Now().Add(time.Hour)})
			return err
		}},
		{"Delete", func() error { return c.Delete(ctx, "i0123456789abcdefghij") }},
		{"Pause", func() error {
			_, _, err := c.Pause(ctx, "i0123456789abcdefghij", "t0123456789abcdefghij")
			return err
		}},
		{"Checkpoint", func() error {
			_, _, err := c.Checkpoint(ctx, "i0123456789abcdefghij")
			return err
		}},
		{"List", func() error { _, err := c.List(ctx); return err }},
		{"UpdateEgress", func() error { return c.UpdateEgress(ctx, "i0123456789abcdefghij", substrate.Egress{}) }},
		{"UpdateEndAt", func() error {
			return c.UpdateEndAt(ctx, "i0123456789abcdefghij", time.Now())
		}},
		{"NodeInfo", func() error { _, err := c.NodeInfo(ctx); return err }},
		{"SetDraining", func() error { return c.SetDraining(ctx, true) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			err := tc.call()
			if err == nil {
				t.Fatalf("%s returned nil, want a timeout", tc.name)
			}
			if status.Code(err) != codes.DeadlineExceeded && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s error = %v, want DeadlineExceeded", tc.name, err)
			}
			// Well under the test's own patience: the per-call timeout
			// released the call.
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("%s took %s, want the per-call timeout", tc.name, elapsed)
			}
		})
	}
}

// TestPerCallDeadlineHonorsEarlierContext: an earlier caller deadline is
// never extended by the per-call bound.
func TestPerCallDeadlineHonorsEarlierContext(t *testing.T) {
	b := newBlockingOrchestrator()
	c := newBlockingClient(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.NodeInfo(ctx)
	if status.Code(err) != codes.DeadlineExceeded && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NodeInfo error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("NodeInfo took %s, want the caller's 30ms deadline to win", elapsed)
	}
}

// TestConfigDefaultsAreBounded: a hand-built Config with zero timeouts
// still ends up with bounded per-call values, and New installs keepalive.
func TestConfigDefaultsAreBounded(t *testing.T) {
	cfg := Config{}.withDefaults()
	for name, got := range map[string]time.Duration{
		"Create":     cfg.CreateTimeout,
		"Pause":      cfg.PauseTimeout,
		"Checkpoint": cfg.CheckpointTimeout,
		"Delete":     cfg.DeleteTimeout,
		"NodeInfo":   cfg.NodeInfoTimeout,
		"Control":    cfg.ControlTimeout,
	} {
		if got <= 0 {
			t.Fatalf("%s timeout = %s, want a positive default", name, got)
		}
	}
	if grpcKeepaliveParams.Time <= 0 || grpcKeepaliveParams.Timeout <= 0 {
		t.Fatalf("keepalive params not set: %+v", grpcKeepaliveParams)
	}
}

// TestEnvTimeoutParsing: E2B_* timeout variables accept a Go duration or
// a plain integer of seconds and fall back to the default otherwise.
func TestEnvTimeoutParsing(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.WriteFile(seed, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	t.Setenv("E2B_TOKEN_SEED_FILE", seed)
	t.Setenv("E2B_CREATE_TIMEOUT", "90s")
	t.Setenv("E2B_DELETE_TIMEOUT", "45")
	t.Setenv("E2B_PAUSE_TIMEOUT", "not-a-duration")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.CreateTimeout != 90*time.Second {
		t.Fatalf("CreateTimeout = %s, want 90s", cfg.CreateTimeout)
	}
	if cfg.DeleteTimeout != 45*time.Second {
		t.Fatalf("DeleteTimeout = %s, want 45s (plain seconds)", cfg.DeleteTimeout)
	}
	if cfg.PauseTimeout != DefaultPauseTimeout {
		t.Fatalf("PauseTimeout = %s, want the default", cfg.PauseTimeout)
	}
}
