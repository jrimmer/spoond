//go:build e2blive

// Live test against the orchestrator on vm2. Run with:
//
//	go test -tags e2blive -count=1 -timeout 30m ./substrate/e2b/
//
// with the E2B_* environment set (E2B_GRPC_ADDR, E2B_PROXY_URL, E2B_TEAM_ID,
// E2B_TOKEN_SEED_FILE), from /root/src/spoond.
package e2b

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

func TestLive(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	// 1. BuildTemplate from debian:12.
	tid := NewTemplateID()
	mainBuild := NewUUID()

	created := map[string]bool{}
	var builds []string // extra builds beyond the template build, for cleanup
	newSandbox := func() string {
		id := NewSandboxID()
		created[id] = true
		return id
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for id := range created {
			_ = c.Delete(cctx, id)
		}
		_ = c.DeleteBuild(cctx, tid, mainBuild)
		for _, b := range builds {
			_ = c.DeleteBuild(cctx, tid, b)
		}
	}

	// BuildTemplate from debian:12 (step 1 builds below).
	res, err := c.BuildTemplate(ctx, substrate.BuildRequest{
		TemplateID: tid,
		BuildID:    mainBuild,
		FromImage:  "docker.io/library/debian:12",
		VCPU:       2,
		MemoryMB:   1024,
		DiskSizeMB: 4096,
	})
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	defer cleanup()
	t.Logf("build %s: kernel=%s firecracker=%s envd=%s disk=%dMiB",
		res.BuildID, res.KernelVersion, res.FirecrackerVersion, res.EnvdVersion, res.DiskSizeMB)
	if res.KernelVersion == "" || res.FirecrackerVersion == "" || res.EnvdVersion == "" {
		t.Fatalf("build versions empty: %+v", res)
	}

	create := func(id, buildID string, resume bool) substrate.Sandbox {
		t.Helper()
		sb, err := c.Create(ctx, substrate.CreateRequest{
			TemplateID:         tid,
			BuildID:            buildID,
			SandboxID:          id,
			KernelVersion:      res.KernelVersion,
			FirecrackerVersion: res.FirecrackerVersion,
			EnvdVersion:        res.EnvdVersion,
			VCPU:               2,
			MemoryMB:           1024,
			DiskSizeMB:         4096,
			Resume:             resume,
			EndAt:              time.Now().Add(time.Hour),
			Egress:             substrate.Egress{AllowedCIDRs: []string{"0.0.0.0/0", "::/0"}},
		})
		if err != nil {
			t.Fatalf("Create %s (build %s): %v", id, buildID, err)
		}
		return sb
	}

	// 2. Create from that build; HostIP is in 10.11.0.0/16.
	id := newSandbox()
	sb := create(id, res.BuildID, false)
	if hostRe := regexp.MustCompile(`^10\.11\.`); !hostRe.MatchString(sb.HostIP) {
		t.Fatalf("HostIP = %q, want match %s", sb.HostIP, hostRe)
	}
	t.Logf("sandbox %s on %s", id, sb.HostIP)

	// 3. Exec returns ok.
	r, err := c.Exec(ctx, id, substrate.ExecRequest{Args: []string{"/bin/bash", "-c", "echo ok"}})
	if err != nil {
		t.Fatalf("Exec echo: %v", err)
	}
	if strings.TrimSpace(r.Stdout) != "ok" || r.ExitCode != 0 {
		t.Fatalf("Exec echo = %+v", r)
	}

	// 4. Exec timeout returns exit 124.
	r, err = c.Exec(ctx, id, substrate.ExecRequest{
		Args:    []string{"/bin/bash", "-c", "sleep 30"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec timeout: %v", err)
	}
	if r.ExitCode != 124 || !strings.Contains(r.Stderr, "exec timed out after 2s") {
		t.Fatalf("Exec timeout = %+v", r)
	}

	// 5. PTY: bash -l, echo X$((1+1)) → X2, then exit.
	p, err := c.Start(ctx, id, substrate.StartRequest{Args: []string{"/bin/bash", "-l"}, PTY: true})
	if err != nil {
		t.Fatalf("Start pty: %v", err)
	}
	defer p.Close()
	if err := p.Write([]byte("echo X$((1+1))\n")); err != nil {
		t.Fatalf("pty write: %v", err)
	}
	sawX2, sawExit := false, false
	deadline := time.After(2 * time.Minute)
pty:
	for {
		select {
		case <-deadline:
			t.Fatal("pty: timed out")
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatal("pty: events closed before exit")
			}
			switch ev.Kind {
			case substrate.EventStarted:
				t.Logf("pty started, pid %d", ev.PID)
			case substrate.EventPTY:
				t.Logf("pty: %q", ev.Data)
				if !sawX2 && bytes.Contains(ev.Data, []byte("X2")) {
					sawX2 = true
					if err := p.Write([]byte("exit\n")); err != nil {
						t.Fatalf("pty write exit: %v", err)
					}
				}
			case substrate.EventExit:
				sawExit = true
				break pty
			case substrate.EventError:
				t.Fatalf("pty: %s", ev.Err)
			}
		}
	}
	if !sawX2 || !sawExit {
		t.Fatalf("pty: sawX2=%v sawExit=%v", sawX2, sawExit)
	}

	// 6. Checkpoint, then create 2 forks from the checkpoint build.
	cb, refs, err := c.Checkpoint(ctx, id)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	builds = append(builds, cb)
	t.Logf("checkpoint build %s: rootfs=%d memfile=%d", cb, len(refs.RootfsBuildIDs), len(refs.MemfileBuildIDs))
	for range 2 {
		fid := newSandbox()
		create(fid, cb, false)
		r, err := c.Exec(ctx, fid, substrate.ExecRequest{Args: []string{"/bin/bash", "-c", "echo fork-ok"}})
		if err != nil {
			t.Fatalf("fork exec %s: %v", fid, err)
		}
		if strings.TrimSpace(r.Stdout) != "fork-ok" {
			t.Fatalf("fork exec %s = %+v", fid, r)
		}
	}

	// 7. Pause a sandbox, resume it with the same id; the file survives.
	if _, err := c.Exec(ctx, id, substrate.ExecRequest{Args: []string{"/bin/bash", "-c", "echo persisted > /tmp/u06-live"}}); err != nil {
		t.Fatalf("write file: %v", err)
	}
	pb, _, err := c.Pause(ctx, id, tid)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	builds = append(builds, pb)
	create(id, pb, true)
	r, err = c.Exec(ctx, id, substrate.ExecRequest{Args: []string{"/bin/bash", "-c", "cat /tmp/u06-live"}})
	if err != nil {
		t.Fatalf("read file after resume: %v", err)
	}
	if strings.TrimSpace(r.Stdout) != "persisted" {
		t.Fatalf("file after resume = %q, want persisted", r.Stdout)
	}

	// 8. Delete everything; List no longer contains them.
	for did := range created {
		if err := c.Delete(ctx, did); err != nil {
			t.Errorf("Delete %s: %v", did, err)
		}
	}
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list {
		if created[s.ID] {
			t.Errorf("List still contains %s", s.ID)
		}
	}

	// 9. DeleteBuild for the builds created.
	if err := c.DeleteBuild(ctx, tid, res.BuildID); err != nil {
		t.Errorf("DeleteBuild %s: %v", res.BuildID, err)
	}
	for _, b := range builds {
		if err := c.DeleteBuild(ctx, tid, b); err != nil {
			t.Errorf("DeleteBuild %s: %v", b, err)
		}
	}
}
