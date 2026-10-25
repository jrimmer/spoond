// Command spoond-runner runs a Forgejo Actions runner that executes
// each job in a microVM sandbox obtained from the lease API.
//
// Configuration is read from the environment:
//
//	FORGEJO_URL       Forgejo instance base URL (e.g. https://code.example.com)
//	                   (required)
//	RUNNER_TOKEN       Runner registration token (required; or
//	                   RUNNER_TOKEN_FILE naming a 0600 file that carries
//	                   it — #137 C2; setting both forms, or naming an
//	                   unreadable file, fails startup, like a missing
//	                   token)
//	RUNNER_NAME        Runner name prefix (default "spoond-runner")
//	RUNNER_LABELS      Comma-separated labels (default "ubuntu-latest")
//	LEASE_URL          Lease API base URL (default http://127.0.0.1:8890)
//	LEASE_TOKEN        Lease API bearer token (required; or
//	                   LEASE_TOKEN_FILE, same file rules)
//	LEASE_TOKEN_MAP    Per-project lease identity, comma-separated
//	                   KEY=PATH (e.g. "acme/web=/etc/spoond/web.token,
//	                   example=/etc/spoond/example.token"): a job from
//	                   repository KEY ("owner/repo"), or from any
//	                   repository of owner KEY ("owner"), leases with
//	                   the token in the file at PATH, so fair shares
//	                   count that project's CI against its own
//	                   identity. The repository's entry beats its
//	                   owner's; keys match case-insensitively; PATH is
//	                   an absolute path (never the token itself), read
//	                   by LEASE_TOKEN_FILE's rules. Anything unmapped
//	                   uses LEASE_TOKEN. A malformed entry or an
//	                   unreadable file fails startup, naming the entry.
//	                   Unset: every job uses LEASE_TOKEN
//	IMAGE_MAP          runs-on label -> image tag, comma-separated
//	                   (e.g. "ubuntu-latest=ci-image"); a value
//	                   "snapshot:NAME[@V]" starts the job from the
//	                   runner identity's named snapshot instead, and
//	                   "snapshot:OWNER:NAME[@V]" from another user's
//	                   through a grant (e.g. "scout-web=snapshot:scout/node24")
//	DEFAULT_IMAGE      Image tag (or "snapshot:...") when no label maps
//	                   (no default; a job whose labels map through
//	                   neither this nor IMAGE_MAP fails, and a runner
//	                   with both unset warns at startup)
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

	"github.com/jrimmer/spoond/v4/metrics"
	"github.com/jrimmer/spoond/v4/runner"
	"github.com/jrimmer/spoond/v4/settings"
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

// imageConfigWarning is the startup warning for a runner with no image
// configured at all: neither IMAGE_MAP nor DEFAULT_IMAGE names one, so
// every job it takes fails before a sandbox is created. spoond ships no
// default image. "" when either is set.
func imageConfigWarning(imageMap map[string]string, defaultImage string) string {
	if len(imageMap) > 0 || defaultImage != "" {
		return ""
	}
	return "neither IMAGE_MAP nor DEFAULT_IMAGE is set; every job will fail until one names an image (e.g. IMAGE_MAP=ubuntu-latest=ci-image or DEFAULT_IMAGE=ci-image)"
}

