// Package metrics defines the Prometheus metric contracts for all spoond
// services (issue #20). Each service (backend, SSH gateway, CI runner)
// owns its own registry and exposes a /metrics endpoint; the backend
// additionally merges namespaced controller passthrough metrics.
//
// Metric naming: spoond_* for service-owned, spoond_controller_* for
// raw forkd-controller passthrough (renamed from forkd_* to avoid
// collision with service semantics).
package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// ---------- Backend metrics ----------

// BackendMetrics holds all backend-owned metrics. They are registered
// on a dedicated registry so handleMetrics can emit service-owned
// metrics alongside the namespaced controller passthrough.
type BackendMetrics struct {
	Registry *prometheus.Registry

	// Pool
	PoolReady      *prometheus.GaugeVec     // {image}: warm VMs available
	PoolCap        prometheus.Gauge         // POOL_SIZE × len(KNOWN_IMAGES)
	PoolRefill     *prometheus.CounterVec   // {image}: refill events
	PoolRefillFail *prometheus.CounterVec   // {image}: refill failures
	PoolRefillDur  *prometheus.HistogramVec // {image}: refill duration
	PoolEvicted    *prometheus.CounterVec   // {image,reason}: evictions

	// Leases
	LeasesActive  prometheus.Gauge       // leased (non-warm) sandboxes
	LeasesQueued  prometheus.Gauge       // demand waiting for a slot
	LeasesTotal   prometheus.Counter     // cumulative leases granted
	LeaseGrantDur prometheus.Histogram   // time from request to ready
	LeaseOps      *prometheus.CounterVec // {op}: suspend, resume, restart, clone, keepalive
	LeaseSwept    prometheus.Counter     // TTL-expired leases swept
	LeaseOrphaned prometheus.Counter     // orphans detected on startup

	// API health
	HTTPReqs *prometheus.CounterVec   // {path,method,code}: API usage
	HTTPDur  *prometheus.HistogramVec // {path}: API latency

	// LLM gateway
	LLMReqs       *prometheus.CounterVec   // {provider}: requests
	LLMDur        *prometheus.HistogramVec // {provider}: upstream latency
	LLMErrors     *prometheus.CounterVec   // {provider,code}: upstream errors
	LLMRateLimit  prometheus.Counter       // 429 from per-user cap
	LLMKeyFail    prometheus.Counter       // LLM key auth failures
	LLMKeylessDen prometheus.Counter       // keyless denied (requireKey)
	LLMInflight   *prometheus.GaugeVec     // {owner}: in-flight per owner

	// Proxy
	ProxyReqs    *prometheus.CounterVec // {host}: proxy requests
	ProxyAuthRes *prometheus.CounterVec // {result}: forward-auth results

	// Network policy
	NetpolApply  *prometheus.CounterVec // {policy}: apply events
	NetpolErrors prometheus.Counter     // apply failures
	NetpolDur    prometheus.Histogram   // apply latency

	// Identity & security
	IdentityUsers  *prometheus.GaugeVec // {kind}: user count
	IdentityAdmins prometheus.Gauge     // admin count
	AuthFailures   prometheus.Counter   // cumulative auth failures
	AuthThrottled  prometheus.Counter   // 429s from rate limiter
	QuotaExceeded  prometheus.Counter   // quota rejections
	QuotaReserved  prometheus.Gauge     // pending reservations
	SharesActive   prometheus.Gauge     // active share grants
	BusySlots      *prometheus.GaugeVec // {owner}: exec/stream concurrency

	// Builds (image bake)
	BuildsInFlight prometheus.Gauge   // active bakes
	BuildsFailed   prometheus.Counter // cumulative bake failures

	// Records (issue #55)
	RecordCreated prometheus.Counter // cumulative record checkpoints started
	RecordReplay  prometheus.Counter // cumulative replay grants
	RecordsActive prometheus.Gauge   // open (un-stopped) run records
}

