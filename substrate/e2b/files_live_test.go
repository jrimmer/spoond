//go:build e2blive

// Live file-operations test against the orchestrator on the E2B host. Run with:
//
//	go test -tags e2blive -count=1 -timeout 30m -run TestLiveFiles ./substrate/e2b/
//
// with the E2B_* environment set (E2B_GRPC_ADDR, E2B_PROXY_URL, E2B_TEAM_ID,
// E2B_TOKEN_SEED_FILE). Builds a debian:12 template, starts one sandbox
// (the real lease) and exercises WriteFile, Stat, ReadFile, MakeDir and
// Remove against it.
package e2b

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

func TestLiveFiles(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	tid := NewTemplateID()
	build := NewUUID()
	res, err := c.BuildTemplate(ctx, substrate.BuildRequest{
		TemplateID: tid,
		BuildID:    build,
		FromImage:  "docker.io/library/debian:12",
		VCPU:       2,
		MemoryMB:   1024,
		DiskSizeMB: 4096,
	})
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	id := NewSandboxID()
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = c.Delete(cctx, id)
		_ = c.DeleteBuild(cctx, tid, build)
	}()

	if _, err := c.Create(ctx, substrate.CreateRequest{
		TemplateID:         tid,
		BuildID:            res.BuildID,
		SandboxID:          id,
		KernelVersion:      res.KernelVersion,
		FirecrackerVersion: res.FirecrackerVersion,
		EndAt:              time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const body = "spoond live file test\n"
	const path = "/root/spoond-live-files.txt"

	// 1. WriteFile with an explicit mode; the parents do not exist yet.
	if err := c.WriteFile(ctx, id, path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 2. Stat reports a regular file with the written size and mode.
	info, err := c.Stat(ctx, id, path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.IsDir {
		t.Fatalf("Stat = %+v, want a regular file", info)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Stat size = %d, want %d", info.Size, len(body))
	}
	if got := info.Mode.Perm(); got != 0o600 {
		t.Errorf("Stat mode = %o, want 600", got)
	}
	if info.Name != "spoond-live-files.txt" {
		t.Errorf("Stat name = %q", info.Name)
	}

	// 3. Stat of a directory (created implicitly with the file's parent).
	dirInfo, err := c.Stat(ctx, id, "/root")
	if err != nil {
		t.Fatalf("Stat /root: %v", err)
	}
	if !dirInfo.IsDir {
		t.Errorf("Stat /root = %+v, want a directory", dirInfo)
	}

	// 4. ReadFile returns the written bytes.
	got, err := c.ReadFile(ctx, id, path, 1<<20)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != body {
		t.Errorf("ReadFile = %q, want %q", got, body)
	}

	// 5. An over-size read fails with ErrTooLarge.
	if _, err := c.ReadFile(ctx, id, path, 4); !errors.Is(err, substrate.ErrTooLarge) {
		t.Errorf("ReadFile max 4: %v, want ErrTooLarge", err)
	}

	// 6. MakeDir creates the directory with the requested mode.
	if err := c.MakeDir(ctx, id, "/root/live-dirs/a/b", 0o750); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	d, err := c.Stat(ctx, id, "/root/live-dirs/a/b")
	if err != nil {
		t.Fatalf("Stat after MakeDir: %v", err)
	}
	if !d.IsDir {
		t.Errorf("MakeDir produced %+v, want a directory", d)
	}

	// 7. Remove deletes the file; it is gone afterwards.
	if err := c.Remove(ctx, id, path, false); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if _, err := c.Stat(ctx, id, path); !errors.Is(err, substrate.ErrNotFound) {
		t.Errorf("Stat after Remove: %v, want ErrNotFound", err)
	}

	// 8. A non-recursive remove refuses a non-empty directory; the recursive
	// remove clears it.
	if err := c.Remove(ctx, id, "/root/live-dirs", false); !errors.Is(err, substrate.ErrNotEmpty) {
		t.Error("non-recursive remove of a non-empty directory succeeded")
	}
	if err := c.Remove(ctx, id, "/root/live-dirs", true); err != nil {
		t.Fatalf("Remove recursive: %v", err)
	}
	if _, err := c.Stat(ctx, id, "/root/live-dirs"); !errors.Is(err, substrate.ErrNotFound) {
		t.Errorf("Stat after recursive Remove: %v, want ErrNotFound", err)
	}
}