// orphanSweeper is the pool's startup sweep (#119): LEASE_TOKEN's
// client, plus one per distinct LEASE_TOKEN_MAP token (a mapped token
// equal to LEASE_TOKEN is swept once). A named helper so the wiring —
// every identity swept, not just LEASE_TOKEN — can be pinned by a test.
func orphanSweeper(leaseURL, leaseToken string, admitWaitSecs int, tokenMap *runner.LeaseTokenMap) runner.LeaseSweeper {
	leaseClient := runner.NewHTTPLeaseClient(leaseURL, leaseToken)
	leaseClient.SetAdmitWait(admitWaitSecs)
	ids := tokenMap.Identities()
	if len(ids) == 0 {
		return leaseClient
	}
	targets := runner.MultiSweeper{{Name: "LEASE_TOKEN", Sweeper: leaseClient}}
	for _, id := range ids {
		if id.Token == leaseToken {
			continue // swept as LEASE_TOKEN already
		}
		c := runner.NewHTTPLeaseClient(leaseURL, id.Token)
		c.SetAdmitWait(admitWaitSecs)
		targets = append(targets, runner.SweepTarget{Name: "LEASE_TOKEN_MAP " + strings.Join(id.Keys, ","), Sweeper: c})
	}
	return targets
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
	// The runner's secrets (#137 C2) resolve through settings.Get:
	// NAME= in the environment, or NAME_FILE= naming a value file (0600,
	// content with trailing newlines trimmed). Both forms set, or an
	// unreadable file, is the startup error, like a missing token.
	token, warn, err := settings.Resolve("RUNNER_TOKEN")
	if err != nil {
		log.Fatal(err)
	}
	if warn != "" {
		log.Printf("warning: %s", warn)
	}
	if token == "" {
		log.Fatal("RUNNER_TOKEN is required (or RUNNER_TOKEN_FILE)")
	}
	leaseToken, warn, err := settings.Resolve("LEASE_TOKEN")
	if err != nil {
		log.Fatal(err)
	}
	if warn != "" {
		log.Printf("warning: %s", warn)
	}
	if leaseToken == "" {
		log.Fatal("LEASE_TOKEN is required (or LEASE_TOKEN_FILE)")
	}
	// Per-project lease identities: values are token file paths, read by
	// LEASE_TOKEN_FILE's rules. Only the keys are ever logged.
	tokenMap, warns, err := runner.ParseLeaseTokenMap(os.Getenv("LEASE_TOKEN_MAP"), os.ReadFile)
	if err != nil {
		log.Fatal(err)
	}
	for _, w := range warns {
		log.Printf("warning: %s", w)
	}
	if keys := tokenMap.Keys(); len(keys) > 0 {
		log.Printf("lease tokens: %d mapped (%s)", len(keys), strings.Join(keys, ", "))
	}
	// The Forgejo admin token is optional (the adaptive pool's admin
	// surface); it takes the file form too (#137 C2).
	adminToken, warn, err := settings.Resolve("FORGEJO_ADMIN_TOKEN")
	if err != nil {
		log.Fatal(err)
	}
	if warn != "" {
		log.Printf("warning: %s", warn)
	}
	name := envOr("RUNNER_NAME", "spoond-runner")
	// Trimmed, empty entries dropped: Register and Declare send the
	// list as is, so "a, b" or a trailing comma would reach Forgejo as
	// " b" or "".
	var labels []string
	for _, l := range strings.Split(envOr("RUNNER_LABELS", "ubuntu-latest"), ",") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}
	if len(labels) == 0 {
		labels = []string{"ubuntu-latest"}
	}
	leaseURL := envOr("LEASE_URL", "http://127.0.0.1:8890")
	defaultImage := os.Getenv("DEFAULT_IMAGE")
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
	// default is 6h. A job's own timeout-minutes bounds only its
	// execution, from sandbox creation, and is capped by this.
	jobTimeout := jobTimeoutFromEnv()

	// The pool's own lease clients: at start they sweep the orphaned job
	// leases (#119) of LEASE_TOKEN and of every LEASE_TOKEN_MAP token, as
	// a previous process may have left some under any of them (the sweep
	// deletes only runner job leases, so a project's other leases under
	// its token stay). Per-worker copies (newWorker below) are what
	// Create, Exec and Delete go through.
	sweeper := orphanSweeper(leaseURL, leaseToken, admitWaitSecs, tokenMap)

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
	if warn := imageConfigWarning(imageMap, defaultImage); warn != "" {
		log.Printf("warning: %s", warn)
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
		AdminToken:     adminToken,
		ForgejoURL:     forgejoURL,
		Leases:         sweeper,
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

	// newLease builds one worker lease client for a token; every lease
	// identity's client gets the same settings.
	newLease := func(token string) *runner.HTTPLeaseClient {
		lease := runner.NewHTTPLeaseClient(leaseURL, token)
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
		return lease
	}
	// newWorker builds a fresh ForgejoAdapter + Executor per worker so
	// each registered runner has its own auth headers and job loop (and
	// its own lease client per identity).
	newWorker := func() runner.RunnerWorker {
		proto := runner.NewForgejoAdapterWithInternal(forgejoURL, envOr("REPO_BASE_URL", ""), nil)
		exec := &runner.Executor{
			Sandbox:      newLease(leaseToken),
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
			// LEASE_TOKEN_MAP: a mapped repository's job leases through
			// this worker's own client for its token (nil when unset).
			SandboxFor: tokenMap.Router(func(token string) runner.SandboxProvider { return newLease(token) }),
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