// NewBackendMetrics creates and registers all backend metrics on a
// fresh prometheus.Registry.
func NewBackendMetrics() *BackendMetrics {
	reg := prometheus.NewRegistry()
	m := &BackendMetrics{
		Registry: reg,
	}

	// Pool
	m.PoolReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "pool_ready",
		Help: "Warm VMs available per image.",
	}, []string{"image"})
	m.PoolCap = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "pool_cap",
		Help: "Designed warm-pool size (POOL_SIZE × len(KNOWN_IMAGES)).",
	})
	m.PoolRefill = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "pool_refill_total",
		Help: "Pool refill events per image.",
	}, []string{"image"})
	m.PoolRefillFail = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "pool_refill_failed_total",
		Help: "Pool refill failures per image.",
	}, []string{"image"})
	m.PoolRefillDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "pool_refill_duration_seconds",
		Help:    "Time to spin up one warm VM.",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 10), // 0.5s → 256s
	}, []string{"image"})
	m.PoolEvicted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "pool_evicted_total",
		Help: "Pool evictions by image and reason.",
	}, []string{"image", "reason"})

	// Leases
	m.LeasesActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "leases_active",
		Help: "Leased (non-warm) sandboxes.",
	})
	m.LeasesQueued = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "leases_queued",
		Help: "Demand waiting for a slot — the real capacity-pressure signal.",
	})
	m.LeasesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "leases_total",
		Help: "Cumulative leases granted.",
	})
	m.LeaseGrantDur = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "lease_grant_duration_seconds",
		Help:    "Time from lease request to sandbox ready.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12), // 0.1s → ~7min
	})
	m.LeaseOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "lease_operations_total",
		Help: "Lifecycle operations: suspend, resume, restart, clone, keepalive.",
	}, []string{"op"})
	m.LeaseSwept = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "lease_swept_total",
		Help: "TTL-expired leases swept.",
	})
	m.LeaseOrphaned = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "lease_orphaned_total",
		Help: "Orphaned leases detected on startup reconciliation.",
	})

	// API health
	m.HTTPReqs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "http_requests_total",
		Help: "API requests by path, method, and status code.",
	}, []string{"path", "method", "code"})
	m.HTTPDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "http_request_duration_seconds",
		Help:    "API request latency by path.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})

	// LLM gateway
	m.LLMReqs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "llm_requests_total",
		Help: "LLM gateway requests by provider.",
	}, []string{"provider"})
	m.LLMDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "llm_request_duration_seconds",
		Help:    "LLM upstream request latency by provider.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
	}, []string{"provider"})
	m.LLMErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "llm_errors_total",
		Help: "LLM upstream errors by provider and HTTP code.",
	}, []string{"provider", "code"})
	m.LLMRateLimit = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "llm_rate_limited_total",
		Help: "429s from per-user concurrent request cap.",
	})
	m.LLMKeyFail = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "llm_key_auth_failures_total",
		Help: "LLM key authentication failures.",
	})
	m.LLMKeylessDen = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "llm_keyless_denied_total",
		Help: "Keyless owners denied in requireKey mode.",
	})
	m.LLMInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "llm_inflight",
		Help: "In-flight LLM requests per owner.",
	}, []string{"owner"})

	// Proxy
	m.ProxyReqs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "proxy_requests_total",
		Help: "Proxy requests by hostname.",
	}, []string{"host"})
	m.ProxyAuthRes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "proxy_forward_auth_total",
		Help: "Forward-auth results.",
	}, []string{"result"})

	// Network policy
	m.NetpolApply = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "netpol_apply_total",
		Help: "Network policy apply events by policy.",
	}, []string{"policy"})
	m.NetpolErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "netpol_apply_errors_total",
		Help: "Network policy apply failures.",
	})
	m.NetpolDur = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "netpol_apply_duration_seconds",
		Help:    "Iptables rule installation latency.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
	})

	// Identity & security
	m.IdentityUsers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "identity_users",
		Help: "Registered users by kind.",
	}, []string{"kind"})
	m.IdentityAdmins = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "identity_admins",
		Help: "Admin user count.",
	})
	m.AuthFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "auth_failures_total",
		Help: "Cumulative authentication failures.",
	})
	m.AuthThrottled = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "auth_throttled_total",
		Help: "Requests rejected by auth rate limiter (429).",
	})
	m.QuotaExceeded = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "quota_exceeded_total",
		Help: "Lease quota rejections.",
	})
	m.QuotaReserved = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "quota_reservations",
		Help: "Pending lease quota reservations.",
	})
	m.SharesActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "shares_active",
		Help: "Active share grants.",
	})
	m.BusySlots = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "busy_slots",
		Help: "Per-owner exec/stream concurrency (cap 8).",
	}, []string{"owner"})

	// Builds
	m.BuildsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "builds_in_flight",
		Help: "Active image bakes.",
	})
	m.BuildsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "builds_failed_total",
		Help: "Cumulative image bake failures.",
	})

	// Records
	m.RecordCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "record_created_total",
		Help: "Cumulative run-record checkpoints started.",
	})
	m.RecordReplay = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "record_replay_total",
		Help: "Cumulative replay grants from a recorded state.",
	})
	m.RecordsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "records_active",
		Help: "Open (started but not stopped) run records.",
	})

	// Register all
	reg.MustRegister(
		m.PoolReady, m.PoolCap, m.PoolRefill, m.PoolRefillFail,
		m.PoolRefillDur, m.PoolEvicted,
		m.LeasesActive, m.LeasesQueued, m.LeasesTotal, m.LeaseGrantDur,
		m.LeaseOps, m.LeaseSwept, m.LeaseOrphaned,
		m.HTTPReqs, m.HTTPDur,
		m.LLMReqs, m.LLMDur, m.LLMErrors, m.LLMRateLimit,
		m.LLMKeyFail, m.LLMKeylessDen, m.LLMInflight,
		m.ProxyReqs, m.ProxyAuthRes,
		m.NetpolApply, m.NetpolErrors, m.NetpolDur,
		m.IdentityUsers, m.IdentityAdmins, m.AuthFailures,
		m.AuthThrottled, m.QuotaExceeded, m.QuotaReserved,
		m.SharesActive, m.BusySlots,
		m.BuildsInFlight, m.BuildsFailed,
		m.RecordCreated, m.RecordReplay, m.RecordsActive,
	)
	return m
}

