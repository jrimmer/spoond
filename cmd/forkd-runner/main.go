// Command forkd-runner runs a Forgejo Actions runner that executes
// each job in a forkd sandbox obtained from the lease API.
//
// Configuration is read from the environment:
//
//	FORGEJO_URL       Forgejo instance base URL (e.g. https://code.lacy.casa)
//	RUNNER_TOKEN       Runner registration token
//	RUNNER_NAME        Runner name (default "forkd-runner")
//	RUNNER_LABELS      Comma-separated labels (default "ubuntu-latest")
//	LEASE_URL          Lease API base URL (default http://127.0.0.1:8890)
//	LEASE_TOKEN        Lease API bearer token
//	IMAGE_MAP          runs-on label -> image tag, comma-separated
//	                   (e.g. "ubuntu-latest=py-base")
//	DEFAULT_IMAGE      Image tag when no label maps (default "py-base")
//	LEASE_TTL          Sandbox lease TTL seconds (default 600)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jrimmer/hyper-forgejo-runner/runner"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	forgejoURL := envOr("FORGEJO_URL", "https://code.lacy.casa")
	token := os.Getenv("RUNNER_TOKEN")
	if token == "" {
		log.Fatal("RUNNER_TOKEN is required")
	}
	name := envOr("RUNNER_NAME", "forkd-runner")
	labels := strings.Split(envOr("RUNNER_LABELS", "ubuntu-latest"), ",")
	leaseURL := envOr("LEASE_URL", "http://127.0.0.1:8890")
	leaseToken := os.Getenv("LEASE_TOKEN")
	if leaseToken == "" {
		log.Fatal("LEASE_TOKEN is required")
	}
	defaultImage := envOr("DEFAULT_IMAGE", "py-base")
	ttl := 600
	if v := os.Getenv("LEASE_TTL"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			ttl = n
		}
	}

	// Parse image map.
	imageMap := map[string]string{}
	if v := os.Getenv("IMAGE_MAP"); v != "" {
		for _, pair := range strings.Split(v, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				imageMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	proto := runner.NewForgejoAdapter(forgejoURL, nil)
	lease := runner.NewHTTPLeaseClient(leaseURL, leaseToken)
	exec := &runner.Executor{
		Sandbox:      lease,
		Sink:         proto,
		Labels:       imageMap,
		DefaultImage: defaultImage,
		TTL:          ttl,
	}

	ctx := context.Background()
	runnerID, err := proto.Register(ctx, name, token, labels, true)
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	log.Printf("registered runner %s (id=%d)", name, runnerID)

	var tasksVersion int64
	for {
		job, newVersion, err := proto.Fetch(ctx, tasksVersion)
		if err != nil {
			log.Printf("fetch task: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		tasksVersion = newVersion
		if job == nil {
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("executing job %d", job.ID)
		if err := exec.Run(ctx, job); err != nil {
			log.Printf("job %d failed: %v", job.ID, err)
		}
	}
}
