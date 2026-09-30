//go:build conformance

package conformance

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestI1_ImagesListed checks that every configured image is listed.
func TestI1_ImagesListed(t *testing.T) {
	begin(t)
	st, body, err := cl.images("")
	if err != nil {
		failf(t, "images: %v", err)
	}
	if st != 200 {
		failf(t, "images: status %d: %s", st, truncate(body))
	}
	var resp struct {
		Images []string `json:"images"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		failf(t, "images: bad body: %v", err)
	}
	have := map[string]bool{}
	for _, name := range resp.Images {
		have[name] = true
	}
	for _, name := range cfg.Images {
		if !have[name] {
			failf(t, "images lacks %q (have %v)", name, resp.Images)
		}
	}
}

// TestI2_RealWorkloads runs a real tool in every workload image.
func TestI2_RealWorkloads(t *testing.T) {
	begin(t)

	elixir := createLease(t, map[string]any{"image": "elixir-release", "ttl": 600})
	execOK(t, elixir.ID, "mix --version")
	execOK(t, elixir.ID, "cargo --version")
	execOK(t, elixir.ID, "pnpm --version")

	golang := createLease(t, map[string]any{"image": "go-base", "ttl": 600})
	if out := execOK(t, golang.ID, "go version"); !strings.Contains(out, "go1.27") {
		failf(t, "go version = %q, want go1.27", out)
	}

	dev := createLease(t, map[string]any{"image": "dev-base", "ttl": 600})
	execOK(t, dev.ID, "tmux -V")
	execOK(t, dev.ID, "sshd -V 2>&1")

	py := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	execOK(t, py.ID, "python3 --version")
}

// TestI3_DockerInDocker runs Docker inside dev-base. e2b only; a failure
// caused by guest kernel features is recorded and decided in U12.
func TestI3_DockerInDocker(t *testing.T) {
	begin(t)
	requireE2B(t)

	l := createLease(t, map[string]any{"image": "dev-base", "ttl": 600, "network_policy": "internet"})
	st, body, err := cl.exec(l.ID, execReq{
		Cmd:     "apt-get install -y docker.io && (dockerd >/tmp/d.log 2>&1 &) && sleep 8 && docker run --rm hello-world",
		Timeout: 300,
	})
	if err != nil {
		failf(t, "docker: %v", err)
	}
	if st != 200 {
		failf(t, "docker: status %d: %s", st, truncate(body))
	}
	var out execResult
	if err := json.Unmarshal(body, &out); err != nil {
		failf(t, "docker: bad body: %v", err)
	}
	if !strings.Contains(out.Stdout+out.Stderr, "Hello from Docker!") {
		failf(t, "docker run hello-world output lacks \"Hello from Docker!\" (exit %d, stderr %s)", out.Exit, truncate([]byte(out.Stderr)))
	}
}