// ---------- Gateway metrics ----------

// GatewayMetrics holds SSH gateway metrics. Served on a separate
// /metrics endpoint (default :2223).
type GatewayMetrics struct {
	Registry *prometheus.Registry

	SessionsActive   *prometheus.GaugeVec   // {image}: active SSH sessions
	ConnectionsTotal prometheus.Counter     // cumulative SSH connections
	AuthFailures     *prometheus.CounterVec // {reason}: auth failures
	SessionDur       prometheus.Histogram   // session duration
	CtlCommands      *prometheus.CounterVec // {verb}: ctl command usage
	CreateTotal      *prometheus.CounterVec // {image}: sandboxes created via SSH
	ImageResolution  *prometheus.CounterVec // {source}: alias, dynamic, extra
}

// NewGatewayMetrics creates and registers gateway metrics.
func NewGatewayMetrics() *GatewayMetrics {
	reg := prometheus.NewRegistry()
	m := &GatewayMetrics{
		Registry: reg,
		SessionsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "spoond", Name: "ssh_sessions_active",
			Help: "Active SSH sessions by image.",
		}, []string{"image"}),
		ConnectionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "spoond", Name: "ssh_connections_total",
			Help: "Cumulative SSH connections.",
		}),
		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "spoond", Name: "ssh_auth_failures_total",
			Help: "SSH auth failures by reason.",
		}, []string{"reason"}),
		SessionDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "spoond", Name: "ssh_session_duration_seconds",
			Help:    "SSH session duration.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 15), // 1s → ~4.5h
		}),
		CtlCommands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "spoond", Name: "ssh_ctl_commands_total",
			Help: "Control plane command usage by verb.",
		}, []string{"verb"}),
		CreateTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "spoond", Name: "ssh_create_total",
			Help: "Sandboxes created via SSH new-<image> by image.",
		}, []string{"image"}),
		ImageResolution: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "spoond", Name: "ssh_image_resolution_total",
			Help: "Image resolution by source: alias, dynamic, extra.",
		}, []string{"source"}),
	}
	reg.MustRegister(
		m.SessionsActive, m.ConnectionsTotal, m.AuthFailures,
		m.SessionDur, m.CtlCommands, m.CreateTotal, m.ImageResolution,
	)
	return m
}

