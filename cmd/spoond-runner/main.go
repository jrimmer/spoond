// Command spoond-runner runs a Forgejo Actions runner that executes
// each job in a microVM sandbox obtained from the lease API.
//
// Configuration is read from the environment:
//
//	FORGEJO_URL       Forgejo instance base URL (e.g. https://code.example.com)
//	                   (required)
//	RUNNER_TOKEN       Runner registration token
//	RUNNER_NAME        Runner name prefix (default "spoond-runner")
//	RUNNER_LABELS      Comma-separated labels (default "ubuntu-latest")
//	LEASE_URL          Lease API base URL (default http://127.0.0.1:8890)
//	LEASE_TOKEN        Lease API bearer token
//	IMAGE_MAP          runs-on label -> image tag, comma-separated
//	                   (e.g. "ubuntu-latest=py-base")
//	DEFAULT_IMAGE      Image tag when no label maps (default "py-base")
//	REPO_BASE_URL      Git host base URL for actions/checkout clones
//	                   (no default; checkout fails without it)
//	LEASE_TTL          Sandbox lease TTL seconds (default 600)
//	EXEC_TIMEOUT_SECS  Per-step exec timeout seconds (default 300)
//	RUNNER_ADMIT_WAIT_SECS  Seconds to wait for admission on a full
//	                   node (sent as "wait", #129; default 900, 0 = no
//	                   wait). A create refused for capacity is retried
//	                   for up to RUNNER_JOB_TIMEOUT instead of failing.
//	RUNNER_JOB_TIMEOUT  Whole-job timeout as a Go duration or seconds
//	                   (default 6h = DefaultJobTimeout; 0 = the job's own
//	                   context governs). Bounds the create's capacity retry
//	                   loop, so a node answering 503 forever cannot pin a
//	                   worker.
//	RUNNER_FLOOR       Minimum registered runners (default 3)
//	RUNNER_MAX         Maximum registered runners (default 12)
//	RUNNER_SCALE_STEP  Runners added/removed per scale event (default 3)
//	SCALE_UP_DELAY     All-busy duration before scaling up (default 10s)
//	SCALE_DOWN_DELAY   Idle duration before scaling down (default 60s)
//	RUNNER_STATE_FILE  Path to persist runner UUIDs between restarts
//	                   (default /var/lib/spoond/runner-state.json)
//	RUNNER_STOP_GRACE  How long a shutdown signal lets running jobs
//	                   finish before cancelling them (default 10m); the
//	                   systemd unit's TimeoutStopSec must be above it
//	FORGEJO_ADMIN_TOKEN  Forgejo admin API token for stale runner cleanup.
//	                   If unset, cleanup is skipped.
package spoondrunner

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/runner"
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
		// A bare whole number is seconds (the sibling *_SECS knobs accept
		// both, e.g. RUNNER_JOB_TIMEOUT=3600).
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// jobTimeoutFromEnv is the whole-job bound (RUNNER_JOB_TIMEOUT): a Go
// duration or bare seconds, defaulting to runner.DefaultJobTimeout (6h).
// `0` disables it. It is a named helper so Main's wiring (not just the
// constant) can be pinned by a test (T2).
func jobTimeoutFromEnv() time.Duration {
	return envDurOr("RUNNER_JOB_TIMEOUT", runner.DefaultJobTimeout)
}

