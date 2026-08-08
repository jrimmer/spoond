// Command forkd-runner runs a Forgejo Actions runner that executes
// each job in a forkd sandbox obtained from the lease API.
//
// Configuration is read from the environment:
//
//	FORGEJO_URL       Forgejo instance base URL (e.g. https://code.lacy.casa)
//	RUNNER_TOKEN       Runner registration token
//	RUNNER_NAME        Runner name prefix (default "forkd-runner")
//	RUNNER_LABELS      Comma-separated labels (default "ubuntu-latest")
//	LEASE_URL          Lease API base URL (default http://127.0.0.1:8890)
//	LEASE_TOKEN        Lease API bearer token
//	IMAGE_MAP          runs-on label -> image tag, comma-separated
//	                   (e.g. "ubuntu-latest=py-base")
//	DEFAULT_IMAGE      Image tag when no label maps (default "py-base")
//	REPO_BASE_URL      Git host base URL for actions/checkout clones
//	                   (default https://code.lacy.casa)
//	LEASE_TTL          Sandbox lease TTL seconds (default 600)
//	RUNNER_FLOOR       Minimum registered runners (default 3)
//	RUNNER_MAX         Maximum registered runners (default 12)
//	RUNNER_SCALE_STEP  Runners added/removed per scale event (default 3)
//	SCALE_UP_DELAY     All-busy duration before scaling up (default 10s)
//	SCALE_DOWN_DELAY   Idle duration before scaling down (default 60s)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jrimmer/forkd-service/runner"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envDurOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
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

	// Adaptive pool config.
	poolCfg := runner.PoolConfig{
		Floor:          envIntOr("RUNNER_FLOOR", 3),
		Max:            envIntOr("RUNNER_MAX", 12),
		ScaleStep:      envIntOr("RUNNER_SCALE_STEP", 3),
		ScaleUpDelay:   envDurOr("SCALE_UP_DELAY", 10*time.Second),
		ScaleDownDelay: envDurOr("SCALE_DOWN_DELAY", 60*time.Second),
		PollInterval:   5 * time.Second,
	}

	// newWorker builds a fresh ForgejoAdapter + Executor per worker so
	// each registered runner has its own auth headers and job loop.
	newWorker := func() runner.RunnerWorker {
		proto := runner.NewForgejoAdapterWithInternal(forgejoURL, envOr("REPO_BASE_URL", ""), nil)
		lease := runner.NewHTTPLeaseClient(leaseURL, leaseToken)
		exec := &runner.Executor{
			Sandbox:      lease,
			Sink:         proto,
			Labels:       imageMap,
			DefaultImage: defaultImage,
			TTL:          ttl,
			RepoBaseURL:  envOr("REPO_BASE_URL", "https://code.lacy.casa"),
		}
		return &runner.WorkerImpl{Adapter: proto, Exec: exec}
	}

	ctx := context.Background()
	pool := runner.NewRunnerPool(poolCfg, newWorker, name, token, labels)
	log.Printf("starting adaptive runner pool: floor=%d max=%d step=%d", poolCfg.Floor, poolCfg.Max, poolCfg.ScaleStep)
	pool.Start(ctx)

	// Block forever; workers run in goroutines.
	select {}
}