// ---------- Runner metrics ----------

// RunnerMetrics holds CI runner metrics. Served on a separate
// /metrics endpoint (default :8892).
type RunnerMetrics struct {
	Registry *prometheus.Registry

	JobsActive        prometheus.Gauge       // jobs currently executing
	JobsTotal         *prometheus.CounterVec // {result}: success, failure
	JobDur            prometheus.Histogram   // end-to-end job time
	ExecRetries       prometheus.Counter     // exec retry attempts
	ExecErrors        *prometheus.CounterVec // {code}: exec errors by HTTP status
	CheckoutDur       prometheus.Histogram   // git checkout latency
	SandboxCreateFail prometheus.Counter     // sandbox creation failures
}

// NewRunnerMetrics creates and registers runner metrics.
func NewRunnerMetrics() *RunnerMetrics {
	reg := prometheus.NewRegistry()
	m := &RunnerMetrics{
		Registry: reg,
		JobsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "spoond", Name: "runner_jobs_active",
			Help: "CI jobs currently executing.",
		}),
		JobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "spoond", Name: "runner_jobs_total",
			Help: "Cumulative CI jobs by result.",
		}, []string{"result"}),
		JobDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "spoond", Name: "runner_job_duration_seconds",
			Help:    "End-to-end CI job time.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 15), // 1s → ~4.5h
		}),
		ExecRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "spoond", Name: "runner_exec_retries_total",
			Help: "Exec retry attempts — spikes indicate backend instability.",
		}),
		ExecErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "spoond", Name: "runner_exec_errors_total",
			Help: "Exec errors by HTTP status code.",
		}, []string{"code"}),
		CheckoutDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "spoond", Name: "runner_checkout_duration_seconds",
			Help:    "Git checkout latency.",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 12),
		}),
		SandboxCreateFail: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "spoond", Name: "runner_sandbox_create_failed_total",
			Help: "Sandbox creation failures.",
		}),
	}
	reg.MustRegister(
		m.JobsActive, m.JobsTotal, m.JobDur,
		m.ExecRetries, m.ExecErrors, m.CheckoutDur, m.SandboxCreateFail,
	)
	return m
}

// NamespaceControllerMetrics rewrites forkd_ controller metrics to
// spoond_controller_ so service vs controller semantics never collide
// on the same scrape target. The input is the raw Prometheus text
// format from forkd-controller's /metrics endpoint.
func NamespaceControllerMetrics(raw []byte) string {
	// Rewrite metric names: forkd_sandboxes_active → spoond_controller_sandboxes_active, etc.
	// Only rewrite lines that start with forkd_ (metric names or HELP/TYPE comments).
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "forkd_") || strings.HasPrefix(line, "# HELP forkd_") || strings.HasPrefix(line, "# TYPE forkd_") {
			lines[i] = strings.Replace(line, "forkd_", "spoond_controller_", 1)
		}
	}
	return strings.Join(lines, "\n")
}