// Main runs the runner until SIGTERM/SIGINT (systemd's stop signal is
// SIGTERM). Shutdown is graceful (#119): the pool stops fetching new
// jobs, running jobs get StopGrace (RUNNER_STOP_GRACE) to finish, then
// they are cancelled — each executor's deferred Delete releases its
// job's lease, and Main exits 0 so systemd sees a clean stop.
func Main(args []string) int {
	forgejoURL := os.Getenv("FORGEJO_URL")
	if forgejoURL == "" {
		log.Fatal("FORGEJO_URL is required (the Forgejo instance base URL, e.g. https://code.example.com)")
	}
	token := os.Getenv("RUNNER_TOKEN")
	if token == "" {
		log.Fatal("RUNNER_TOKEN is required")
	}
	name := envOr("RUNNER_NAME", "spoond-runner")
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
	// Per-step exec timeout for CI steps (mix release / cargo builds run
	// tens of minutes). 0 -> executor default (300s).
	stepTimeout := 0
	if v := os.Getenv("EXEC_TIMEOUT_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			stepTimeout = n
		}
	}

	// Admission wait (#129): how long a create may be queued for room on
	// a full node before the runner gives up on it. 0 sends no "wait".
	admitWaitSecs := envIntOr("RUNNER_ADMIT_WAIT_SECS", runner.DefaultAdmitWaitSecs)
	// Whole-job timeout. A capacity refusal is retried until this, so a
	// node answering 503 forever cannot pin a worker indefinitely; the
	// default is 6h. A job's own timeout-minutes is the tighter bound
	// when set.
	jobTimeout := jobTimeoutFromEnv()

	// The pool's own lease client: at start it sweeps this token's
	// orphaned job leases (#119). Per-worker copies (newWorker below)
	// are what Create, Exec and Delete go through.
	leaseClient := runner.NewHTTPLeaseClient(leaseURL, leaseToken)
	leaseClient.SetAdmitWait(admitWaitSecs)

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
		StateFile:      envOr("RUNNER_STATE_FILE", "/var/lib/spoond/runner-state.json"),
		AdminToken:     os.Getenv("FORGEJO_ADMIN_TOKEN"),
		ForgejoURL:     forgejoURL,
		Leases:         leaseClient,
		StopGrace:      envDurOr("RUNNER_STOP_GRACE", runner.DefaultStopGrace),
	}

	// Start metrics endpoint (issue #20). It lives and dies with the
	// process: served until Main's shutdown begins, then closed so no
	// goroutine outlives the run.
	metricsListen := envOr("METRICS_LISTEN", "")
	var runnerMetrics *metrics.RunnerMetrics
	var metricsSrv *http.Server
	if metricsListen != "" {
		runnerMetrics = metrics.NewRunnerMetrics()
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(runnerMetrics.Registry, promhttp.HandlerOpts{}))
		metricsSrv = &http.Server{Addr: metricsListen, Handler: mux}
		ln, err := net.Listen("tcp", metricsListen)
		if err != nil {
			log.Printf("runner metrics: %v", err)
			metricsSrv = nil
		} else {
			log.Printf("runner metrics on %s", metricsListen)
			go func() {
				if err := metricsSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
					log.Printf("runner metrics: %v", err)
				}
			}()
		}
	}

	// newWorker builds a fresh ForgejoAdapter + Executor per worker so
	// each registered runner has its own auth headers and job loop.
	newWorker := func() runner.RunnerWorker {
		proto := runner.NewForgejoAdapterWithInternal(forgejoURL, envOr("REPO_BASE_URL", ""), nil)
		lease := runner.NewHTTPLeaseClient(leaseURL, leaseToken)
		lease.SetAdmitWait(admitWaitSecs)
		// Keep the lease client's HTTP timeout above the per-step exec
		// timeout, or long CI steps die at the client's own cap.
		if stepTimeout > 0 {
			lease.SetHTTPTimeout(time.Duration(stepTimeout+120) * time.Second)
		}
		lease.NetPolicy = envOr("LEASE_NETPOL", "internet")
		if v := os.Getenv("LEASE_NET_ALLOW"); v != "" {
			lease.NetAllow = strings.Split(v, ",")
			for i, a := range lease.NetAllow {
				lease.NetAllow[i] = strings.TrimSpace(a)
			}
		}
		exec := &runner.Executor{
			Sandbox:      lease,
			Metrics:      runnerMetrics,
			Sink:         proto,
			Labels:       imageMap,
			DefaultImage: defaultImage,
			TTL:          ttl,
			JobTimeout:   jobTimeout,
			RepoBaseURL:  envOr("REPO_BASE_URL", ""),
			StepTimeout:  stepTimeout,
			RecordDir:    envOr("JOB_RECORD_DIR", "/var/lib/spoond/jobs"),
			// #119: the job lease's comment names the job and links to
			// it ("forgejo job <id> <url>"); the orphan sweep keys on it.
			ForgejoURL: forgejoURL,
		}
		return &runner.WorkerImpl{Adapter: proto, Exec: exec}
	}

	// The signal context is the whole process's lifetime: SIGTERM or
	// SIGINT (Ctrl-C) starts the graceful drain below. It governs the
	// pool's own loops (fetching, scaling) — the running jobs hang off
	// the pool's jobs context instead, so the signal cannot kill them:
	// Stop lets them finish for RUNNER_STOP_GRACE before it cancels
	// them (#119).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool := runner.NewRunnerPool(poolCfg, newWorker, name, token, labels)
	log.Printf("starting adaptive runner pool: floor=%d max=%d step=%d", poolCfg.Floor, poolCfg.Max, poolCfg.ScaleStep)
	pool.Start(ctx)

	// Block until a shutdown signal arrives (or the context is
	// otherwise cancelled), then drain.
	<-ctx.Done()
	log.Printf("shutdown signal received; draining runner pool")

	pool.Stop()
	if metricsSrv != nil {
		metricsSrv.Close()
	}
	log.Printf("runner stopped")
	return 0
}
