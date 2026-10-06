// Package metrics defines the Prometheus metric contracts for all spoond
// services (issue #20). Each service (backend, SSH gateway, CI runner)
// owns its own registry and exposes a /metrics endpoint; the backend
// additionally appends the orchestrator's collector output verbatim.
//
// Metric naming: spoond_* for service-owned metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

// ---------- Backend metrics ----------

// BackendMetrics holds all backend-owned metrics. They are registered
// on a dedicated registry so handleMetrics can emit service-owned
// metrics alongside the orchestrator passthrough.
type BackendMetrics struct {
	Registry *prometheus.Registry

	// Pool
	PoolReady      *prometheus.GaugeVec     // {image}: warm VMs available
	PoolCap        prometheus.Gauge         // POOL_SIZE × number of stocked images
	PoolRefill     *prometheus.CounterVec   // {image}: refill events
	PoolRefillFail *prometheus.CounterVec   // {image}: refill failures
	PoolRefillDur  *prometheus.HistogramVec // {image}: refill duration
	PoolEvicted    *prometheus.CounterVec   // {image,reason}: evictions

	// Leases
	LeasesActive prometheus.Gauge // leased (non-warm) sandboxes
	LeasesQueued prometheus.Gauge // demand waiting for a slot
	// LeasesQueuedOldest is how long the oldest waiting create has waited
	// (#129 part 1), for the dashboard's capacity panel.
	LeasesQueuedOldest prometheus.Gauge
	LeasesTotal        prometheus.Counter     // cumulative leases granted
	LeaseGrantDur      prometheus.Histogram   // time from request to ready
	LeaseOps           *prometheus.CounterVec // {op}: suspend, resume, restart, clone, keepalive
	LeaseSwept         prometheus.Counter     // TTL-expired leases swept
	LeaseOrphaned      prometheus.Counter     // orphans detected on startup
	LeaseHeartbeats    prometheus.Counter     // guest-service lease heartbeats accepted

	// Queued admission (#129 part 1): creates that could not be admitted
	// are held in process and retried in fair-share order.
	AdmitWait     prometheus.Histogram // how long admitted creates waited
	AdmitTimeouts prometheus.Counter   // queued creates that timed out or were cancelled

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

	// Store
	StoreErrors *prometheus.CounterVec // {op}: SQLite write failures (U05)

	// Webhook notifications (2.2, #117)
	Notifications *prometheus.CounterVec // {webhook,severity,result}: delivery outcomes

	// StartTime is when this backend process started (unix seconds), so a
	// reader can tell the backend's uptime from the host's.
	StartTime prometheus.Gauge

	// Builds (image bake)
	BuildsInFlight prometheus.Gauge   // active bakes
	BuildsFailed   prometheus.Counter // cumulative bake failures

	// Checkpoints (U10)
	CheckpointDur prometheus.Histogram // sub.Checkpoint snapshot duration
	// CheckpointPause (2.3, #122): how long one checkpoint pauses the
	// guest, in seconds, buckets 1..600.
	CheckpointPause prometheus.Histogram

	// Snapshot catalog (U11)
	SnapshotBytes *prometheus.GaugeVec   // {kind}: measured build disk bytes
	StorageFree   prometheus.Gauge       // free bytes at the template storage path
	GCDeleted     *prometheus.CounterVec // {kind}: builds deleted by the catalog GC
	// Kept checkpoints (#126): pins of live leases and their bytes.
	KeptBuildsBytes prometheus.Gauge // summed size_bytes over kept builds of live leases
	KeptBuilds      prometheus.Gauge // pin count over live leases

	// Held-lease limits (2.1): automatic actions on held leases
	HeldActions *prometheus.CounterVec // {rule,action}: idle/stale/expiry/pressure/critical × suspend/release/expire

	// Preemption (#128 part 3): burst leases suspended to make room for
	// a guaranteed admission, and how many are preempted right now.
	PreemptionsTotal prometheus.Counter // cumulative preemptions
	PreemptedLeases  prometheus.Gauge   // leases currently preempted

	// Idle suspension (2.5, #129 part 2): persistent leases suspended by
	// the idle sweep through the pause path; the next call resumes them.
	IdleSuspendsTotal prometheus.Counter // cumulative idle suspensions

	// Guest port dials (2.2, #113): host-to-guest TCP over a WebSocket
	GuestDialsActive prometheus.Gauge       // open WebSocket→guest TCP bridges
	GuestDialsTotal  *prometheus.CounterVec // {result}: ok, refused, error

	// Substrate (U11)
	LeasesByState     *prometheus.GaugeVec     // {state}: leases per state
	LeasesByImage     *prometheus.GaugeVec     // {image}: live leases per image
	NodeRunning       prometheus.Gauge         // orchestrator running sandboxes
	NodeHugepagesFree prometheus.Gauge         // (total − used − reserved) × page size
	NodeWork          prometheus.Gauge         // orchestrator outstanding work
	CreateDur         *prometheus.HistogramVec // {resume}: sub.Create duration
	CapacityRej       prometheus.Counter       // admission refusals
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
		Help: "Designed warm-pool size (POOL_SIZE × number of stocked images).",
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
	m.LeasesQueuedOldest = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "leases_queued_oldest_seconds",
		Help: "Age of the oldest create waiting for admission.",
	})
	m.AdmitWait = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "admit_wait_seconds",
		Help:    "How long a queued create waited before it was admitted.",
		Buckets: []float64{0, 1, 2, 5, 10, 30, 60, 120, 300, 600},
	})
	m.AdmitTimeouts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "admit_timeouts_total",
		Help: "Queued creates that gave up waiting (timeout, client gone or drain).",
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
	m.LeaseHeartbeats = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "lease_heartbeats_total",
		Help: "Guest-service lease heartbeats accepted (POST /lease/{id}/active).",
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

	// Store
	m.StoreErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "store_errors_total",
		Help: "Store write failures by operation.",
	}, []string{"op"})
	// Webhook notifications (2.2, #117): the webhook label is the
	// webhook's index in NOTIFY_WEBHOOKS — never the URL, which may
	// carry secrets — and "-" when no webhook was chosen (queue,
	// dedupe and drop-before-match outcomes).
	m.StartTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "backend_start_time_seconds",
		Help: "Unix time the backend process started.",
	})
	m.StartTime.Set(float64(time.Now().UnixNano()) / 1e9)
	m.Notifications = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "notifications_total",
		Help: "Webhook notification delivery outcomes. `webhook` is the receiver's index in NOTIFY_WEBHOOKS (never its URL); result is sent, retry, dropped, deduped or rate_limited.",
	}, []string{"webhook", "severity", "result"})

	// Builds
	m.BuildsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "builds_in_flight",
		Help: "Active image bakes.",
	})
	m.BuildsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "builds_failed_total",
		Help: "Cumulative image bake failures.",
	})
	// Checkpoints
	m.CheckpointDur = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "checkpoint_duration_seconds",
		Help:    "Duration of one substrate Checkpoint call.",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 10), // 0.5s → 256s
	})
	// Checkpoint pause (2.3, #122): how long each checkpoint leaves the
	// guest paused, whatever surfaced it (periodic pass, manual route,
	// clone, fork).
	m.CheckpointPause = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "checkpoint_pause_seconds",
		Help:    "How long one checkpoint pauses the guest, in seconds.",
		Buckets: []float64{1, 2, 5, 10, 30, 60, 120, 300, 600},
	})

	// Snapshot catalog (U11)
	m.SnapshotBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "snapshot_bytes",
		Help: "Disk bytes per build kind (allocated blocks × 512).",
	}, []string{"kind"})
	m.StorageFree = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "storage_free_bytes",
		Help: "Free bytes at the template storage path.",
	})
	m.GCDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "gc_deleted_total",
		Help: "Builds deleted by the catalog GC, by kind.",
	}, []string{"kind"})
	// Kept checkpoints (2.3 #121, #126): the pins and their disk bytes.
	m.KeptBuildsBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "kept_builds_bytes",
		Help: "Disk bytes held by kept checkpoints of live leases (summed recorded size_bytes).",
	})
	m.KeptBuilds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "kept_builds",
		Help: "Kept checkpoints of live leases (pins; a build pinned twice counts once per lease).",
	})

	// Held-lease limits (2.1): automatic actions on held leases.
	m.HeldActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "held_actions_total",
		Help: "Automatic actions on held leases, by rule (idle, stale, expiry, pressure, critical) and action (suspend_idle, release, expire).",
	}, []string{"rule", "action"})

	// Preemption (#128 part 3): burst leases suspended to make room for
	// a guaranteed admission.
	m.PreemptionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "preemptions_total",
		Help: "Burst leases suspended to make room for a guaranteed admission.",
	})
	m.PreemptedLeases = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "preempted_leases",
		Help: "Burst leases currently suspended by preemption, awaiting the resume queue.",
	})

	// Idle suspension (2.5, #129 part 2): persistent leases suspended by
	// the idle sweep. The next call resumes them.
	m.IdleSuspendsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "idle_suspends_total",
		Help: "Persistent leases suspended by the idle sweep (idle_suspend).",
	})

	// Guest port dials (2.2, #113)
	m.GuestDialsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "guest_dials_active",
		Help: "Open guest port dial bridges (WebSocket to guest TCP).",
	})
	m.GuestDialsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "spoond", Name: "guest_dials_total",
		Help: "Guest port dial attempts by result: ok, refused (per-owner cap), error (the guest dial failed).",
	}, []string{"result"})

	// Substrate (U11)
	m.LeasesByState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "leases",
		Help: "Leases per state: running, suspended, recovered, lost.",
	}, []string{"state"})
	m.LeasesByImage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "leases_by_image",
		Help: "Live (non-released) leases per image.",
	}, []string{"image"})
	m.NodeRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "node_running_sandboxes",
		Help: "Sandboxes running on the orchestrator node.",
	})
	m.NodeHugepagesFree = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "node_hugepages_free_bytes",
		Help: "Free hugepage bytes: (total − used − reserved) × page size.",
	})
	m.NodeWork = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "spoond", Name: "node_outstanding_work",
		Help: "Outstanding tracked operations on the node.",
	})
	m.CreateDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "spoond", Name: "create_duration_seconds",
		Help:    "Duration of one substrate Create call, by resume.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12), // 0.1s → ~3.4min
	}, []string{"resume"})
	m.CapacityRej = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "spoond", Name: "capacity_rejections_total",
		Help: "Admission refusals (hugepages or node status).",
	})

	// Register all
	reg.MustRegister(
		m.PoolReady, m.PoolCap, m.PoolRefill, m.PoolRefillFail,
		m.PoolRefillDur, m.PoolEvicted,
		m.LeasesActive, m.LeasesQueued, m.LeasesQueuedOldest, m.LeasesTotal, m.LeaseGrantDur,
		m.LeaseOps, m.LeaseSwept, m.LeaseOrphaned, m.LeaseHeartbeats,
		m.AdmitWait, m.AdmitTimeouts,
		m.HTTPReqs, m.HTTPDur,
		m.LLMReqs, m.LLMDur, m.LLMErrors, m.LLMRateLimit,
		m.LLMKeyFail, m.LLMKeylessDen, m.LLMInflight,
		m.ProxyReqs, m.ProxyAuthRes,
		m.NetpolApply, m.NetpolErrors, m.NetpolDur,
		m.IdentityUsers, m.IdentityAdmins, m.AuthFailures,
		m.AuthThrottled, m.QuotaExceeded, m.QuotaReserved,
		m.SharesActive, m.BusySlots,
		m.StoreErrors,
		m.Notifications, m.StartTime,
		m.BuildsInFlight, m.BuildsFailed,
		m.CheckpointDur, m.CheckpointPause,
		m.SnapshotBytes, m.StorageFree, m.GCDeleted,
		m.KeptBuildsBytes, m.KeptBuilds,
		m.HeldActions,
		m.PreemptionsTotal, m.PreemptedLeases,
		m.IdleSuspendsTotal,
		m.GuestDialsActive, m.GuestDialsTotal,
		m.LeasesByState, m.LeasesByImage, m.NodeRunning, m.NodeHugepagesFree, m.NodeWork,
		m.CreateDur, m.CapacityRej,
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
	JobsTotal         *prometheus.CounterVec // {result}: success, failure, cancelled
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
