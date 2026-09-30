//go:build e2blive

// Live check per image against the orchestrator on vm2. Run with:
//
//	go test -tags e2blive -count=1 -timeout 60m ./cmd/spoond-images/
//
// from /root/src/spoond, with the E2B_* environment set (FromEnv
// defaults). For every baked manifest image it creates a sandbox from
// the image's current_build_id with the manifest env, runs `cat
// /etc/resolv.conf`, requires the first line to be `nameserver
// 10.1.0.1`, and deletes the sandbox.
package spoondimages

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/store"
	"github.com/jrimmer/spoond/substrate"
	"github.com/jrimmer/spoond/substrate/e2b"
)

// repoRoot is the spoond checkout root, resolved from this file's
// location: go test runs with the package directory as the working
// directory, so a cwd-relative path like images/manifest.yaml does not
// resolve.
var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)     // .../cmd/spoond-images/live_test.go
	dir := filepath.Dir(file)              // .../cmd/spoond-images
	return filepath.Dir(filepath.Dir(dir)) // checkout root
}()

func TestLiveImages(t *testing.T) {
	ctx := context.Background()

	m, err := loadManifest(filepath.Join(repoRoot, "images", "manifest.yaml"))
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	db, err := store.Open(envOr("SPOOND_DB_PATH", "/var/lib/spoond/staging.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer db.Close()

	cfg, err := e2b.FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	c, err := e2b.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, img := range m.Images {
		if !img.Baked {
			continue
		}
		t.Run(img.Name, func(t *testing.T) {
			row, err := db.GetImage(ctx, img.Name)
			if err != nil {
				t.Fatalf("GetImage %s: %v", img.Name, err)
			}
			if row.CurrentBuildID == "" {
				t.Fatalf("%s: no current build (run `spoond images build --all` first)", img.Name)
			}
			build, err := db.GetBuild(ctx, row.CurrentBuildID)
			if err != nil {
				t.Fatalf("GetBuild %s: %v", row.CurrentBuildID, err)
			}

			id := e2b.NewSandboxID()
			sb, err := c.Create(ctx, substrate.CreateRequest{
				TemplateID:         row.TemplateID,
				BuildID:            row.CurrentBuildID,
				SandboxID:          id,
				KernelVersion:      build.KernelVersion,
				FirecrackerVersion: build.FirecrackerVersion,
				EnvdVersion:        build.EnvdVersion,
				VCPU:               uint32(row.VCPU),
				MemoryMB:           uint32(row.MemoryMB),
				DiskSizeMB:         uint32(row.DiskMB),
				EnvVars:            img.Env,
				EndAt:              time.Now().Add(time.Hour),
				Egress:             substrate.Egress{AllowedCIDRs: []string{"0.0.0.0/0", "::/0"}},
			})
			if err != nil {
				t.Fatalf("Create from build %s: %v", row.CurrentBuildID, err)
			}
			t.Logf("sandbox %s on %s", sb.ID, sb.HostIP)

			r, err := c.Exec(ctx, id, substrate.ExecRequest{
				Args:    []string{"/bin/bash", "-c", "cat /etc/resolv.conf"},
				Timeout: 30 * time.Second,
			})
			if err != nil {
				t.Fatalf("Exec cat /etc/resolv.conf: %v", err)
			}
			firstLine := strings.SplitN(r.Stdout, "\n", 2)[0]
			if firstLine != "nameserver 10.1.0.1" {
				t.Fatalf("/etc/resolv.conf first line = %q, want %q (stdout=%q stderr=%q exit=%d)",
					firstLine, "nameserver 10.1.0.1", r.Stdout, r.Stderr, r.ExitCode)
			}

			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := c.Delete(dctx, id); err != nil {
				t.Fatalf("Delete %s: %v", id, err)
			}
		})
	}
}
