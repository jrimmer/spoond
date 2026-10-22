//go:build e2blive

// Live check per image against the orchestrator on the E2B host. Run with:
//
//	SPOOND_IMAGES_MANIFEST=/path/to/manifest.yaml \
//	  go test -tags e2blive -count=1 -timeout 60m ./cmd/spoond-images/
//
// from a spoond checkout, with the E2B_* environment set (FromEnv
// defaults). spoond ships no image catalog, so SPOOND_IMAGES_MANIFEST
// names the manifest the host's images were built from (an absolute
// path: go test runs in the package directory); unset, the test skips.
// For every baked manifest image it creates a sandbox from
// the image's current_build_id with the manifest env, runs `cat
// /etc/resolv.conf`, checks every comma-separated address in
// $SPOOND_GUEST_DNS_ADDR has a `nameserver` line (skipped when that is
// unset), and deletes the sandbox.
package spoondimages

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/store"
	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/e2b"
)

func TestLiveImages(t *testing.T) {
	ctx := context.Background()

	manifestPath := os.Getenv("SPOOND_IMAGES_MANIFEST")
	if manifestPath == "" {
		t.Skip("SPOOND_IMAGES_MANIFEST not set: point it at the manifest the host's images were built from")
	}
	m, err := loadManifest(manifestPath)
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
			stdout := r.Stdout
			want := envOr("SPOOND_GUEST_DNS_ADDR", "")
			if want == "" {
				t.Skip("SPOOND_GUEST_DNS_ADDR not set; resolver check skipped")
			}
			for _, addr := range strings.Split(want, ",") {
				addr = strings.TrimSpace(addr)
				if addr == "" {
					continue
				}
				line := "nameserver " + addr
				if !strings.Contains(stdout, line+"\n") {
					t.Fatalf("/etc/resolv.conf lacks %q (stdout=%q stderr=%q exit=%d)",
						line, stdout, r.Stderr, r.ExitCode)
				}
			}

			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := c.Delete(dctx, id); err != nil {
				t.Fatalf("Delete %s: %v", id, err)
			}
		})
	}
}
