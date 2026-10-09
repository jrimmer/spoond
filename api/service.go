// Package api implements the lease API backend on the Substrate
// interface (U08): every lease operation is an E2B sandbox operation
// recorded in SQLite. Consumers never manage templates, builds, pools
// or egress directly.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// Lease is a sandbox granted to a consumer for a bounded lifetime.
type Lease struct {
	ID         string // unguessable lease id
	Owner      string `json:"owner"` // owner identity (user id or legacy consumer id)
	Image      string // image name in the catalog
	SandboxID  string // underlying E2B sandbox id
	HostIP     string // host-side address of the running sandbox, no port
	BuildID    string // build the running sandbox was created from ("" when suspended)
	TemplateID string // template of the lease's image (looked up, not persisted)
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Persistent bool      // interactive lease: not TTL-swept, keep-alive extends
	LastActive time.Time // last activity (exec/stream/proxy/keepalive), for idle sweep
	Suspended  bool      // derived from State == "suspended"
	Name       string    // optional friendly name/tag (unique per owner; resolved by ssh/proxy)
	NetPolicy  string    // egress policy: none|lan|internet|restricted ("" = restricted)
	NetAllow   []string  // allowlist for restricted policy
	// ExposePorts are guest TCP ports published to other sandboxes:
	// peers reach them as their own egress policy permits
	// (peerAllowances).
	ExposePorts []int
	// ExposedIP mirrors HostIP (kept for the store column).
	ExposedIP string
	Comment   string // optional free-text annotation (set/cleared via ctl comment)
	// Lifecycle state kept in the store (U05); later units set the
	// checkpoint/recovery fields. State is running|suspended|recovered|lost
	// ("" = derived from Suspended).
	State                 string
	ResumeBuildID         string    // build a suspended lease resumes from
	LastCheckpointBuildID string    // newest checkpoint build of this lease
	LastCheckpointAt      time.Time // zero = never checkpointed
	RecoveredFrom         time.Time // zero = never recovered
	// LostAt is when the lease became lost (zero = unset). "lost_at" is
	// omitted while unset; the GC's lost-lease grace period counts from
	// it.
	LostAt time.Time `json:"lost_at,omitempty"`
	// LostReason is why the lease was lost — the detail of its lost
	// event, e.g. "no checkpoint to recover from; the running state is
	// gone" or "root disk unreadable (I/O errors)". It is returned by
	// the lease API as "lost_reason" and carried in every 409
	// lease_lost response, so the initiator learns the cause. "" for a
	// lease lost before the column existed.
	LostReason string `json:"lost_reason,omitempty"`
	// Drained marks a lease the admin drain paused (U10): undrain
	// resumes exactly the drained leases.
	Drained bool
	// Holder names what holds the lease (a CI job, an orchestrator's
	// flight, a person's scratch work) and HolderUrl links to it. A
	// non-empty holder makes the lease held: not released at its TTL and
	// not idle-suspended. (Periodic checkpoints follow the lease's own
	// checkpoint_interval since 2.3, not the hold.) "" = unheld.
	Holder    string `json:"holder,omitempty"`
	HolderUrl string `json:"holder_url,omitempty"`
	// HoldSetAt/HoldExpiresAt bound the hold (2.1): it lasts HoldTTL
	// from when it was set or renewed, at most HoldTTLMax for an
	// explicit hold_ttl. Past HoldExpiresAt the holder fields are
	// cleared and the lease follows the normal TTL and idle rules.
	// HoldTTL mirrors the requested explicit hold_ttl (0 = the default).
	HoldSetAt     time.Time     `json:"hold_set_at,omitempty"`
	HoldExpiresAt time.Time     `json:"hold_expires_at,omitempty"`
	HoldTTL       time.Duration `json:"-"`
	// LastAction/LastActionAt record the last automatic held-lease
	// action ("rule/action", e.g. "idle/suspend_idle") and when it
	// happened; both are returned by the lease API.
	LastAction   string    `json:"last_action,omitempty"`
	LastActionAt time.Time `json:"last_action_at,omitempty"`
	// Generation is the lease's continuity generation (2.2): 1 at
	// create, bumped every time the guest's memory does not continue
	// from where its processes left it — crash recovery and restart. A
	// planned pause/resume and the admin drain/undrain continue the
	// memory and do not bump it. Returned by the lease API as
	// "generation" and written into the guest at /run/spoond/generation.
	Generation int64 `json:"generation"`
	// CheckpointInterval is the lease's own periodic checkpoint
	// interval in seconds (2.3, #122): -1 = the host default
	// (CHECKPOINT_INTERVAL_MINS, itself 0 = never), 0 = never,
	// >0 = seconds. The lease API reports the effective value (the
	// host default already resolved) as "checkpoint_interval".
	CheckpointInterval int64 `json:"-"`
	// IdleSuspend is the lease's own idle reclamation threshold in
	// seconds (2.5, #129 part 2): -1 = the host default
	// (IDLE_SUSPEND_DEFAULT_SECS, itself 0 = never), 0 = never, >0 =
	// suspend the persistent lease after that long without activity,
	// resuming it on the next call. The lease API reports the effective
	// value (the host default already resolved) as "idle_suspend".
	IdleSuspend int64 `json:"-"`
	// MemoryMB is the lease's MiB charge (#128): the image's memory_mb,
	// stamped when the guest is granted or rebuilt, and summed over a
	// user's running leases for admission (a suspended lease keeps its
	// stamp but is not charged — it holds no hugepages). Cached on the
	// lease so quota accounting never reads the image catalog; 0 =
	// unknown (leases from before the stamp, or a vanished image row).
	MemoryMB int `json:"-"`
	// Class is the lease's admission class (#128 part 2):
	// "guaranteed" or "burst", decided once at admission and kept for
	// the lease's life. Guaranteed: the owner's running charge with
	// this lease stays within their guaranteed_mib (a user with none
	// keeps every lease guaranteed — today's behaviour). Burst: above
	// the guarantee, or the request forced burst; admissible only while
	// the node's free hugepages stay above BurstReserveMiB after this
	// lease's own. Reported by the lease API as "class".
	Class string `json:"-"`
	// PreemptedAt is when the lease was preempted (#128 part 3): it was
	// suspended through the pause path to free hugepages for a
	// guaranteed admission. A preempted lease is a burst lease the
	// resume queue brings back when capacity allows; resume clears the
	// instant. Zero = not preempted. Persisted as preempted_at and
	// reported by the lease API as "preempted".
	PreemptedAt time.Time `json:"-"`
	// SnapshotBuildID is the named-snapshot version's build the lease was
	// started from (2.7, #83); '' when it did not start from one. A
	// version whose build a live lease runs from is never dropped by
	// retention. Start-from-snapshot (and its API field) is task 2.
	SnapshotBuildID string `json:"-"`
	// SuspendReason, SuspendPolicyStep, SuspendBuildID and SuspendedAt
	// record an automatic suspend (#145 D6): reason is one of
	// idle|idle_suspend|hold_lapsed|pressure|preempt, policy step is the
	// pressure order's step name ("" until it names steps), the build is
	// the pause build written and suspended_at is when. A hand or drain
	// suspend carries none of them. Reported by the lease API as
	// suspend_reason, suspend_policy_step, suspend_build_id and
	// suspended_at; cleared on resume. Persisted (migration 0021).
	SuspendReason     string    `json:"-"`
	SuspendPolicyStep string    `json:"-"`
	SuspendBuildID    string    `json:"-"`
	SuspendedAt       time.Time `json:"-"`
	// SnapshotName and SnapshotVersion name the named-snapshot version
	// the lease started from, for the "snapshot" object in the API
	// (A3). They are not persisted; a lease loaded from the store looks
	// its version up by SnapshotBuildID when the API needs it.
	SnapshotName    string `json:"-"`
	SnapshotVersion int64  `json:"-"`
	// Priority orders preemption within a class (#128 part 2): a lower
	// number is preempted first. 0 = the default. Reported as
	// "priority".
	Priority int `json:"-"`
	// Burst records the request's "burst": true (#128 part 2) — the
	// caller asked for preemptible scheduling even within the owner's
	// guarantee. It keeps the lease bursting across rebuilds: a lease
	// classified burst by demand stays burst on resume/restart, while
	// one classified burst by a full guarantee may fall back to
	// guaranteed when the charge drops below it. Not persisted; Class
	// is. The cost of that: a backend restart loads leases from the
	// store without the flag, so a request-burst lease re-classifies
	// from the owner's standing at its next resume/restart — its stored
	// class stays burst until then, but the re-decision may return it
	// to guaranteed. Accepted for part 2: the flag is the request's,
	// the stored class stays the truthful last admission.
	Burst bool `json:"-"`
	// pooled marks a lease served from the warm pool: the sandbox's envd
	// default SPOOND_LEASE_ID is "pool" (env vars cannot be updated after
	// create), so exec/stream/stat/prompt add the lease id per request.
	// Not persisted.
	released bool
	pooled   bool
	// busy marks a lease with an in-flight suspend/resume/restart/
	// checkpoint/pause operation (U10): a second operation on it returns
	// 409. Not persisted.
	busy bool
}

// live reports whether the lease has a running sandbox. "recovered" is
// introduced in U10 and behaves exactly like "running".
func (l *Lease) live() bool { return l.State == "running" || l.State == "recovered" }

// setState records a lifecycle state change. Entering "lost" stamps
// LostAt once — an existing timestamp is never overwritten, so a lease
// that dips in and out of the lost state keeps the original loss time
// (and the grace period counted from it) — and leaving "lost" clears
// it.
func (l *Lease) setState(state string) {
	l.State = state
	// Only a suspended lease can be preempted (#128 part 3): every path
	// that runs it again — resume, restore, cold restart, recovery — or
	// loses it ends the preemption here, so none can leave a running or
	// lost lease flagged for the resume queue. resumeLease reads the
	// flag before this call for its event detail.
	if state != "suspended" {
		l.PreemptedAt = time.Time{}
		// Leaving the suspended state ends its structured facts (#145
		// D6): a resume, restore, restart or loss must not leave a stale
		// reason, policy step, build or time behind.
		clearSuspendFactsLocked(l)
	}
	if state == "lost" {
		if l.LostAt.IsZero() {
			l.LostAt = time.Now()
		}
		return
	}
	l.LostAt = time.Time{}
	l.LostReason = ""
}

// clearSuspendFactsLocked drops a lease's structured suspension facts
// (#145 D6): every path that brings the guest back — resume, restore,
// cold restart, crash recovery — runs it, so a stale reason never
// describes a later, different suspend and the lease API omits the
// fields once the lease runs again. Call with s.store.mu held.
func clearSuspendFactsLocked(l *Lease) {
	l.SuspendReason = ""
	l.SuspendPolicyStep = ""
	l.SuspendBuildID = ""
	l.SuspendedAt = time.Time{}
}

// ShareMode selects which surfaces a share covers.
type ShareMode string

const (
	ShareSSH  ShareMode = "ssh"  // SSH attach / ctl access
	ShareHTTP ShareMode = "http" // proxy / exec API access
)

// Share grants a user access to a lease owned by someone else (T6/#33).
type Share struct {
	LeaseID   string    // the shared lease
	Grantee   string    // user id receiving access
	Mode      ShareMode // ssh | http
	ExpiresAt time.Time // zero = never expires
	CreatedAt time.Time
}

// Store holds the live leases and the warm pool.
type Store struct {
	mu     sync.Mutex
	leases map[string]*Lease
	// pool holds pre-created E2B sandbox ids per image name, oldest first.
	pool map[string][]string
	// shares maps lease id -> grantee id -> share (T6/#33).
	shares map[string]map[string]*Share
	// pending counts in-flight lease creations per owner (T4/#31 quota
	// reservation, security review #37 H2): a slot is reserved under
	// the same store lock as the quota count and released when the lease
	// is inserted or the grant fails, closing the check-then-create race.
	pending map[string]int
	// pendingMiB is the memory twin of pending (#128): the MiB reserved
	// by in-flight creations per owner, under the same store lock, so
	// two racing creates cannot both pass max_mib.
	pendingMiB map[string]int
	// lastActiveDirty batches touch() updates; the sweeper flushes them
	// to the store once per tick instead of writing on every activity.
	lastActiveDirty map[string]time.Time
	// runningJobs counts each lease's running background exec jobs (2.6,
	// #135). The idle sweeps read it under the same lock as the leases
	// they walk, so a lease with a running job counts as active without a
	// store round trip.
	runningJobs map[string]int
}

func newStore() *Store {
	return &Store{
		leases:          make(map[string]*Lease),
		pool:            make(map[string][]string),
		shares:          make(map[string]map[string]*Share),
		pending:         make(map[string]int),
		pendingMiB:      make(map[string]int),
		lastActiveDirty: make(map[string]time.Time),
		runningJobs:     make(map[string]int),
	}
}

// newID returns a random unguessable hex id.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ServiceConfig carries the constructor tunables.
type ServiceConfig struct {
	PoolSize                        int
	DefaultTTL, MaxTTL, IdleTimeout time.Duration
	HostGuestAddr                   string // HOST_GUEST_SERVICE_ADDR
	HostGuestPort                   int    // HOST_GUEST_SERVICE_PORT
	// GuestDNSAddr is the guest's DNS resolver address or addresses
	// (SPOOND_GUEST_DNS_ADDR, comma-separated). Each is granted to every
	// lease's egress policy on port 53. Empty = no resolver allowance
	// (the deployment relies on the guest's own resolv.conf).
	GuestDNSAddr string
	// ProxyHostSuffix is the wildcard hostname suffix the HTTP proxy
	// routes (SPOOND_PROXY_HOST_SUFFIX), e.g. ".sandbox.example.com":
	// <lease-id>.<suffix> and <lease-id>-<port>.<suffix>. Empty = the
	// generic default.
	ProxyHostSuffix string
	MetricsToken    string // METRICS_TOKEN: bearer that may read /metrics only (scrapers, dashboards)
	EventsToken     string // EVENTS_TOKEN: bearer that may read the lease event stream only (dashboards)
	HostAPIPort     int    // HOST_API_PORT: lease API port lan/internet guests may reach on HostGuestAddr (0 = none)
	ProxyURL        string // E2B orchestrator sandbox proxy (e.g. http://127.0.0.1:5007)
	// CheckpointIntervalDefault is the host default checkpoint interval
	// in seconds (2.3, #122) applied to leases whose own interval is -1
	// ("the host default"). 0 = never. CHECKPOINT_INTERVAL_MINS.
	CheckpointIntervalDefault int64
	// IdleSuspendDefault is the host default idle reclamation threshold
	// in seconds (2.5, #129 part 2) applied to persistent leases whose
	// own idle_suspend is -1 ("the host default"). 0 = never.
	// IDLE_SUSPEND_DEFAULT_SECS.
	IdleSuspendDefault  int64
	TemplateStoragePath string // E2B_TEMPLATE_STORAGE_PATH: build storage root, for disk accounting (U11)
	// BuildTimeout is how long a template build may run before the GC
	// treats a row still `building` as stale and fails it (spoond-4yl).
	// The GC fails a building row older than twice this, logged. Zero
	// falls back to substrate.DefaultBuildTimeout. The backend sets it
	// from SPOOND_BUILD_TIMEOUT, but `spoond images build` reads that
	// variable in its own process, so a deployment must set it for both
	// (spoond-rzz).
	BuildTimeout time.Duration
	// LostGracePersistent / LostGrace are how long a lost lease's
	// resume_build_id and last_checkpoint_build_id stay kept roots after
	// lost_at — 7 days for persistent leases, 1 day for the rest, so a
	// lease lost in a substrate crash leaves its snapshots around long
	// enough to be reclaimed by hand. Zero falls back to the defaults.
	LostGracePersistent time.Duration
	LostGrace           time.Duration
	// Held-lease limits (2.1, owner decision 2026-10-03): how long a
	// held lease may sit idle before it is suspended (0 disables), how
	// long it may stay suspended before it is released (0 disables),
	// how long a hold lasts and how long an explicit hold_ttl may be,
	// and the disk-free percentages that shorten the idle threshold
	// (pressure) or release suspended held leases (critical). Zero
	// falls back to the Default* constants in api/held.go.
	HeldIdleTimeout        time.Duration
	HeldSuspendedRelease   time.Duration
	HoldTTL                time.Duration
	HoldTTLMax             time.Duration
	PressureDiskFreePct    float64
	PressureHeldIdle       time.Duration
	CriticalDiskFreePct    float64
	CriticalDiskRecoverPct float64
	// MaxKeptPerLease is the per-lease kept-checkpoint cap (#126): a
	// keep on a lease already holding this many kept builds answers 409
	// and takes nothing. 0 = no cap. MAX_KEPT_PER_LEASE, default
	// DefaultMaxKeptPerLease.
	MaxKeptPerLease int
	// MaxNamedSnapshots is the per-owner cap on distinct named-snapshot
	// names (2.7, #83): a save that would add a new name past it answers
	// 409. 0 = no cap. MAX_NAMED_SNAPSHOTS, default
	// DefaultMaxNamedSnapshots.
	MaxNamedSnapshots int
	// SnapshotKeepVersions is the default retention for a named snapshot
	// name (2.7, #83): the last N versions survive a save. A name's own
	// first-save keep overrides it. SNAPSHOT_KEEP_VERSIONS, default
	// DefaultSnapshotKeepVersions.
	SnapshotKeepVersions int
	// BurstReserveMiB is the hugepage reserve (#128 part 2) a burst
	// lease must leave free on the node after its own hugepages — the
	// guaranteed class never runs into it. BURST_RESERVE_MIB, default
	// DefaultBurstReserveMiB. 0 disables the reserve.
	BurstReserveMiB int
	// PreemptDiskFloorPct is the snapshot-disk free percentage a
	// preemption or idle-suspend pause must leave after it (#128 part 3,
	// 2.5). PREEMPT_DISK_FLOOR_PCT, default DefaultPreemptDiskFloorPct.
	// 0 or negative means the default.
	PreemptDiskFloorPct float64
	// MaxRunningJobsPerLease bounds concurrent background exec jobs per
	// lease (2.6, #135). 0 = DefaultMaxRunningJobsPerLease.
	MaxRunningJobsPerLease int
	// JobRetentionSecs is how long an exited background job record is
	// kept before pruning (2.6, #135). 0 = DefaultJobRetentionSecs.
	JobRetentionSecs int64
	// JobMaxRuntimeSecs caps how long a background job may run before it
	// is killed and marked exited with reason timed_out (spoond-wb5). A
	// job may ask for a shorter max_runtime_secs, never a longer one. 0
	// = DefaultJobMaxRuntimeSecs; a negative value disables the cap.
	JobMaxRuntimeSecs int64
	// MaxAdmitWaitSecs caps how long a create may wait for admission
	// (#129 part 1). MAX_ADMIT_WAIT_SECS, default DefaultMaxAdmitWaitSecs;
	// 0 disables waiting (the request field is accepted and ignored).
	MaxAdmitWaitSecs int
	// CrashTest enables POST /api/leases/{id}/crash-test, which runs one
	// lease through crash recovery on demand (api/crash.go). Off by
	// default; when off the route answers 404 like an unknown route.
	// CRASH_TEST ("1" or "true").
	CrashTest bool
	// SnapshotWriteConcurrency is the width of the process-wide
	// snapshot-write limiter (SNAPSHOT_WRITE_CONCURRENCY): every
	// substrate Pause and Checkpoint goes through it, so the node never
	// sees a stack of memory snapshots at once (spoond-t1s). 0 =
	// unlimited (the pre-fix behaviour); cmd maps an unset variable to
	// DefaultSnapshotWriteConcurrency (1).
	SnapshotWriteConcurrency int
	// DrainSnapshotConcurrency is the width of the drain's own
	// snapshot-write limiter (DRAIN_SNAPSHOT_CONCURRENCY), used only for
	// the admin drain's pauses. It is separate so a planned orchestrator
	// restart can pause a batch of live leases within the unit's drain
	// window without widening the default limiter. 0 = unlimited; cmd
	// maps an unset variable to DefaultDrainSnapshotConcurrency (2).
	DrainSnapshotConcurrency int
	// UndrainConcurrency is how many drained leases the admin undrain
	// resumes at once (UNDRAIN_CONCURRENCY): bounded so restoring a batch
	// of large memory snapshots does not stack the whole node's I/O and
	// memory. 0 = unlimited; cmd maps an unset variable to
	// DefaultUndrainConcurrency (2). spoond-urm.
	UndrainConcurrency int
	// UndrainResumeRetries is how many extra attempts a resume the admin
	// undrain failed with a retryable envd/start error gets before the
	// lease is marked lost (UNDRAIN_RESUME_RETRIES). 0 disables retries;
	// cmd maps an unset variable to DefaultUndrainResumeRetries (2).
	// spoond-urm.
	UndrainResumeRetries int
	// RecoveryRetryAttempts is how many failed crash-recovery attempts
	// (a transient failure of recoverFromCheckpoint) a lease gets before
	// it is marked lost (RECOVERY_RETRY_ATTEMPTS). <=0 uses
	// DefaultRecoveryRetryAttempts. Recovery counts anything not
	// permanent; only a substrate capacity refusal waits for room
	// without counting (admission is skipped for a live lease).
	// spoond-dxq.
	RecoveryRetryAttempts int
	// RecoveryRetryWindow bounds how long a lease may stay in recovery
	// since its first failed attempt, whatever the failure kind
	// (RECOVERY_RETRY_WINDOW). It is the backstop that stops a capacity
	// refusal from waiting forever. <=0 uses DefaultRecoveryRetryWindow.
	// spoond-dxq.
	RecoveryRetryWindow time.Duration
	// SweepTimeout bounds one background sweep stage (TTL release, held
	// rules, pool refill, job prune) and each other background loop
	// pass. The substrate bounds every individual RPC too (spoond-j3a);
	// this is the belt-and-suspenders that frees the loop, and so a
	// lease's busy flag, even if a substrate call ignores its context.
	// 0 = DefaultSweepTimeout. SWEEP_TIMEOUT.
	SweepTimeout time.Duration
	// DrainMaxSecs bounds how long a drain may stay in effect while the
	// node is healthy before spoond undrains itself, logs it and emits
	// an event (DRAIN_MAX_SECS, default DefaultDrainMaxSecs = 900). A
	// drain outliving its orchestrator restart must not keep refusing
	// creates forever. 0 means the default; a negative value (tests
	// only) disables the automatic undrain.
	DrainMaxSecs int
	// DrainResumeMaxAge bounds how long the drain self-heal loop keeps
	// retrying a lease whose resume is deferred (DRAIN_RESUME_MAX_AGE,
	// default DefaultDrainResumeMaxAge = 24h). Past it the loop stops,
	// keeps the lease suspended (its snapshot is intact) and emits a
	// drain_gave_up event, leaving the exit to the owner or the idle
	// rules. 0 means the default; a negative value (tests only) disables
	// the bound.
	DrainResumeMaxAge time.Duration
}

// Service is the lease API backend.
type Service struct {
	sub substrate.Substrate
	// db persists leases, shares, the pool and the sandbox catalog
	// (U05/U08). Required.
	db    *store.DB
	store *Store
	// tokens maps a consumer token to its consumer id (legacy mode).
	tokens map[string]string
	// ownerDeleteMu guards deletedOwners (spoond-q4j): the owners whose
	// identity was removed while a create, clone or fork of theirs could
	// still be in flight. reserveQuota and the grant/clone/fork commits
	// consult it so no admission revives an ownerless (uncapped) lease
	// recreated by a create that raced DELETE /api/users/{id}. Entries
	// are never pruned: deleting a user is permanent, and a request that
	// passed auth before the removal could still reach a commit, so the
	// mark must outlive the cleanup. The set is in-memory and bounded by
	// the number of users deleted in one process's life (a few small
	// strings); a backend restart drops both the parked tickets and the
	// set, which is safe because no racing request survives it either.
	ownerDeleteMu sync.Mutex
	deletedOwners map[string]bool
	// identities is the user/identity store (epic #26 T1). When set,
	// bearer-token auth resolves against it first; tokens map remains as
	// the backward-compatible fallback for single-user deployments.
	identities *identity.Store
	// gatewayToken is the SSH gateway's service token (U6/T5). When a
	// request authenticates with it, the X-Spoond-User-Id header is
	// honored so the gateway can act as the SSH-authenticated user; the
	// backend's owner-scoping then applies to that user, not the gateway
	// service identity. Empty disables impersonation.
	gatewayToken string
	// cfg carries the pool size, TTL bounds, idle timeout and the
	// host-service address guests use (LLM gateway, proxy, assets).
	cfg ServiceConfig
	// probeEnabled runs integrityProbe inside each sandbox before it is
	// pooled or handed to a lease. probeTimeout bounds that exec.
	probeEnabled bool
	probeTimeout time.Duration
	// rootfsProbeInterval is how often the rootfs liveness probe runs
	// against running leases (spoond-5ca) as a duration; 0 disables it.
	// It is atomic so the probe loop reads it without a lock even if
	// SetRootfsProbe runs concurrently with a pass. rootfsProbeMu guards
	// the last-success times and the consecutive-failure counters; both
	// are keyed by lease id, the failures also by sandbox id so a
	// replacement sandbox starts clean.
	rootfsProbeInterval atomic.Int64
	rootfsProbeMu       sync.Mutex
	// rootfsProbeOK is the last successful exec time per lease: an exec
	// that started within the probe interval already proves the guest is
	// alive, so the probe is skipped.
	rootfsProbeOK map[string]time.Time
	// rootfsProbeFails is the consecutive rootfs-probe failure count per
	// lease, tracked against the sandbox it was counted on.
	rootfsProbeFails map[string]*rootfsProbeFailure
	// rootfsProbeAllFailedLogged suppresses the "every probe failed"
	// line to once per outage rather than once per pass.
	rootfsProbeAllFailedLogged bool
	// retryMu guards the per-lease retry budgets below. recoveryRetries
	// counts the failed crash-recovery attempts of a lease, keyed by the
	// sandbox id that failed (so a lease given a new sandbox is never
	// rolled back to an old checkpoint by a stale budget). It is in
	// memory: a lease that recovers, is lost or is released has its entry
	// dropped (spoond-dxq).
	retryMu         sync.Mutex
	recoveryRetries map[string]*retryBudget
	// sweepInterval is the TTL-sweeper tick (overridable in tests).
	sweepInterval time.Duration
	// sweepTimeout bounds one background sweep stage and each other
	// background loop pass, so a substrate call that ignores its context
	// cannot wedge the loop (spoond-j3a). DefaultSweepTimeout unless
	// configured.
	sweepTimeout time.Duration
	// lostSandboxDeleteAttempts and lostSandboxDeleteBackoff bound the
	// substrate Delete retries when a lease becomes lost (api/recovery.go
	// markLost): the sandbox a failed create or resume left behind is
	// deleted with these, and a delete that still fails is left to the
	// orphan sweep. Fields so tests can shrink the pause.
	lostSandboxDeleteAttempts int
	lostSandboxDeleteBackoff  time.Duration
	// orphanSweepInterval is how often the periodic orphan sandbox sweep
	// runs (spoond-abc): it deletes substrate sandboxes whose lease is
	// lost or released, catching a lost path's bounded delete that still
	// failed. Defaults to defaultOrphanSweepInterval; tests shorten it.
	orphanSweepInterval time.Duration
	// orphanMu guards orphanSandboxIDs: sandbox ids a lost or released
	// lease's substrate Delete could not stop, kept so the periodic orphan
	// sweep retries them. A released lease's row and in-memory entry are
	// gone, so the sweep cannot rediscover them from the lease set alone;
	// this set is what "a Delete that still fails is left to the orphan
	// sweep" means. It is in-memory only (a backend restart drops it, and
	// the startup ReconcileOrphans sweeps the substrate then).
	orphanMu         sync.Mutex
	orphanSandboxIDs map[string]struct{}
	// orphanSweep is the periodic orphan sandbox sweep's in-memory state
	// (spoond-abc): the sandbox ids a creation currently holds, from the
	// moment its id is chosen until its lease or pool entry claims it,
	// and the unclaimed sandboxes seen on the previous pass. See
	// api/orphan_sweep.go.
	orphanSweep orphanSweepState
	// now is the service clock. Tests replace it to age a lease's
	// lost_at without sleeping; the GC's grace periods read it.
	now func() time.Time
	// diskCapacity reads the snapshot store's total and free bytes for
	// the held-lease pressure and critical rules (2.1). Tests replace it
	// to exercise those thresholds without a real filesystem.
	diskCapacity func(path string) (total, free uint64, err error)
	// diskUsage measures one build directory (allocated bytes): the
	// per-build half of disk accounting and of write-time size
	// recording (#125). Tests replace it to force a measurement
	// failure, which permission bits cannot do when tests run as root.
	diskUsage func(dir string) (int64, error)
	// sizeSettleEvery/sizeSettleFor pace settleBuildSize: how often a
	// fresh build is re-measured, and for how long at most. Tests shorten
	// them.
	sizeSettleEvery time.Duration
	sizeSettleFor   time.Duration
	sizeSettleQuiet time.Duration // 0 = 15 s
	// refreshMu serializes refreshPeers runs, which are scheduled
	// asynchronously after lifecycle events (U09).
	refreshMu sync.Mutex
	// appliedEgress remembers the canonical JSON of the egress config
	// last applied to each lease's sandbox, keyed by lease id, so
	// refreshPeers only calls UpdateEgress on change.
	// appliedMu guards appliedEgress and appliedInFlight.
	appliedMu     sync.Mutex
	appliedEgress map[string]string
	// appliedInFlight holds the ids of leases whose egress memo was just
	// applied but whose lease is not yet in store.leases (a create records
	// the memo before it registers the lease). refreshPeers' prune must
	// not drop those memos in the gap, or the next pass re-applies egress
	// once needlessly (spoond-ob18).
	appliedInFlight map[string]struct{}
	log             *log.Logger
	// metrics (issue #20): service-level Prometheus metrics.
	metrics *metrics.BackendMetrics
	// draining is true while the admin drain is running (U10): pool
	// refill, idle sweep, GC and the crash reconcile skip until undrain.
	draining atomic.Bool
	// drainClearPending is true after an undrain's SetDraining(false)
	// failed: the node's drain could not be cleared, so spoond keeps its
	// own draining state and the self-heal loop retries the clear (not
	// gated on DRAIN_MAX_SECS, and even when no drained lease remains)
	// until it succeeds (spoond-52c R1).
	drainClearPending atomic.Bool
	// undraining is true while an undrain is resuming the drained
	// leases, so the self-heal loop does not race it with a second pass.
	undraining atomic.Bool
	// drainStartedAt is when the current drain began (unix nanos; 0 =
	// not draining). The self-heal loop undrains a drain older than
	// DRAIN_MAX_SECS while the node is healthy, and the notifier warns
	// once it passes half that age.
	drainStartedAt atomic.Int64
	// drainHealMu guards the per-lease drain self-heal backoff (a
	// deferred resume is retried with exponential backoff
	// drainHealBackoffMin doubling to drainHealBackoffMax, rather than
	// every pass, and gives up at DRAIN_RESUME_MAX_AGE, spoond-52c B2)
	// and the rate limiter for the loop's "node info" log while the
	// orchestrator is down. Keyed by lease id.
	drainHealMu sync.Mutex
	drainHeal   map[string]*drainHealState
	// drainNodeInfoLogAt is when the self-heal loop last logged a node-info
	// failure; a down orchestrator logs at most once per
	// drainHealBackoffMax rather than every pass (spoond-52c NIT). Guarded
	// by drainHealMu.
	drainNodeInfoLogAt time.Time
	// drainGate serialises the admin drain with the rootfs probe's
	// recovery (spoond-5ca). drain takes the write side around
	// SetDraining and draining.Store(true); recoverDeadRootfs holds the
	// read side for the whole recovery, so a drain that begins mid-pass
	// cannot overlap a recovery for a lease it has not paused yet. An
	// RWMutex that is locked only by those two paths never blocks the
	// rest of the service.
	drainGate sync.RWMutex
	// stopLoops cancels the background sweeper/refiller started by Start.
	stopLoops context.CancelFunc

	// Rule 5's guards (held-lease limits): when its GC last ran, and
	// when it last logged that a dry-run GC stops it. Sweep goroutine only.
	criticalGCAt         time.Time
	criticalDryRunLogged time.Time
	// bus is the lease event bus (2.2, #115): every lifecycle change
	// emits one event here. Set in NewService; never nil.
	bus *eventBus

	// createSecrets holds create-time lease secrets in memory only
	// (#80): never persisted, never logged, lost on restart — the
	// caller re-sends them on its next exec. Keyed by lease id.
	secretsMu     sync.Mutex
	createSecrets map[string]map[string]string
	// notifier receives the bus's person-relevant events (2.2, #117:
	// lease lost, held-lease rule actions). Nil until SetNotifier; the
	// notify loop is only started when it is set.
	notifier NotifySink
	// gcErr remembers the last snapshot GC pass's outcome for the
	// notify checks (gc.failed). Set in NewService.
	gcErr *gcTracker

	// nodeInfoCache is the substrate's last good NodeInfo with its
	// fetch time, behind freeHugepageMiBLocked (#128 part 2): the burst
	// reserve is checked against a value at most nodeInfoCacheTTL old,
	// so a burst of admissions costs the orchestrator one call. The
	// node gauges' loop refreshes it too.
	nodeInfoMu    sync.Mutex
	nodeInfoCache substrate.NodeInfo
	nodeInfoAt    time.Time

	// preemptMu serialises preemption (#128 part 3): one guaranteed
	// admission preempts at a time, so two concurrent creates cannot
	// each suspend a different burst lease for themselves. Held across
	// the whole preempt-then-admit sequence (see admitClass).
	preemptMu sync.Mutex

	// jobStartMu guards the jobStarts map: one start lock per lease,
	// held across the per-lease running-job cap check-then-insert (2.6,
	// #135). A single global lock serialised every background start on
	// every lease across the substrate Start round trip; per-lease locks
	// keep starts on different leases concurrent while two starts on one
	// lease still cannot both pass the cap.
	jobStartMu sync.Mutex
	// jobStarts holds the per-lease start locks, reference counted so an
	// idle lease's entry is dropped once no start holds or waits on it.
	jobStarts map[string]*jobStartLock
	// liveJobSecrets holds the secret names staged for each running job
	// (memory only, never the lease_jobs row): the exit watcher removes
	// exactly those files if the guest wrapper did not. Lost on restart,
	// when the wrapper's own cleanup is the only one left.
	liveJobSecrets map[string][]string
	// timingOutMu guards timingOut, the job ids the max-runtime cap is
	// killing right now. The cap adds an id just before the kill and
	// removes it once the record is closed, so the live watcher cannot
	// record the kill's signal exit as a normal exit in that window. In
	// memory only: a lost entry after a restart leaves the cap to close
	// the record, and the watcher reads the guest rc if anything writes
	// one.
	timingOutMu sync.Mutex
	timingOut   map[string]struct{}
	// jobKillNotBefore caps how often the reconcile's max-runtime kill
	// retries a job whose pid file has not appeared yet. jobPID waits
	// briefly for the wrapper's pid write; a job whose file never appears
	// would otherwise spend that wait on every pass. Guarded by
	// timingOutMu.
	jobKillNotBefore map[string]time.Time
	// stagedExecSecrets counts leases with synchronous exec-time secrets
	// staged right now (between stageSecrets and its deferred cleanup). A
	// named snapshot save refuses while any is staged (2.7, #83): the
	// checkpoint would capture them. Guarded by secretsMu.
	stagedExecSecrets map[string]int
	// stagedExecSecretNames holds the names of those synchronous
	// exec-time secrets, so a save that raced the refuse check scrubs
	// them before its checkpoint. Guarded by secretsMu.
	stagedExecSecretNames map[string][]string
	// pendingSecretRemovals records the exec-time secret names a
	// finishing job could not remove because a named-snapshot save held
	// the lease's secrets gate. The save drains it before its
	// create-time re-stage, so the source ends with its create-time
	// values and no exec-time file survives the save (Q1). Guarded by
	// secretsMu.
	pendingSecretRemovals map[string][]string
	// saveInterrupt, when set by a test, runs between a save's checkpoint
	// and its version-row insert to simulate a backend that stops
	// mid-save (A7). Nil in production.
	saveInterrupt func(ctx context.Context, l *Lease, buildID string) error
	// saveAfterClaim, when set by a test, runs after a save claims its
	// idempotency key in memory and before it re-checks the catalog for a
	// committed replay (S3). Nil in production.
	saveAfterClaim func(owner, name, key string)
	// saveBeforeInsert, when set by a test, runs after a save's
	// checkpoint and before it inserts its version row, so a test can
	// commit a conflicting row and exercise the insert-conflict replay
	// (R5). Nil in production.
	saveBeforeInsert func(owner, name, key string)
	// pauseBeforeSuspend, when set by a test, runs after a pause's build
	// and refs are written and before the store lock that marks the lease
	// suspended. It lets a test land a release in that window to pin the
	// re-check under the lock (spoond-d76). Nil in production.
	pauseBeforeSuspend func(l *Lease)
	// restartBeforeRecheck, when set by a test, runs after restart has
	// re-checked the lease is not released and just before it takes the
	// store lock for the save, so a test can land a release in that
	// window (G2). Nil in production.
	restartBeforeRecheck func()
	// userDeleteStoreErr, when set by a test, returns an error for a named
	// user-delete store read (list_jobs, list_kept_builds,
	// unpin_kept_builds, drop_named_snapshots or the owner-state pre-check
	// read_owner_state), so a test can pin that the failed read answers an
	// incomplete cleanup (or a 500 pre-check) instead of a 200 or 404
	// (spoond-y0jj). It is consulted once per step before the real store
	// call. Nil in production.
	userDeleteStoreErr func(step string) error
	// saves tracks in-flight and recently failed named-snapshot saves in
	// memory (2.7, #83 A2): a concurrent same-key save answers 409, a
	// failed key is retryable, and both read absent after a restart.
	saves namedSaveInFlight
	// startingMu guards startingBuilds, the in-memory refcount of
	// named-snapshot builds a lease start is using right now (B1). A
	// start is invisible to the catalog between resolving the version
	// and writing the lease row, so delete-in-use, retention and the GC
	// read this set too: a version another start holds is never deleted
	// or pruned, and its build is a GC root.
	startingMu     sync.Mutex
	startingBuilds map[string]int
	// secretsGate serialises a named-snapshot save's secret scrub against
	// exec and job secret staging on the same lease (2.7, #83 B2): a save
	// holds it across the checkpoint, an exec/job staging takes it first
	// and answers 409 lease_busy while a save holds it.
	secretsGate secretsGate
	// admitQ holds creates waiting for admission (#129 part 1) and
	// serialises their admissions.
	admitQ admissionQueue
	// wakeScheduled guards against piling up wake-up passes: at most one
	// queued-admission retry runs at a time.
	wakeScheduled atomic.Bool
	// wakePending records a wake-up that arrived while a pass was
	// running. The running pass re-checks it before it finishes, so a
	// release that credits room mid-pass is never lost to the periodic
	// tick (#spoond-vbdj).
	wakePending atomic.Bool
	// admitPassHook, when set by a test, runs inside tryAdmitQueued once
	// the pass has judged every queued ticket and is about to return. It
	// lets a test hold a pass mid-flight while it frees room and wakes,
	// then release the pass and assert the wake is absorbed
	// (spoond-vbdj). Nil in production.
	admitPassHook func()

	// snapshotLimiters paces every substrate memory-snapshot write
	// (Pause and Checkpoint) process-wide (spoond-t1s). The default
	// limiter has width SNAPSHOT_WRITE_CONCURRENCY; the drain limiter has
	// its own width DRAIN_SNAPSHOT_CONCURRENCY and is used only by the
	// admin drain's pauses.
	snapshotLimiters snapshotLimiters
}

// NewService builds the lease service on sub. db is required: every
// mutation is persisted (U05). tokens maps legacy consumer tokens to
// consumer ids.
func NewService(sub substrate.Substrate, db *store.DB, tokens map[string]string, cfg ServiceConfig) *Service {
	svc := &Service{
		sub:                       sub,
		db:                        db,
		store:                     newStore(),
		tokens:                    tokens,
		cfg:                       cfg,
		sweepInterval:             5 * time.Second,
		sweepTimeout:              sweepTimeoutOrDefault(cfg.SweepTimeout),
		now:                       time.Now,
		diskCapacity:              statfsCapacity,
		diskUsage:                 store.BuildDiskUsage,
		appliedEgress:             map[string]string{},
		appliedInFlight:           map[string]struct{}{},
		createSecrets:             map[string]map[string]string{},
		log:                       log.Default(),
		probeEnabled:              true,
		probeTimeout:              20 * time.Second,
		rootfsProbeOK:             map[string]time.Time{},
		rootfsProbeFails:          map[string]*rootfsProbeFailure{},
		recoveryRetries:           map[string]*retryBudget{},
		lostSandboxDeleteAttempts: defaultLostSandboxDeleteAttempts,
		lostSandboxDeleteBackoff:  defaultLostSandboxDeleteBackoff,
		orphanSweepInterval:       defaultOrphanSweepInterval,
		orphanSandboxIDs:          map[string]struct{}{},
		orphanSweep:               newOrphanSweepState(),
		drainHeal:                 map[string]*drainHealState{},
		bus:                       newEventBus(),
		gcErr:                     newGCTracker(),
		liveJobSecrets:            map[string][]string{},
		timingOut:                 map[string]struct{}{},
		jobKillNotBefore:          map[string]time.Time{},
		stagedExecSecrets:         map[string]int{},
		stagedExecSecretNames:     map[string][]string{},
		pendingSecretRemovals:     map[string][]string{},
		saves:                     namedSaveInFlight{saves: map[string]*namedSaveState{}},
		startingBuilds:            map[string]int{},
		secretsGate:               secretsGate{saving: map[string]int{}, staging: map[string]int{}},
		deletedOwners:             map[string]bool{},
		jobStarts:                 map[string]*jobStartLock{},
	}
	svc.rootfsProbeInterval.Store(int64(time.Duration(DefaultRootfsProbeSecs) * time.Second))
	svc.snapshotLimiters = snapshotLimiters{
		def:   newSnapshotLimiter(cfg.SnapshotWriteConcurrency, svc.now, svc.log.Printf),
		drain: newSnapshotLimiter(cfg.DrainSnapshotConcurrency, svc.now, svc.log.Printf),
	}
	return svc
}

// SetMetrics installs the Prometheus metrics collector (issue #20).
// Called by the Server after NewServerWithLLM so the service can
// record pool, lease and quota events.
func (s *Service) SetMetrics(m *metrics.BackendMetrics) {
	s.metrics = m
}

// bakesRunning reports how many template bakes are in flight: the
// catalog's template builds still `building`. The image pipeline runs in
// the separate `spoond images build` process, so the backend sees its
// bakes only through the shared catalog; a stale row a killed build left
// is failed by the GC (spoond-4yl). The orphan sweep skips while this is
// non-zero (spoond-63a G3), and a catalog read failure is returned so the
// sweep can skip rather than delete blind (spoond-63a N1). CollectMetrics
// publishes the same count as spoond_builds_in_flight on every /metrics
// scrape, which is how the dashboard's "builds busy" cell reads it; the
// sweep guard and the gauge therefore share one number (spoond-rzz).
func (s *Service) bakesRunning(ctx context.Context) (int64, error) {
	if s.db == nil {
		return 0, nil
	}
	c, err := s.db.CountBuildingTemplateBuilds(ctx)
	if err != nil {
		return 0, err
	}
	return int64(c), nil
}

// SetGatewayToken marks the SSH gateway's service token, enabling
// trusted impersonation (U6/T5): requests carrying this token may set
// X-Spoond-User-Id to act as the SSH-authenticated user.
func (s *Service) SetGatewayToken(tok string) {
	s.gatewayToken = tok
}

// SetSandboxProbe configures the per-create integrity probe. enabled=false
// turns it off (every sandbox is then handed out unverified); timeout<=0
// leaves the default in place.
func (s *Service) SetSandboxProbe(enabled bool, timeout time.Duration) {
	s.probeEnabled = enabled
	if timeout > 0 {
		s.probeTimeout = timeout
	}
}

// SetRootfsProbe sets how often the rootfs liveness probe runs against
// running leases (spoond-5ca). 0 disables it entirely. A negative value
// leaves the default in place.
func (s *Service) SetRootfsProbe(secs int) {
	if secs >= 0 {
		s.rootfsProbeInterval.Store(int64(time.Duration(secs) * time.Second))
	}
}

// rootfsProbeEvery returns how often the rootfs liveness probe runs. It
// is safe to call while SetRootfsProbe runs.
func (s *Service) rootfsProbeEvery() time.Duration {
	return time.Duration(s.rootfsProbeInterval.Load())
}

// SetIdentities installs the identity store used for token→user and
// key→user resolution. Call before serving; when set, the first user in
// the store is the admin (KTD-2) and legacy consumer tokens still work.
func (s *Service) SetIdentities(ids *identity.Store) {
	s.identities = ids
}

// ResolveOwner resolves a bearer token to an owner identity. It prefers
// the identity store (user id) and falls back to the legacy consumer
// token map for single-user deployments.
func (s *Service) ResolveOwner(token string) (string, bool) {
	if s.identities != nil {
		if u := s.identities.UserByToken(token); u != nil {
			return u.ID, true
		}
	}
	owner, ok := s.tokens[token]
	return owner, ok
}

// exposedMap renders a lease's published ports as {"<port>": "<ip>:<port>"}.
// The published address is the sandbox's host address; only a live lease
// with a host address has anything to reach.
func exposedMap(l *Lease) map[string]string {
	out := map[string]string{}
	if !l.live() || l.HostIP == "" {
		return out
	}
	for _, p := range l.ExposePorts {
		out[fmt.Sprint(p)] = net.JoinHostPort(l.HostIP, fmt.Sprint(p))
	}
	return out
}

// lanRanges is RFC 1918 minus the sandbox networks 10.11.0.0/16 and
// 10.12.0.0/16: the private CIDRs the lan and internet policies permit.
var lanRanges = []string{
	"10.0.0.0/13",
	"10.8.0.0/15",
	"10.10.0.0/16",
	"10.13.0.0/16",
	"10.14.0.0/15",
	"10.16.0.0/12",
	"10.32.0.0/11",
	"10.64.0.0/10",
	"10.128.0.0/9",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// egressForLocked builds the lease's E2B egress policy. hostSvc is the
// host-service allowance every guest needs (LLM gateway, proxy, assets);
// dns is the guest resolver. The caller holds s.store.mu: in U09
// peerAllowances reads other leases.
func (s *Service) egressForLocked(l *Lease) substrate.Egress {
	hostSvc := substrate.PrivateAllowance{
		CIDR:     s.cfg.HostGuestAddr + "/32",
		TCPPorts: []uint32{uint32(s.cfg.HostGuestPort)},
	}
	// Guests resolve through the configured resolvers only
	// (SPOOND_GUEST_DNS_ADDR, comma-separated, baked into the guest image
	// by images/guest/spoond-guest-init). Empty = no allowance, and the
	// substrate then keeps the public DNS fallback for allow-listed domains.
	dns := dnsAllowances(s.cfg.GuestDNSAddr)
	guestDNS := len(dns) > 0
	// The fork's host-address guard admits a destination on the host only
	// when an allowance names both the IP and the port; the LAN ranges'
	// any-port allowances do not count. So lan and internet name the lease
	// API explicitly (restricted and none never reach it).
	var hostAPI []substrate.PrivateAllowance
	if s.cfg.HostAPIPort > 0 {
		hostAPI = []substrate.PrivateAllowance{{
			CIDR:     s.cfg.HostGuestAddr + "/32",
			TCPPorts: []uint32{uint32(s.cfg.HostAPIPort)},
		}}
	}
	policy := l.NetPolicy
	if policy == "" {
		policy = string(PolicyRestricted)
	}
	switch NetworkPolicy(policy) {
	case PolicyNone:
		return substrate.Egress{DeniedCIDRs: []string{"0.0.0.0/0"}}
	case PolicyInternet:
		// Public destinations stay allowed; listing the LAN ranges as
		// private allowances keeps private/LAN addresses reachable.
		return substrate.Egress{Private: append(append(append(lanPrivate(l, hostSvc), dns...), hostAPI...), s.peerAllowances(l)...), GuestDNS: guestDNS}
	case PolicyLAN:
		return substrate.Egress{
			DeniedCIDRs: []string{"0.0.0.0/0"},
			Private:     append(append(append(lanPrivate(l, hostSvc), dns...), hostAPI...), s.peerAllowances(l)...),
			GuestDNS:    guestDNS,
		}
	default: // restricted: the default when empty
		eg := substrate.Egress{
			DeniedCIDRs: []string{"0.0.0.0/0"},
			Private:     append([]substrate.PrivateAllowance{hostSvc}, dns...),
			GuestDNS:    guestDNS,
		}
		for _, entry := range l.NetAllow {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			cidr := entry
			if ip := net.ParseIP(entry); ip != nil {
				if ip.To4() == nil {
					cidr = ip.String() + "/128"
				} else {
					cidr = ip.String() + "/32"
				}
			} else if _, n, err := net.ParseCIDR(entry); err == nil {
				cidr = n.String()
			} else {
				// A NetAllow entry that names a lease (id, name, or the
				// "lease:"-prefixed forms) is a peer reference, not a
				// domain: peers are permitted through peerAllowances,
				// never resolved as domains (U09).
				if s.isPeerReference(entry) {
					continue
				}
				eg.AllowedDomains = append(eg.AllowedDomains, entry)
				continue
			}
			if isPrivateCIDR(cidr) {
				eg.Private = append(eg.Private, substrate.PrivateAllowance{CIDR: cidr})
			} else {
				eg.AllowedCIDRs = append(eg.AllowedCIDRs, cidr)
			}
		}
		eg.Private = append(eg.Private, s.peerAllowances(l)...)
		return eg
	}
}

// lanPrivate is the allowance list of the lan and internet policies:
// the LAN ranges (empty port scope), then the host service. The caller
// appends the configured guest DNS allowance.
func lanPrivate(l *Lease, hostSvc substrate.PrivateAllowance) []substrate.PrivateAllowance {
	out := make([]substrate.PrivateAllowance, 0, len(lanRanges)+1)
	for _, cidr := range lanRanges {
		out = append(out, substrate.PrivateAllowance{CIDR: cidr})
	}
	out = append(out, hostSvc)
	return out
}

// dnsAllowances turns the configured guest DNS addresses
// (SPOOND_GUEST_DNS_ADDR, comma-separated) into one allowance per
// address. Blank entries and surrounding whitespace are ignored, so a
// single address keeps working. Each entry must be an exact host
// address, never a CIDR.
//
// Each allowance names port 53 for TCP (layer 2's tcpfirewall); the
// private CIDR itself is exempted in the sandbox's netns firewall
// (layer 1), so UDP 53 to the same address is granted as well.
func dnsAllowances(addrs string) []substrate.PrivateAllowance {
	var out []substrate.PrivateAllowance
	for _, addr := range strings.Split(addrs, ",") {
		if a, ok := dnsAllowance(addr); ok {
			out = append(out, a)
		}
	}
	return out
}

// dnsAllowance turns one configured guest DNS address into a port-53
// allowance. A bare IPv4 or IPv6 address gets a full-length prefix (/32
// or /128); a blank entry yields no allowance. An entry carrying a
// prefix is rejected: the resolver is an exact host, and a CIDR would
// grant port 53 to a whole range.
func dnsAllowance(addr string) (substrate.PrivateAllowance, bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" || strings.Contains(addr, "/") {
		return substrate.PrivateAllowance{}, false
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return substrate.PrivateAllowance{}, false
	}
	cidr := addr + "/32"
	if ip.To4() == nil {
		cidr = addr + "/128"
	}
	return substrate.PrivateAllowance{CIDR: cidr, TCPPorts: []uint32{53}}, true
}

// isPrivateCIDR reports whether cidr lies within 10/8, 172.16/12,
// 192.168/16, 127/8 or 169.254/16.
func isPrivateCIDR(cidr string) bool {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	for _, base := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "169.254.0.0/16"} {
		_, b, _ := net.ParseCIDR(base)
		if b.Contains(n.IP) {
			return true
		}
	}
	return false
}

// egressFor is egressForLocked with the store lock taken. Call it where
// the lock is not held; call egressForLocked where it is (item 18 paths).
func (s *Service) egressFor(l *Lease) substrate.Egress {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.egressForLocked(l)
}

// peerAllowances permits egress into ranges other leases own (U09): the
// exposed ports of every other live lease that publishes them, on any
// owner — parity with the old shared bridge, where every published port
// was reachable from every sandbox whose own policy let it route there.
// Under restricted, a peer counts only when the allowlist names it. It
// is always called with s.store.mu held (from egressForLocked).
func (s *Service) peerAllowances(l *Lease) []substrate.PrivateAllowance {
	policy := l.NetPolicy
	if policy == "" {
		policy = string(PolicyRestricted)
	}
	if NetworkPolicy(policy) == PolicyNone {
		return nil
	}
	restricted := NetworkPolicy(policy) == PolicyRestricted
	var out []substrate.PrivateAllowance
	for _, p := range s.store.leases {
		if p.ID == l.ID || len(p.ExposePorts) == 0 || !p.live() || p.HostIP == "" {
			continue
		}
		if restricted && !netAllowNamesPeer(l.NetAllow, p) {
			continue
		}
		ports := make([]uint32, 0, len(p.ExposePorts))
		for _, port := range p.ExposePorts {
			ports = append(ports, uint32(port))
		}
		out = append(out, substrate.PrivateAllowance{CIDR: p.HostIP + "/32", TCPPorts: ports})
	}
	return out
}

// netAllowNamesPeer reports whether an egress allowlist names peer p:
// by id, by friendly name, or in the "lease:"+id / "lease:"+name forms.
func netAllowNamesPeer(allow []string, p *Lease) bool {
	for _, a := range allow {
		a = strings.TrimSpace(a)
		a = strings.TrimPrefix(a, "lease:")
		if a == p.ID || (p.Name != "" && a == p.Name) {
			return true
		}
	}
	return false
}

// isPeerReference reports whether a restricted allowlist entry names a
// lease — by id, by friendly name, or "lease:"-prefixed — and is
// therefore a peer reference rather than a domain. Called with
// s.store.mu held (from egressForLocked).
func (s *Service) isPeerReference(entry string) bool {
	name := strings.TrimPrefix(strings.TrimSpace(entry), "lease:")
	if name == "" {
		return false
	}
	for _, p := range s.store.leases {
		if p.ID == name || (p.Name != "" && p.Name == name) {
			return true
		}
	}
	return false
}

// canonicalEgress renders eg as the canonical JSON refreshPeers compares
// against the last applied value: sorted CIDR and domain lists, Private
// sorted by CIDR, and every allowance's TCPPorts sorted.
func canonicalEgress(eg substrate.Egress) string {
	eg.AllowedCIDRs = append([]string(nil), eg.AllowedCIDRs...)
	eg.DeniedCIDRs = append([]string(nil), eg.DeniedCIDRs...)
	eg.AllowedDomains = append([]string(nil), eg.AllowedDomains...)
	sort.Strings(eg.AllowedCIDRs)
	sort.Strings(eg.DeniedCIDRs)
	sort.Strings(eg.AllowedDomains)
	eg.Private = append([]substrate.PrivateAllowance(nil), eg.Private...)
	sort.Slice(eg.Private, func(i, j int) bool { return eg.Private[i].CIDR < eg.Private[j].CIDR })
	for i, a := range eg.Private {
		ports := append([]uint32(nil), a.TCPPorts...)
		sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
		eg.Private[i].TCPPorts = ports
	}
	b, err := json.Marshal(eg)
	if err != nil {
		return "" // Egress carries only strings and ints: cannot fail
	}
	return string(b)
}

// recordAppliedEgress remembers the canonical form of the egress config
// just applied to a lease's sandbox (create, pooled grant, or a live
// update), so the next refreshPeers does not re-apply it.
func (s *Service) recordAppliedEgress(leaseID string, eg substrate.Egress) {
	s.appliedMu.Lock()
	defer s.appliedMu.Unlock()
	s.appliedEgress[leaseID] = canonicalEgress(eg)
}

// forgetAppliedEgress drops a released lease's remembered egress config,
// so appliedEgress does not grow one entry per lease for the life of the
// process (spoond-966 L1).
func (s *Service) forgetAppliedEgress(leaseID string) {
	s.appliedMu.Lock()
	defer s.appliedMu.Unlock()
	delete(s.appliedEgress, leaseID)
}

// beginAppliedEgress marks a lease as mid-create before createSandbox
// records its egress memo. refreshPeers' prune skips in-flight ids, so a
// refresh that lands in the gap between the egress record and the
// store.leases insert does not drop the memo and force one redundant
// UpdateEgress (spoond-ob18). endAppliedEgress clears the mark once the
// lease is registered or the create has cleaned up.
func (s *Service) beginAppliedEgress(leaseID string) {
	s.appliedMu.Lock()
	defer s.appliedMu.Unlock()
	s.appliedInFlight[leaseID] = struct{}{}
}

// endAppliedEgress clears an in-flight egress hold. It is safe to call
// more than once and on a lease that was never marked.
func (s *Service) endAppliedEgress(leaseID string) {
	s.appliedMu.Lock()
	defer s.appliedMu.Unlock()
	delete(s.appliedInFlight, leaseID)
}

// refreshPeers re-applies every live lease's egress config whose value
// changed (U09): peer allowances move when leases expose ports, go live,
// or are released, and each affected sandbox needs an UpdateEgress. The
// store lock is held only to read state; substrate calls run without it.
// Callers must hold refreshMu (see runRefreshPeers).
func (s *Service) refreshPeers(ctx context.Context) {
	type update struct {
		leaseID, sandboxID, canon string
		eg                        substrate.Egress
	}
	s.store.mu.Lock()
	var upds []update
	for _, l := range s.store.leases {
		policy := l.NetPolicy
		if policy == "" {
			policy = string(PolicyRestricted)
		}
		if l.released || !l.live() || NetworkPolicy(policy) == PolicyNone || l.SandboxID == "" {
			continue
		}
		eg := s.egressForLocked(l)
		upds = append(upds, update{leaseID: l.ID, sandboxID: l.SandboxID, canon: canonicalEgress(eg), eg: eg})
	}
	s.store.mu.Unlock()

	for _, u := range upds {
		s.appliedMu.Lock()
		unchanged := s.appliedEgress[u.leaseID] == u.canon
		s.appliedMu.Unlock()
		if unchanged {
			continue
		}
		if err := s.sub.UpdateEgress(ctx, u.sandboxID, u.eg); err != nil {
			s.log.Printf("refreshPeers: update egress for %s: %v", u.leaseID, err)
			continue
		}
		// Re-check the lease under the store lock before writing the memo:
		// a release that landed while UpdateEgress ran cleared the entry,
		// and writing it here would resurrect it (spoond-966 follow-up).
		s.appliedMu.Lock()
		s.store.mu.Lock()
		if l := s.store.leases[u.leaseID]; l != nil && !l.released {
			s.appliedEgress[u.leaseID] = u.canon
		} else {
			delete(s.appliedEgress, u.leaseID)
		}
		s.store.mu.Unlock()
		s.appliedMu.Unlock()
	}

	// Drop any memo whose lease is no longer live and unreleased. A
	// release clears its own entry, but a refresh that raced it (or a
	// network-policy update that landed after the release) can have
	// re-added one; this pass reaps it. The pool placeholder is kept:
	// it never appears in the live store (spoond-966 follow-up), and a
	// lease mid-create keeps its memo: it is recorded before the lease
	// enters the store, and dropping it here would force one redundant
	// UpdateEgress (spoond-ob18).
	s.appliedMu.Lock()
	s.store.mu.Lock()
	for id := range s.appliedEgress {
		if id == "pool" {
			continue
		}
		if _, inflight := s.appliedInFlight[id]; inflight {
			continue
		}
		if l := s.store.leases[id]; l == nil || l.released || !l.live() {
			delete(s.appliedEgress, id)
		}
	}
	s.store.mu.Unlock()
	s.appliedMu.Unlock()
}

// refreshPeersAsync schedules one refreshPeers run on a goroutine. Runs
// are serialized by refreshMu; ctx cancellation does not stop the run —
// the refresh must outlive the request that triggered it. Called after
// grant/resume/restart/clone/fork/release of a lease with ExposePorts
// and after any network policy change (U09).
func (s *Service) refreshPeersAsync(ctx context.Context) {
	go s.runRefreshPeers(ctx)
}

// runRefreshPeers runs refreshPeers serialized by refreshMu.
func (s *Service) runRefreshPeers(ctx context.Context) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.refreshPeers(context.WithoutCancel(ctx))
}

// createSandbox admits and creates one sandbox for lease l from build b.
// sandboxID "" allocates a new E2B sandbox id; resume reuses the paused
// sandbox's id. On success the sandboxes row is upserted (upsert because
// resume reuses the sandbox id). The lease's hugepage admission runs
// here for every cold create and resume — the class decision (#128
// part 2) happened earlier on the path that owns the lease, and the
// plain capacity check stays with the sandbox it sizes.
func (s *Service) createSandbox(ctx context.Context, img store.ImageRow, b store.BuildRow, resume bool, sandboxID string, l *Lease) (sb substrate.Sandbox, err error) {
	if err := s.admit(ctx, b.MemoryMB); err != nil {
		return substrate.Sandbox{}, err
	}
	if sandboxID == "" {
		sandboxID = e2b.NewSandboxID()
	}
	// From the moment the id is chosen until the caller claims the
	// sandbox (the lease or pool entry is recorded), the orphan sweep
	// must not touch it: a create can leave its lease row for a while
	// (the integrity probe) or never, if it fails. A create that fails
	// here ends the window itself; on success the caller must call
	// endCreatingSandbox once it has claimed the sandbox or cleaned it
	// up. spoond-abc.
	s.beginCreatingSandbox(sandboxID)
	defer func() {
		if err != nil {
			s.endCreatingSandbox(sandboxID)
		}
	}()
	env := make(map[string]string, len(img.Env)+2)
	for k, v := range img.Env {
		env[k] = v
	}
	env["SPOOND_LEASE_ID"] = l.ID
	env["SPOOND_GATEWAY_URL"] = "http://" + s.cfg.HostGuestAddr + ":" + strconv.Itoa(s.cfg.HostGuestPort)
	eg := s.egressFor(l)
	start := time.Now()
	sb, err = s.sub.Create(ctx, substrate.CreateRequest{
		TemplateID:         b.TemplateID,
		BuildID:            b.BuildID,
		SandboxID:          sandboxID,
		KernelVersion:      b.KernelVersion,
		FirecrackerVersion: b.FirecrackerVersion,
		EnvdVersion:        b.EnvdVersion,
		VCPU:               uint32(b.VCPU),
		MemoryMB:           uint32(b.MemoryMB),
		DiskSizeMB:         uint32(b.DiskMB),
		Resume:             resume,
		EnvVars:            env,
		Metadata:           map[string]string{"lease_id": l.ID, "owner": l.Owner},
		EndAt:              l.ExpiresAt,
		Egress:             eg,
	})
	if s.metrics != nil {
		s.metrics.CreateDur.WithLabelValues(strconv.FormatBool(resume)).Observe(time.Since(start).Seconds())
	}
	if err != nil {
		// A failed resume Create can leave a half-started sandbox behind;
		// remove it (best effort) so the retry can reuse the same sandbox
		// id. Only a resume reuses an existing id, so only it needs the
		// cleanup; a refusal before the Create (admission, node status)
		// never reached the orchestrator and must not delete or log
		// (spoond-52c S2/NIT).
		if resume {
			s.deleteHalfSandbox(ctx, l, sandboxID)
		}
		return substrate.Sandbox{}, err
	}
	// A substrate may mint the id rather than honour the requested one;
	// track the returned id from here on so the caller's endCreatingSandbox
	// matches and the sweep keys on the id the substrate reports.
	if sb.ID != "" && sb.ID != sandboxID {
		s.endCreatingSandbox(sandboxID)
		sandboxID = sb.ID
		s.beginCreatingSandbox(sandboxID)
	}
	// The create can take minutes on a saturated host. If the lease was
	// released while this one was starting, the sandbox must not become
	// the lease's again: release already deleted the sandbox id it knew
	// about before this guest existed and dropped the row, and no path
	// may re-add one or save the lease back (spoond-775). Stop the fresh
	// sandbox here (bounded retries, spoond-63a) and tell the caller not
	// to save; the caller's own released checks and saveLeaseLocked's
	// guard cover a release that lands later. The deferred
	// endCreatingSandbox ends the in-flight mark.
	if s.leaseReleased(l) {
		s.log.Printf("create: lease %s was released while its sandbox %s started; stopping it", l.ID, sandboxID)
		s.deleteSandboxWithRetries(sandboxID, l.ID, "released")
		s.deleteSandboxRow(sandboxID)
		return substrate.Sandbox{}, errLeaseReleased
	}
	s.recordAppliedEgress(l.ID, eg)
	leaseID := l.ID
	if leaseID == "pool" {
		leaseID = "" // pool placeholder: pool sandboxes have no lease
	}
	s.upsertSandboxRow(store.SandboxRow{
		SandboxID:   sb.ID,
		LeaseID:     leaseID,
		BuildID:     sb.BuildID,
		ExecutionID: sb.ExecutionID,
		HostIP:      sb.HostIP,
		VCPU:        int(sb.VCPU),
		MemoryMB:    int(sb.MemoryMB),
		StartedAt:   sb.StartedAt,
		EndAt:       sb.EndAt,
	})
	return sb, nil
}

// DefaultSweepTimeout bounds one background sweep stage and each other
// background loop pass. It is deliberately generous (larger than any
// single bounded substrate RPC's default, so a whole pass of several
// RPCs still fits) while still finite: a wedged call frees the loop and
// the lease's busy flag within it.
const DefaultSweepTimeout = 15 * time.Minute

func sweepTimeoutOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultSweepTimeout
	}
	return d
}

// sweepCtx returns a context bounded by the sweep timeout for one
// background-loop stage.
func (s *Service) sweepCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.sweepTimeout)
}

// runSweepStage runs one background sweep stage under the sweep bound
// and logs when it is cut short, so a wedged stage is visible rather
// than silent.
func (s *Service) runSweepStage(ctx context.Context, name string, stage func(context.Context)) {
	sctx, cancel := s.sweepCtx(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		stage(sctx)
	}()
	select {
	case <-done:
	case <-sctx.Done():
		if ctx.Err() == nil {
			s.log.Printf("sweep: %s exceeded %s; abandoning this pass", name, s.sweepTimeout)
		}
	}
}

// Start begins the TTL sweeper and warm-pool refill. It runs until ctx
// is cancelled or Shutdown stops it.
func (s *Service) Start(ctx context.Context) {
	ctx, s.stopLoops = context.WithCancel(ctx)
	// Adopt a node another process left draining (a backend restart
	// between drain and undrain), so the self-heal loop does not believe
	// a draining node is healthy and undrain it mid-planned-stop
	// (spoond-52c B1). Best effort: an unreachable node leaves the state
	// to the self-heal loop.
	adoptCtx, cancelAdopt := context.WithTimeout(ctx, s.sweepTimeout)
	s.adoptDrainState(adoptCtx)
	cancelAdopt()
	go func() {
		t := time.NewTicker(s.sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Each stage runs under its own bound so a hung RPC wedges
				// only that stage, never the following ones or the next tick
				// (spoond-j3a).
				s.runSweepStage(ctx, "sweepExpired", s.sweepExpired)
				s.runSweepStage(ctx, "refillPool", s.refillPool)
				s.runSweepStage(ctx, "pruneJobs", s.pruneJobs)
			}
		}
	}()
	// Crash reconcile (U10): every 30 s while not draining, plus
	// immediately when NodeInfo goes from failing to succeeding (the
	// orchestrator came back).
	go func() {
		reconcile := time.NewTicker(30 * time.Second)
		defer reconcile.Stop()
		probe := time.NewTicker(s.sweepInterval)
		defer probe.Stop()
		nodeDown := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-reconcile.C:
				if s.draining.Load() {
					continue
				}
				s.reconcileCrash(ctx)
			case <-probe.C:
				if _, err := s.sub.NodeInfo(ctx); err != nil {
					nodeDown = true
					continue
				}
				if nodeDown {
					nodeDown = false
					if s.draining.Load() {
						continue
					}
					s.log.Printf("reconcile: node info recovered, reconciling crashes")
					s.reconcileCrash(ctx)
					continue
				}
			}
		}
	}()
	// Periodic orphan sandbox sweep (spoond-abc, spoond-63a): every
	// orphanSweepInterval, delete substrate sandboxes a lost lease owns,
	// an unclaimed sandbox seen unclaimed twice, and any id a failed
	// delete remembered. It is the backstop for a lost path's bounded
	// delete and for a guest a previous incarnation left, and it never
	// touches a live, busy, in-flight or pool sandbox. The startup pass
	// is ReconcileOrphans, so this loop is purely periodic.
	go func() {
		t := time.NewTicker(s.orphanSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runSweepStage(ctx, "sweepOrphanSandboxes", s.sweepOrphanSandboxes)
			}
		}
	}()
	// Periodic checkpoints (U10, 2.3 #122): leases whose effective
	// interval says so and that saw activity since their last snapshot,
	// one at a time, spaced 2 s apart. The loop ticks every minute.
	go s.runCheckpointLoop(ctx)
	// Snapshot catalog GC + disk accounting (U11): once 10 minutes
	// after the backend starts, then once an hour.
	go s.runGCCatalogLoop(ctx)
	// Node gauges (U11): refreshed every 15 s. CollectMetrics refreshes
	// the catalog-derived template-bake gauge on each /metrics scrape,
	// which is when the dashboard reads it (spoond-rzz).
	go s.runNodeMetricsLoop(ctx)
	// Promote burst leases into an owner's guarantee as leases churn
	// (#128): every 15 s, so the guarantee stays filled. This used to ride
	// the preemption resume queue; that background auto-resume is gone
	// (#145 D2), the promote sweep stays on its own ticker.
	go s.runPromoteLoop(ctx)
	// Background exec jobs (2.6, #135): every 10 s reconcile running job
	// records against the guest files (a backend restart or a broken
	// envd stream left them unobserved).
	go s.runJobReconcileLoop(ctx)
	// Rootfs liveness probe (spoond-5ca): every probe interval check that
	// each running lease's root block device still reads, and recover a
	// guest whose disk died like a crash.
	go s.runRootfsProbeLoop(ctx)
	// Queued admission (#129 part 1): retry waiting creates every 5 s
	// even when nothing signalled.
	go s.runAdmitQueueLoop(ctx)
	// Drain self-heal (spoond-52c): undrain a drain that outlived
	// DRAIN_MAX_SECS on a healthy node, and resume any lease left
	// Drained by a failed or deferred undrain.
	go s.startDrainHealLoop(ctx)
	// Webhook notifications (2.2, #117): forward the bus's
	// person-relevant events. Only when a notifier is installed.
	if s.notifier != nil {
		go s.runNotifyLoop(ctx)
	}
}

// runNodeMetricsLoop refreshes the NodeInfo-derived gauges every 15 s.
func (s *Service) runNodeMetricsLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.updateNodeMetrics(ctx)
		}
	}
}

// updateNodeMetrics sets the node gauges from NodeInfo; on error the
// gauges keep their last values. The fetched NodeInfo also refreshes
// the shared cache behind freeHugepageMiBLocked (#128 part 2): the burst
// reserve then reads the same value the gauges do, and a run with no
// burst admissions still keeps that value at most nodeInfoCacheTTL old.
func (s *Service) updateNodeMetrics(ctx context.Context) {
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		return
	}
	s.nodeInfoMu.Lock()
	s.nodeInfoCache = info
	s.nodeInfoAt = s.now()
	s.nodeInfoMu.Unlock()
	if s.metrics == nil {
		return
	}
	s.metrics.NodeRunning.Set(float64(info.RunningSandboxes))
	s.metrics.NodeWork.Set(float64(info.OutstandingWork))
	s.metrics.NodeHugepagesFree.Set(float64(info.FreeHugepageBytes()))
}

// refillPool pre-creates cfg.PoolSize sandboxes for every image with a
// current build so grants can be served from the warm pool instead of
// cold-creating.
func (s *Service) refillPool(ctx context.Context) {
	// The drain pauses everything; refill stays off until undrain (U10).
	if s.cfg.PoolSize <= 0 || s.draining.Load() {
		return
	}
	imgs, err := s.db.ListImages(ctx)
	if err != nil {
		s.log.Printf("refillPool: list images: %v", err)
		return
	}
	for _, img := range imgs {
		if img.CurrentBuildID == "" {
			continue
		}
		s.warmPool(ctx, img)
	}
}

// sweepExpired releases leases whose TTL has passed. Persistent leases
// are not TTL-swept (the consumer keeps them alive via keep-alive and
// disposes via delete), but when IdleTimeout is set every persistent
// lease is auto-suspended after that long without activity
// (exec/stream/proxy/keep-alive all bump LastActive). A suspended
// sandbox keeps its state snapshot and is cheap to resume.
func (s *Service) sweepExpired(ctx context.Context) {
	// Draining pauses every lease and undrain resumes it; the idle sweep
	// must not fight the drain (U10). The held-lease limits (2.1) skip
	// with it.
	if s.draining.Load() {
		return
	}
	s.store.mu.Lock()
	s.flushLastActiveLocked(ctx)
	var expired []*Lease
	var idleSuspend []*Lease
	now := s.now()
	for _, l := range s.store.leases {
		if l.released {
			continue
		}
		// A held lease (non-empty holder) is left alone by both sweeps:
		// not released at its TTL, not idle-suspended. Its own limits
		// (hold expiry, idle suspend, stale release, pressure, critical)
		// run in runHeldRules below.
		if !l.Persistent && !l.held() && now.After(l.ExpiresAt) {
			expired = append(expired, l)
			continue
		}
		if l.Persistent && !l.held() && s.effectiveIdleSuspend(l) <= 0 && s.cfg.IdleTimeout > 0 && !l.Suspended && !s.hasRunningJobLocked(l.ID) && now.After(l.LastActive.Add(s.cfg.IdleTimeout)) {
			s.log.Printf("idle sweep: suspending persistent lease %s (idle since %s)", l.ID, l.LastActive.Format(time.RFC3339))
			idleSuspend = append(idleSuspend, l)
		}
	}
	s.store.mu.Unlock()
	// Held-lease limits (2.1): hold expiry, stale release, idle suspend
	// (shortened under pressure) and the critical-disk release — in that
	// order. A hold expiring this tick clears the holder, so an
	// already-expired TTL releases the lease in the second pass below;
	// seenNow keeps leases already collected out of it.
	s.runHeldRules(ctx, now)
	// Per-lease idle reclamation (2.5, #129 part 2): leases with their own
	// idle_suspend are suspended on that threshold and not on the plain
	// idle timeout or held rule 1 (both skipped such leases above and in
	// suspendIdleHeld).
	s.suspendIdleLeases(ctx, now)
	seenNow := make(map[*Lease]bool, len(expired))
	for _, l := range expired {
		seenNow[l] = true
	}
	s.store.mu.Lock()
	now = s.now()
	for _, l := range s.store.leases {
		if l.released || l.held() || l.Persistent || seenNow[l] {
			continue
		}
		if now.After(l.ExpiresAt) {
			expired = append(expired, l)
		}
	}
	s.store.mu.Unlock()
	// Suspend idle leases in small, staggered batches. Each suspend is a
	// snapshot write on the node; a large backlog (e.g. after a long test
	// session) must not produce one big burst. Cap per tick and space
	// them out — with the 5s sweep tick, 13 idle leases clear in ~25s
	// instead of a single burst. When the process-wide snapshot limiter is
	// busy (a hand suspend, a checkpoint or the drain is writing), stand
	// down for this tick and retry next tick rather than queue the batch
	// behind the running write (spoond-t1s).
	const maxSuspendPerTick = 3
	suspended := 0
	for _, l := range idleSuspend {
		if suspended >= maxSuspendPerTick {
			break
		}
		if s.snapshotBusy() {
			break
		}
		if _, err := s.suspendWith(ctx, l.Owner, l.ID, suspendPolicy{reason: suspendReasonIdle}); err != nil {
			s.log.Printf("idle sweep: suspend %s: %v", l.ID, err)
			continue
		}
		suspended++
		time.Sleep(500 * time.Millisecond)
	}
	for _, l := range expired {
		s.releaseBecause(ctx, l, "TTL expired")
	}
}

// touch records activity on a lease so the idle sweeper doesn't reclaim
// it. Returns nil for unknown/released leases.
func (s *Service) touch(id string) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if l := s.store.leases[id]; l != nil && !l.released {
		now := time.Now()
		l.LastActive = now
		s.store.lastActiveDirty[id] = now
	}
}

// incRunningJob records that a lease gained a running background job.
func (s *Service) incRunningJob(leaseID string) {
	s.store.mu.Lock()
	s.store.runningJobs[leaseID]++
	s.store.mu.Unlock()
}

// decRunningJob records that a lease lost a running background job.
func (s *Service) decRunningJob(leaseID string) {
	s.store.mu.Lock()
	if s.store.runningJobs[leaseID] <= 1 {
		delete(s.store.runningJobs, leaseID)
	} else {
		s.store.runningJobs[leaseID]--
	}
	s.store.mu.Unlock()
}

// hasRunningJobLocked reports whether a lease has a running background
// job. Call with s.store.mu held (the idle checks run under it).
func (s *Service) hasRunningJobLocked(leaseID string) bool {
	return s.store.runningJobs[leaseID] > 0
}

// hasRunningJob reports whether a lease has a running background job,
// taking the store lock itself.
func (s *Service) hasRunningJob(leaseID string) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.store.runningJobs[leaseID] > 0
}

// markActive records activity on a lease and persists it immediately:
// the guest heartbeat needs its write to land now (a crash must not
// lose the one signal that keeps the lease alive), so unlike touch it
// does not batch into the sweeper's dirty set — and it drops any pending
// batched update for the lease, which would otherwise flush an older
// timestamp over the newer one. Only LastActive moves — ExpiresAt,
// persistence and state are untouched, and a suspended lease is never
// resumed. Returns false for unknown/released leases.
func (s *Service) markActive(id string) bool {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.released {
		s.store.mu.Unlock()
		return false
	}
	l.LastActive = time.Now()
	delete(s.store.lastActiveDirty, id)
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	return true
}

// keepAlive extends a persistent lease's expiry so the sweeper never
// reclaims it. Non-persistent leases are rejected (their TTL is fixed).
func (s *Service) keepAlive(owner, id string, ttl time.Duration) (*Lease, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l, ok := s.store.leases[id]
	if !ok || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		return nil, err
	}
	if !l.Persistent {
		return nil, errNotPersistent
	}
	if ttl <= 0 {
		ttl = s.cfg.MaxTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	l.ExpiresAt = time.Now().Add(ttl)
	l.LastActive = time.Now() // keep-alive is activity
	s.saveLeaseLocked(l)
	return l, nil
}

// release deletes the lease's sandbox (nil when already gone), drops the
// sandboxes row, then the lease row; shares cascade. Builds are left for
// U11's GC.
func (s *Service) release(ctx context.Context, l *Lease) {
	s.releaseBecause(ctx, l, "lease released")
}

// releaseBecause is release with the reason its released event carries
// (the dashboard's events panel and SSE clients read it).
func (s *Service) releaseBecause(ctx context.Context, l *Lease, reason string) {
	s.store.mu.Lock()
	if l.released {
		s.store.mu.Unlock()
		return
	}
	l.released = true
	// A running lease's hugepages come back with the release; a
	// suspended one holds none.
	freedMiB := 0
	if l.live() {
		freedMiB = l.MemoryMB
	}
	s.store.mu.Unlock()
	// Create-time secrets are memory-only bookkeeping; the sandbox they
	// were staged into goes with the release (#80).
	s.clearCreateSecrets(l.ID)
	// The per-lease in-memory maps go too: the applied-egress memo
	// (spoond-966 L1) and any deferred exec-time secret removals, whose
	// files went with the sandbox (spoond-966 L1).
	s.forgetAppliedEgress(l.ID)
	s.clearPendingSecretRemovals(l.ID)

	if err := s.sub.Delete(ctx, l.SandboxID); err != nil {
		s.log.Printf("release: delete %s: %v", l.SandboxID, err)
		s.rememberOrphanSandbox(l.SandboxID)
	}
	s.deleteSandboxRow(l.SandboxID)
	// The lease's kept builds stop being GC roots (2.3, #121): the next
	// pass may reclaim them.
	if err := s.db.DeleteKeptBuilds(ctx, l.ID); err != nil {
		s.log.Printf("release: delete kept builds of %s: %v", l.ID, err)
	}
	// Jobs still running die with the sandbox (2.6, #135). Their rows
	// cascade away with the lease below, so no exit or reconcile path
	// will ever finish them: settle them here — the running gauge, the
	// remembered secret names (the files went with the guest) and a
	// job_lost event, so a watcher learns the job ended.
	s.settleJobsOfReleasedLease(ctx, l)
	// The rootfs probe's per-lease state goes with the lease.
	s.forgetRootfs(l.ID)
	// A released lease has no in-flight recovery: drop its retry budget
	// (spoond-dxq S2), so a later release or sandbox reuse cannot trip a
	// stale budget.
	s.clearRecoveryRetries(l)
	// A release drops any drain self-heal backoff the lease carried, so a
	// later planned restart's deferral starts fresh (spoond-52c B3).
	s.clearDrainHeal(l.ID)
	s.store.mu.Lock()
	delete(s.store.leases, l.ID)
	delete(s.store.shares, l.ID)
	// The lease's job rows cascade with the lease row; drop the in-memory
	// running-job count too, which only exit/lost would otherwise clear.
	delete(s.store.runningJobs, l.ID)
	s.deleteLeaseLocked(l.ID)
	s.store.mu.Unlock()
	// A version that retention spared only because this lease ran from it
	// is dropped once no live lease uses it (S5): re-run the name's
	// retention now that the lease row is gone.
	s.rerunSnapshotRetention(context.WithoutCancel(ctx), l)
	if len(l.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	s.emitLeaseEvent(l.ID, l.Owner, LeaseReleased, reason)
	s.creditNodeInfo(freedMiB)
	// The owner's guarantee may have room now (#128).
	s.promoteBurst(l.Owner)
	// A release frees hugepages and quota: retry waiting creates (#129).
	s.wakeAdmissionQueue()
}

// errQuotaExceeded is returned when a user hits their concurrent-lease
// cap (T4/#31) or their running-lease memory cap (#128). The API layer
// maps it to HTTP 429. The error's message says which cap it was.
var errQuotaExceeded = fmt.Errorf("lease quota exceeded")

// memoryQuotaError marks the memory-cap refusal (#128) as distinct from
// the lease-count refusal, so queued admission (#129) knows which one it
// may wait out: a full memory cap frees when other leases end, while the
// count cap is final. Its Error and Unwrap are the wrapped
// errQuotaExceeded, so callers that only test errors.Is(err,
// errQuotaExceeded) are unchanged.
type memoryQuotaError struct{ err error }

func (e *memoryQuotaError) Error() string { return e.err.Error() }
func (e *memoryQuotaError) Unwrap() error { return e.err }

// isMemoryQuotaRefusal reports whether err is the memory-cap refusal.
func isMemoryQuotaRefusal(err error) bool {
	var m *memoryQuotaError
	return errors.As(err, &m)
}

// errMemoryQuotaExceeded is errQuotaExceeded carrying the memory cap in
// its message (#128): reserving MiB past the user's max_mib answers 429
// with a message that names the memory limit.
func errMemoryQuotaExceeded(maxMiB int) error {
	return &memoryQuotaError{fmt.Errorf("%w: memory limit of %d MiB exceeded", errQuotaExceeded, maxMiB)}
}

// reserveQuota enforces a user's concurrent-lease cap (T4/#31) and
// running-lease memory cap (#128) before granting, and RESERVES n slots
// and their MiB atomically (security review #37 H2): the counts and the
// reservations happen under the same store lock, so concurrent creates
// cannot both pass max_leases or max_mib. The caller MUST call
// releaseQuotaReservation when it finishes (success or failure).
// memoryMB is the per-lease memory charge in MiB — the image's
// memory_mb, stamped on the lease at grant time. newLease is false for
// a resume (or any other operation that turns an existing suspended
// lease back into a running one): those pass ONLY the memory check —
// the lease already holds its max_leases slot — while create, clone and
// fork pass both. Returns errQuotaExceeded when a cap is hit. Owners
// without an identity-store user (legacy consumer tokens) are uncapped.
//
// An owner whose identity was removed mid-create is refused here
// (spoond-q4j): without the user row every other check is skipped (the
// legacy-uncapped branch), and accepting the grant would recreate
// exactly the ownerless, uncapped lease DELETE /api/users/{id} exists to
// remove. reserveQuota refuses a marked owner; a grant that passed the
// check before the mark is released by the cleanup's lease re-scan (the
// commit holds the store lock, and the mark is set before the re-scan,
// so the two are ordered).
//
// Only RUNNING leases are charged (#128): a suspended lease holds no
// hugepages, so suspending frees the charge and resuming re-passes this
// check (resume calls it with the lease's own charge before the sandbox
// comes back).
func (s *Service) reserveQuota(owner string, n int, memoryMB int, newLease bool) error {
	if s.identities == nil {
		return nil
	}
	s.ownerDeleteMu.Lock()
	deleted := s.deletedOwners[owner]
	s.ownerDeleteMu.Unlock()
	if deleted {
		return errOwnerGone
	}
	u := s.identities.UserByID(owner)
	if u == nil {
		return nil
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	// The count cap stays for new leases only (T4/#31): a resume does
	// not add a lease, so counting the (suspended) leases being resumed
	// would cap a user out of resuming their own work (#128).
	if newLease && u.MaxLeases > 0 {
		active := 0
		for _, l := range s.store.leases {
			if !l.released && l.Owner == owner {
				active++
			}
		}
		if active+s.store.pending[owner]+n > u.MaxLeases {
			if s.metrics != nil {
				s.metrics.QuotaExceeded.Inc()
			}
			return errQuotaExceeded
		}
	}
	if u.MaxMiB > 0 && memoryMB > 0 {
		// usedMiBLocked counts only live leases, so the in-flight
		// reservation below enters the sum exactly once (through
		// pendingMiB) until the lease lands — the exact-fit race (two
		// creates that together fill the cap) must admit exactly one.
		if s.usedMiBLocked(owner)+n*memoryMB > u.MaxMiB {
			if s.metrics != nil {
				s.metrics.QuotaExceeded.Inc()
			}
			return errMemoryQuotaExceeded(u.MaxMiB)
		}
	}
	s.store.pending[owner] += n
	s.store.pendingMiB[owner] += n * memoryMB
	return nil
}

// usedMiB reports the owner's current memory charge (#128): the sum of
// the leases' stamped memory_mb over the owner's RUNNING leases
// (suspended ones hold no hugepages and are not charged), plus any MiB
// reserved by in-flight creations. A lease stamped 0 (its image row is
// gone) charges nothing; grant stamps the charge so this sum never
// reads the catalog.
func (s *Service) usedMiB(owner string) int {
	if s.identities == nil {
		return 0
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.usedMiBLocked(owner)
}

// usedMiBLocked is usedMiB without the lock. Caller holds the store
// lock. It reads only in-memory state — no catalog I/O — so
// reservations stay atomic without holding the lock across SQLite.
func (s *Service) usedMiBLocked(owner string) int {
	used := s.store.pendingMiB[owner]
	for _, l := range s.store.leases {
		if l.released || l.Owner != owner || !l.live() {
			continue
		}
		used += l.MemoryMB
	}
	return used
}

// releaseQuotaReservation drops n reservations made by reserveQuota.
func (s *Service) releaseQuotaReservation(owner string, n int, memoryMB int) {
	if s.identities == nil {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.pending[owner] <= n {
		delete(s.store.pending, owner)
	} else {
		s.store.pending[owner] -= n
	}
	if s.store.pendingMiB[owner] <= n*memoryMB {
		delete(s.store.pendingMiB, owner)
	} else {
		s.store.pendingMiB[owner] -= n * memoryMB
	}
}

// imageBuild loads an image row and its current build. An image without
// a row, or whose current build is unset, is unknown (→ 404).
func (s *Service) imageBuild(ctx context.Context, image string) (store.ImageRow, store.BuildRow, error) {
	img, err := s.db.GetImage(ctx, image)
	if errors.Is(err, store.ErrNotFound) {
		return store.ImageRow{}, store.BuildRow{}, errUnknownImage
	}
	if err != nil {
		return store.ImageRow{}, store.BuildRow{}, fmt.Errorf("load image %s: %w", image, err)
	}
	if img.CurrentBuildID == "" {
		return store.ImageRow{}, store.BuildRow{}, errUnknownImage
	}
	b, err := s.db.GetBuild(ctx, img.CurrentBuildID)
	if err != nil {
		return store.ImageRow{}, store.BuildRow{}, fmt.Errorf("load build %s: %w", img.CurrentBuildID, err)
	}
	return img, b, nil
}

// imageMemoryMB returns the image's memory_mb for the checkpoint pause
// log line (2.3, #122); 0 when the catalog cannot answer.
func (s *Service) imageMemoryMB(ctx context.Context, image string) (int, error) {
	img, err := s.db.GetImage(ctx, image)
	if err != nil {
		return 0, err
	}
	return img.MemoryMB, nil
}

// imageMiB is imageMemoryMB with its own bounded context: the memory
// charge of one lease of the image (#128). The value is stamped on the
// lease so accounting and release use the same number the admission
// check reserved with. 0 when the catalog cannot answer: an uncharged
// lease, and admission never blocks on a catalog hiccup — but it is
// logged, so an operator can see the cap is not being enforced for
// that image.
func (s *Service) imageMiB(image string) int {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	mb, err := s.imageMemoryMB(ctx, image)
	if err != nil {
		s.log.Printf("memory quota: no memory_mb for image %s, leasing it uncharged: %v", image, err)
		return 0
	}
	return mb
}

// grant creates a new lease for owner: served from the warm pool when
// one is configured and stocked, else a cold create from the image's
// current build. Persistent leases are intended for interactive use:
// they are not TTL-swept (see keepAlive) and the consumer drives their
// lifecycle. holder/holderURL name what holds the lease ("" = unheld,
// normal sweeping); validation lives in validateHolder (the API layer
// calls it before granting). The hold's clock itself is stamped by
// grantHeld; grant leaves it zero so an unheld grant carries no hold.
func (s *Service) grant(ctx context.Context, owner, image string, ttl time.Duration, persistent bool, netPolicy string, netAllow []string, holder, holderURL string, createSecrets map[string]string, exposePorts ...int) (*Lease, error) {
	return s.grantLease(ctx, leaseRequest{owner: owner, image: image, ttl: ttl, persistent: persistent,
		netPolicy: netPolicy, netAllow: netAllow, holder: holder, holderURL: holderURL,
		createSecrets: createSecrets, exposePorts: exposePorts})
}

// leaseRequest carries one admission path's inputs to grantLease: the
// plain grant() arguments plus the class knobs (#128 part 2) — the
// request's burst flag and priority, and whether the admission is a new
// lease (create, clone, fork) or the re-admission of an existing one
// (resume, restart of a suspended lease).
type leaseRequest struct {
	owner, image      string
	ttl               time.Duration
	persistent        bool
	netPolicy         string
	netAllow          []string
	holder, holderURL string
	createSecrets     map[string]string
	exposePorts       []int
	burst             bool
	priority          int
	// snapshot, when set, starts the lease from a named snapshot version
	// (2.7, #83 task 2) instead of the image's current build. The image
	// is the version's image and the memory charge is the version's
	// memory_mb. The pool is bypassed and lease.snapshot_build_id is
	// stamped, so retention never drops a version a live lease runs
	// from.
	snapshot *snapshotStart
	// leaseID is the id the caller already allocated and announced —
	// the queued-admission path (#129) reserves it when the create is
	// queued so the `queued` and `created` events name the same lease.
	// Empty allocates a fresh id at grant.
	leaseID string
}

// snapshotStart is one resolved named-snapshot version a lease create
// starts from. The row carries the name, version, build and memory_mb
// the grant needs.
type snapshotStart struct {
	row store.NamedSnapshotRow
}

// beginStartingBuild takes a reference on a named-snapshot build a lease
// start is about to use (B1). From here until endStartingBuild the build
// is visible to delete-in-use, retention and the GC as if a live lease
// ran from it, so none of them can remove it out from under the start.
func (s *Service) beginStartingBuild(buildID string) {
	if buildID == "" {
		return
	}
	s.startingMu.Lock()
	s.startingBuilds[buildID]++
	s.startingMu.Unlock()
}

// endStartingBuild releases the reference taken by beginStartingBuild.
// It must run on every path that took one, success or failure.
func (s *Service) endStartingBuild(buildID string) {
	if buildID == "" {
		return
	}
	s.startingMu.Lock()
	if s.startingBuilds[buildID] <= 1 {
		delete(s.startingBuilds, buildID)
	} else {
		s.startingBuilds[buildID]--
	}
	s.startingMu.Unlock()
}

// startingBuildSet returns a copy of the builds a start currently holds,
// for the store's retention query and the GC's root walk.
func (s *Service) startingBuildSet() map[string]bool {
	s.startingMu.Lock()
	defer s.startingMu.Unlock()
	if len(s.startingBuilds) == 0 {
		return nil
	}
	out := make(map[string]bool, len(s.startingBuilds))
	for id := range s.startingBuilds {
		out[id] = true
	}
	return out
}

// startingBuild reports whether a start currently holds buildID.
func (s *Service) startingBuild(buildID string) bool {
	s.startingMu.Lock()
	defer s.startingMu.Unlock()
	return s.startingBuilds[buildID] > 0
}

// snapshotStartError is returned when a lease cannot start from a named
// snapshot version on this host (2.7, #83): the build row or its files
// are gone, or the substrate refuses the build. The API maps it to 409
// cannot_start; there is no retry loop.
type snapshotStartError struct {
	name    string
	version int64
	cause   string
}

func (e *snapshotStartError) Error() string {
	return fmt.Sprintf("snapshot %s@%d cannot start on this host (%s); save it again", e.name, e.version, e.cause)
}

// grantLease is grant with the class admission (#128 part 2): the
// lease's class is decided before the sandbox is created — guaranteed
// within the owner's guaranteed_mib, burst above it or on an explicit
// burst request, the burst held to the node's reserve — and stamped on
// the lease with the requested priority.
func (s *Service) grantLease(ctx context.Context, req leaseRequest) (*Lease, error) {
	owner, image, ttl, persistent := req.owner, req.image, req.ttl, req.persistent
	start := time.Now()
	// A create from a named snapshot starts from the version's build, not
	// the image's current build (2.7, #83): the image is the version's
	// image and the memory charge is the version's memory_mb. The lease
	// is never served from the warm pool. A version whose build row is
	// gone answers cannot_start before admission.
	snap := req.snapshot
	var img store.ImageRow
	var b store.BuildRow
	memoryMB := 0
	if snap != nil {
		image = snap.row.Image
		var err error
		img, err = s.db.GetImage(ctx, image)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("load image %s: %w", image, err)
		}
		b, err = s.db.GetBuild(ctx, snap.row.BuildID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, &snapshotStartError{name: snap.row.Name, version: snap.row.Version, cause: "build files missing"}
		}
		if err != nil {
			return nil, fmt.Errorf("load snapshot build %s: %w", snap.row.BuildID, err)
		}
		// A build that is not ready (deleted, failed, or still being
		// written) cannot be started: answer cannot_start rather than
		// handing the substrate a build that will not run (B1).
		if b.State != "ready" {
			return nil, &snapshotStartError{name: snap.row.Name, version: snap.row.Version, cause: "build is " + b.State}
		}
		// A version saved against a different host envd/firecracker or
		// orchestrator cannot start here (S2): compare the row's recorded
		// versions against the host's and name the component that
		// differs. Only a known host version is compared; an unknown one
		// ("") never refuses.
		if cause, ok := s.snapshotHostMismatch(ctx, snap.row); ok {
			return nil, &snapshotStartError{name: snap.row.Name, version: snap.row.Version, cause: cause}
		}
		memoryMB = snap.row.MemoryMB
	} else {
		var err error
		img, b, err = s.imageBuild(ctx, image)
		if err != nil {
			return nil, err
		}
		memoryMB = img.MemoryMB
	}
	// The lease costs its image's memory_mb (#128): admission checks the
	// user's running-lease memory before the sandbox is created, and the
	// deferred release below drops the same number it reserved. A
	// snapshot start charges the version's memory_mb.
	if err := s.reserveQuota(owner, 1, memoryMB, true); err != nil {
		return nil, err
	}
	// The reservation becomes the real lease when it's stored below;
	// the deferred release runs on BOTH success and failure: on failure
	// it frees the slot, on success the lease is already counted as
	// active so the pending reservation must be dropped (security
	// review #37 H2). Both mutations take the same store lock, so a
	// concurrent reserveQuota sees a consistent active+pending count.
	defer func() { s.releaseQuotaReservation(owner, 1, memoryMB) }()
	if s.metrics != nil {
		s.metrics.LeasesTotal.Inc()
	}
	now := time.Now()
	leaseID := req.leaseID
	if leaseID == "" {
		leaseID = newID()
	}
	lease := &Lease{
		ID:          leaseID,
		Owner:       owner,
		Image:       image,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
		Persistent:  persistent,
		LastActive:  now,
		NetPolicy:   req.netPolicy,
		NetAllow:    req.netAllow,
		ExposePorts: req.exposePorts,
		Holder:      req.holder,
		HolderUrl:   req.holderURL,
		State:       "running",
		// The lease's MiB charge (#128): the image's memory_mb, the very
		// number reserveQuota admitted with, so accounting and release
		// always agree and quota sums never re-read the catalog.
		MemoryMB: memoryMB,
		// Every lease starts on generation 1 (2.2) and on the host's
		// checkpoint and idle-suspend defaults (-1), unless the create
		// request carries its own (the API stamps it after grant).
		Generation:         1,
		CheckpointInterval: checkpointIntervalHost,
		IdleSuspend:        idleSuspendHost,
		TemplateID:         img.TemplateID,
		// The class knobs (#128 part 2): priority rides the request,
		// and the class stamp lands after admitClass decides it below.
		Priority: req.priority,
		Burst:    req.burst,
	}
	if lease.TemplateID == "" {
		// A snapshot start does not need the image's current build, so a
		// vanished image row is not fatal: the build row carries the
		// template.
		lease.TemplateID = b.TemplateID
	}
	if snap != nil {
		// The named-snapshot version this lease starts from (2.7, #83):
		// the build is persisted so retention never drops a version a
		// live lease runs from, and the name/version ride the response
		// and the lease detail (A3).
		lease.SnapshotBuildID = snap.row.BuildID
		lease.SnapshotName = snap.row.Name
		lease.SnapshotVersion = snap.row.Version
	}
	// Class admission (#128 part 2): decides guaranteed vs burst (the
	// charge above is the sum the guarantee is measured against) and
	// holds a burst lease to the node's reserve. A refusal answers
	// before any sandbox exists.
	class, err := s.admitClass(ctx, owner, memoryMB, req.burst, "")
	if err != nil {
		return nil, err
	}
	lease.Class = class
	// Hold the lease's egress memo in flight from before any sandbox is
	// created until it is registered in the store (or the grant fails and
	// cleans up). createSandbox records the memo before the lease row
	// exists, and a refreshPeers landing in that gap must not prune it as
	// a memo with no live lease (spoond-ob18).
	s.beginAppliedEgress(lease.ID)
	defer s.endAppliedEgress(lease.ID)

	// Pool: pop the oldest entry for the image. The pool serves
	// persistent and non-persistent grants alike. A pooled sandbox was
	// created for the "pool" placeholder lease, so its egress policy and
	// EndAt are updated for the new lease, and its sandboxes row moves to
	// it. Entries from another build, with no row, or unhealthy are
	// discarded and the next one is tried. A create from a named
	// snapshot never uses the pool (the pooled sandbox is the image's
	// current build, not the version's).
	if s.cfg.PoolSize > 0 && snap == nil {
		for {
			s.store.mu.Lock()
			pool := s.store.pool[image]
			var id string
			if len(pool) > 0 {
				id = pool[0]
				s.store.pool[image] = pool[1:]
				s.removePoolLocked(id)
				// The pool entry is gone before the lease row names the
				// sandbox: hold it in flight, in the same critical
				// section, so the orphan sweep never sees it unclaimed
				// in the gap (spoond-abc).
				s.beginCreatingSandbox(id)
			}
			s.store.mu.Unlock()
			if id == "" {
				break
			}
			row, err := s.db.GetSandbox(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				s.log.Printf("grant: pooled %s (%s) has no sandboxes row, discarding", id, image)
				s.discardPoolSandbox(ctx, id)
				s.endCreatingSandbox(id)
				continue
			}
			if err != nil {
				s.discardPoolSandbox(ctx, id)
				s.endCreatingSandbox(id)
				return nil, fmt.Errorf("load pooled sandbox %s: %w", id, err)
			}
			if row.BuildID != img.CurrentBuildID {
				s.log.Printf("grant: pooled %s (%s) was built from %s, want %s, discarding", id, image, row.BuildID, img.CurrentBuildID)
				s.discardPoolSandbox(ctx, id)
				s.endCreatingSandbox(id)
				continue
			}
			if err := s.sub.Health(ctx, id); err != nil {
				s.log.Printf("grant: pooled %s (%s) is unhealthy, discarding: %v", id, image, err)
				s.discardPoolSandbox(ctx, id)
				s.endCreatingSandbox(id)
				continue
			}
			eg := s.egressFor(lease)
			if err := s.sub.UpdateEgress(ctx, id, eg); err != nil {
				s.discardPoolSandbox(ctx, id)
				s.endCreatingSandbox(id)
				return nil, fmt.Errorf("update egress on pooled sandbox: %w", err)
			}
			s.recordAppliedEgress(lease.ID, eg)
			if err := s.sub.UpdateEndAt(ctx, id, lease.ExpiresAt); err != nil {
				s.discardPoolSandbox(ctx, id)
				s.endCreatingSandbox(id)
				// The egress was recorded before the EndAt update failed: drop
				// it with the discarded sandbox so a failed pooled grant leaves
				// no memo behind (spoond-966 follow-up).
				s.forgetAppliedEgress(lease.ID)
				return nil, fmt.Errorf("update end at on pooled sandbox: %w", err)
			}
			row.LeaseID = lease.ID
			s.upsertSandboxRow(row)
			lease.SandboxID = id
			lease.HostIP = row.HostIP
			lease.ExposedIP = row.HostIP
			lease.BuildID = row.BuildID
			lease.pooled = true
			// The lease row will claim the sandbox below; keep it safe
			// until then, then release the in-flight hold.
			break
		}
	}

	if lease.SandboxID == "" {
		sb, err := s.createSandbox(ctx, img, b, false, "", lease)
		if err != nil {
			// A create the orchestrator does not know (missing build
			// files) cannot start here (2.7, #83 S2): map only the
			// structural ErrNotFound. A capacity, draining or transport
			// failure keeps its current retryable handling; an
			// incompatible version is caught before the create by the
			// host-version check above.
			if snap != nil && errors.Is(err, substrate.ErrNotFound) {
				return nil, &snapshotStartError{name: snap.row.Name, version: snap.row.Version, cause: "build files missing"}
			}
			return nil, err
		}
		lease.SandboxID = sb.ID
		lease.HostIP = sb.HostIP
		lease.ExposedIP = sb.HostIP
		lease.BuildID = b.BuildID
	}

	// A lease started from a snapshot gets its copy-side markers before
	// any exec the API runs in it (A4/A7): /run/spoond/lease-id,
	// /run/spoond/generation and /run/spoond/started-from exist before
	// the integrity probe below. These are not best effort: if any write
	// fails the sandbox is deleted and the create fails with no lease row
	// (S3), because a copy that cannot tell source from copy is not safe
	// to hand out.
	if snap != nil {
		if err := s.writeGeneration(lease); err != nil {
			_ = s.sub.Delete(ctx, lease.SandboxID)
			s.deleteSandboxRow(lease.SandboxID)
			s.endCreatingSandbox(lease.SandboxID)
			s.forgetAppliedEgress(lease.ID)
			return nil, fmt.Errorf("write lease-id and generation markers: %w", err)
		}
		if err := s.writeStartedFromMarker(lease); err != nil {
			_ = s.sub.Delete(ctx, lease.SandboxID)
			s.deleteSandboxRow(lease.SandboxID)
			s.endCreatingSandbox(lease.SandboxID)
			s.forgetAppliedEgress(lease.ID)
			return nil, fmt.Errorf("write started-from marker: %w", err)
		}
		// The snapshot's memory may still carry secret files (a save
		// scrubs them before its checkpoint, but a copy must not trust
		// that). Remove anything under /run/secrets before the new
		// lease's own create-time secrets are staged below, so the copy
		// holds only its own (A4/S4).
		if _, err := s.scrubAllSecrets(ctx, lease.SandboxID, nil); err != nil {
			_ = s.sub.Delete(ctx, lease.SandboxID)
			s.deleteSandboxRow(lease.SandboxID)
			s.endCreatingSandbox(lease.SandboxID)
			s.forgetAppliedEgress(lease.ID)
			return nil, fmt.Errorf("scrub secrets on snapshot start: %w", err)
		}
	}

	// The integrity probe runs through exec before the sandbox is handed
	// out. On probe failure the sandbox is deleted (A1 §17 item 7's leak,
	// fixed).
	if err := s.probeSandbox(ctx, lease.SandboxID); err != nil {
		_ = s.sub.Delete(ctx, lease.SandboxID)
		s.deleteSandboxRow(lease.SandboxID)
		s.endCreatingSandbox(lease.SandboxID)
		s.forgetAppliedEgress(lease.ID)
		return nil, err
	}
	// Create-time secrets (#80) are staged after the integrity probe so
	// the probe never runs with them present, and before the lease is
	// registered, so a staging failure cannot hand out (or strand) a
	// lease without its secrets.
	if len(req.createSecrets) > 0 {
		if err := s.stageSecrets(ctx, lease.SandboxID, req.createSecrets); err != nil {
			_ = s.sub.Delete(ctx, lease.SandboxID)
			s.deleteSandboxRow(lease.SandboxID)
			s.endCreatingSandbox(lease.SandboxID)
			s.forgetAppliedEgress(lease.ID)
			return nil, fmt.Errorf("stage lease secrets: %w", err)
		}
		s.setCreateSecrets(lease.ID, req.createSecrets)
	}

	// The guest's generation file always exists (2.2): generation 1 on a
	// fresh lease. Best effort, after the probe so a recycled sandbox
	// never sees it. A snapshot start already wrote it (and its
	// started-from marker) before the probe.
	if snap == nil {
		s.writeGeneration(lease)
	}

	s.store.mu.Lock()
	// Final guard against a delete that raced this grant (spoond-q4j). A
	// grant that passed reserveQuota before the owner was marked deleted
	// holds the store lock across its commit, and deleteUserData sets the
	// mark before it re-scans leases, so either this guard sees the mark
	// and refuses, or the commit lands first and the cleanup's re-scan
	// releases the lease. A deleted owner's create is always refused
	// while the sandbox is still deletable, before the lease is visible.
	s.ownerDeleteMu.Lock()
	deleted := s.deletedOwners[owner]
	s.ownerDeleteMu.Unlock()
	if deleted {
		s.store.mu.Unlock()
		// The deferred releaseQuotaReservation drops the reservation.
		_ = s.sub.Delete(ctx, lease.SandboxID)
		s.deleteSandboxRow(lease.SandboxID)
		s.endCreatingSandbox(lease.SandboxID)
		// The create-time secrets were staged into this sandbox; its
		// files go with the delete, so drop the in-memory copy too or it
		// would outlive the sandbox that never became a lease.
		s.clearCreateSecrets(lease.ID)
		// The pooled grant recorded the lease's egress memo before this
		// guard; drop it with the sandbox that never became a lease
		// (spoond-q4j NIT), as every other failure path does.
		s.forgetAppliedEgress(lease.ID)
		return nil, errOwnerGone
	}
	s.store.leases[lease.ID] = lease
	s.saveLeaseLocked(lease)
	s.store.mu.Unlock()
	// The lease now claims the sandbox: the orphan sweep may see it as
	// owned even before the next List. spoond-abc.
	s.endCreatingSandbox(lease.SandboxID)
	s.countImageUse(image)
	if len(lease.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	// Observed only when the lease is actually returned, so the
	// histogram measures the full grant — pool hit, cold create and the
	// integrity probe — and failed grants stay out of the latency.
	if s.metrics != nil {
		s.metrics.LeaseGrantDur.Observe(time.Since(start).Seconds())
	}
	detail := fmt.Sprintf("granted from image %s in %s", image, eventDuration(time.Since(start)))
	if snap != nil {
		detail = fmt.Sprintf("started from snapshot %s@%d in %s", snap.row.Name, snap.row.Version, eventDuration(time.Since(start)))
	}
	s.emitLeaseEvent(lease.ID, owner, LeaseCreated, detail)
	return lease, nil
}

// countImageUse records one grant of image for the dashboard's catalog.
// A failed write is logged and counted, never fails the grant.
func (s *Service) countImageUse(image string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.CountImageUse(ctx, image); err != nil {
		s.storeError("count_image_use", image, err)
	}
}

// discardPoolSandbox deletes a pooled sandbox that failed validation:
// the substrate sandbox and its rows go, and grant tries the next entry.
func (s *Service) discardPoolSandbox(ctx context.Context, id string) {
	_ = s.sub.Delete(ctx, id)
	s.deleteSandboxRow(id)
}

// suspend pauses a persistent lease's sandbox into a new build and stops
// it. The lease stays; resume restores it with the same sandbox id.
func (s *Service) suspend(ctx context.Context, owner, id string) (*Lease, error) {
	return s.suspendWith(ctx, owner, id, suspendPolicy{})
}

// suspendWith is suspend with the structured suspension facts an
// automatic caller records (#145 D6): the plain IDLE_TIMEOUT_SECS sweep
// passes reason idle. A hand suspend passes the zero policy.
func (s *Service) suspendWith(ctx context.Context, owner, id string, pol suspendPolicy) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		s.store.mu.Unlock()
		return nil, err
	}
	if !l.Persistent {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	s.store.mu.Unlock()

	if _, err := s.pauseLeaseWith(ctx, l, false, pol); err != nil {
		return nil, err
	}
	return l, nil
}

// pauseLease is suspend without the owner/persistence checks: the admin
// drain pauses non-persistent leases too (U10). It marks the lease busy
// (a second operation on a busy lease returns errLeaseBusy), pauses into
// a new build, inserts the pause build row and marks the lease
// suspended — plus Drained when drained.
// LastAction values a pause records before any rule overwrites them.
const (
	pauseActionHand    = "suspend/hand"
	pauseActionDrain   = "drain/suspend"
	pauseActionPreempt = "preempt/suspend"
)

// Suspend reasons (#145 D6): the "reason" field of an automatic
// lease.suspended event, the lease's suspend_reason and the 409
// lease_suspended body. idle is the plain IDLE_TIMEOUT_SECS sweep;
// idle_suspend is the per-lease idle_suspend threshold; hold_lapsed is a
// hold that expired; pressure is held rule 1 shortened under pressure
// (rule 4) and, from the pressure order on, its eviction steps; preempt
// is the resume queue reclaiming hugepages for a guaranteed admission.
// A hand or drain suspend has no reason ("").
const (
	suspendReasonIdle        = "idle"
	suspendReasonIdleSuspend = "idle_suspend"
	suspendReasonHoldLapsed  = "hold_lapsed"
	suspendReasonPressure    = "pressure"
	suspendReasonPreempt     = "preempt"
)

// suspendPolicy carries the structured suspension facts a pause stamps on
// the lease and its suspended event (#145 D6): reason names why ("" for
// a hand or drain suspend), policyStep the pressure order's step (""
// until that order names steps).
type suspendPolicy struct {
	reason     string
	policyStep string
}

func (s *Service) pauseLease(ctx context.Context, l *Lease, drained bool) (string, error) {
	return s.pauseLeaseWith(ctx, l, drained, suspendPolicy{})
}

// pauseLeaseWith is pauseLease with the structured suspension facts an
// automatic caller records (#145 D6): the reason and pressure policy step
// are stamped on the lease (persisted) and carried by the suspended
// event. A hand or drain pause passes the zero policy.
func (s *Service) pauseLeaseWith(ctx context.Context, l *Lease, drained bool, pol suspendPolicy) (string, error) {
	s.store.mu.Lock()
	if l.busy {
		s.store.mu.Unlock()
		return "", errLeaseBusy
	}
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)
	return s.pauseLeaseBody(ctx, l, drained, pol)
}

// pauseLeaseBody is the sub work of a pause. Callers own the busy
// window; it must not be called with s.store.mu held. drained selects
// the drain limiter for the snapshot write (only the admin drain sets
// it); every other pause uses the default limiter. The write waits for
// a slot, bounded by ctx. pol carries the automatic suspension facts
// (#145 D6); it is stamped on the lease before the suspended event.
func (s *Service) pauseLeaseBody(ctx context.Context, l *Lease, drained bool, pol suspendPolicy) (string, error) {
	release, err := s.snapshotAcquire(ctx, drained)
	if err != nil {
		return "", err
	}
	defer release()
	buildID, refs, err := s.sub.Pause(ctx, l.SandboxID, l.TemplateID)
	if err != nil {
		return "", err
	}
	// The pause build records what was snapshotted: versions and sizes
	// copied from the parent build row. Its own size_bytes is measured
	// at write time (2.3 #125), like a checkpoint build's.
	parent, err := s.db.GetBuild(ctx, l.BuildID)
	if err != nil {
		return "", fmt.Errorf("load parent build %s: %w", l.BuildID, err)
	}
	now := time.Now()
	if err := s.db.InsertBuild(ctx, store.BuildRow{
		BuildID:            buildID,
		Kind:               "pause",
		TemplateID:         l.TemplateID,
		Image:              l.Image,
		ParentBuildID:      l.BuildID,
		SourceSandboxID:    l.SandboxID,
		Owner:              l.Owner,
		State:              "ready",
		KernelVersion:      parent.KernelVersion,
		FirecrackerVersion: parent.FirecrackerVersion,
		EnvdVersion:        parent.EnvdVersion,
		VCPU:               parent.VCPU,
		MemoryMB:           parent.MemoryMB,
		DiskMB:             parent.DiskMB,
		SizeBytes:          s.measureNewBuildOnDisk(buildID),
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		return "", fmt.Errorf("insert pause build: %w", err)
	}
	s.settleBuildSize(buildID, s.pauseChainObserver(buildID))
	// The new build's headers reference the blocks of other builds
	// (A3 C3); GC keeps them (U11).
	if err := s.db.AddBuildRefs(ctx, buildID, append(refs.RootfsBuildIDs, refs.MemfileBuildIDs...)); err != nil {
		return "", fmt.Errorf("store pause build refs: %w", err)
	}
	if s.pauseBeforeSuspend != nil {
		s.pauseBeforeSuspend(l)
	}
	s.store.mu.Lock()
	if l.released {
		// The lease was released while the pause was in flight: do not
		// mark it suspended, delete its (already gone) sandbox row or
		// credit its memory a second time. This check runs under the
		// same lock that sets Suspended and saves, so a release that
		// lands after the build and refs are written is seen here and
		// the pause reports the release instead of returning success
		// and letting its caller emit suspended, credit memory again or
		// bump an idle/preempt metric for a released lease (spoond-d76).
		// The pause build stays as an ordinary, GC-able candidate
		// (spoond-775).
		s.store.mu.Unlock()
		s.log.Printf("pause: lease %s was released during its pause; build %s left unreferenced", l.ID, buildID)
		return "", errLeaseReleased
	}
	l.setState("suspended")
	l.Suspended = true
	l.ResumeBuildID = buildID
	if drained {
		l.Drained = true
	}
	// Every pause records why the lease is suspended now. The idle
	// sweep, the held rules and preemption overwrite this right after
	// with their own action; a hand suspend or a drain keeps it. Without
	// it a stale rule action from an earlier suspension (e.g. an
	// idle_suspend the lease was since resumed from) would still
	// describe this one, and an idle-suspended-only behaviour such as
	// resume-on-next-call would apply to a lease suspended by hand.
	l.LastAction = pauseActionHand
	if drained {
		l.LastAction = pauseActionDrain
	}
	l.LastActionAt = s.now()
	// The structured suspension facts (#145 D6): the reason
	// (idle|idle_suspend|hold_lapsed|pressure|preempt), the pressure
	// order's step, the pause build it wrote and when describe an
	// automatic suspend. A hand or drain pause has no automatic reason,
	// so all four stay empty and the GET omits them; the lease still
	// names the pause build in last_action/resume_build_id. Stale facts
	// from an earlier suspension are dropped first, and all four are
	// cleared on resume.
	clearSuspendFactsLocked(l)
	if pol.reason != "" {
		l.SuspendReason = pol.reason
		l.SuspendPolicyStep = pol.policyStep
		l.SuspendBuildID = buildID
		l.SuspendedAt = l.LastActionAt
	}
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.deleteSandboxRow(l.SandboxID)
	s.emitSuspendEvent(l.ID, l.Owner, buildID, pol.reason, pol.policyStep)
	// A pause frees the lease's hugepages and quota: retry waiting
	// creates (#129).
	// The pause freed the lease's hugepages: the next admission inside
	// the cache window sees them (every pause: hand, drain, idle, held
	// rules and preemption alike).
	s.creditNodeInfo(l.MemoryMB)
	// A paused guaranteed lease frees its owner's guarantee (#128).
	if l.Class != ClassBurst {
		s.promoteBurst(l.Owner)
	}
	s.wakeAdmissionQueue()
	return buildID, nil
}

// resume restores a suspended persistent lease: create with snapshot
// from the pause build, same sandbox id.
func (s *Service) resume(ctx context.Context, owner, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if !l.Persistent && !l.held() {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	s.store.mu.Unlock()
	return s.resumeLease(ctx, l)
}

// resumeAny is resume without the owner check, for the SSH gateway's
// service token: before a session starts, the gateway resumes a
// suspended lease the connecting user is already authorised for (a held
// lease suspended by an idle rule resumes on next use this way).
// Persistent and held leases only, as for resume.
func (s *Service) resumeAny(ctx context.Context, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if !l.Persistent && !l.held() {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	s.store.mu.Unlock()
	return s.resumeLease(ctx, l)
}

// resumeLease is resume without the owner/persistence checks: the
// undrain resumes drained non-persistent leases through it (U10). It
// marks the lease busy (a second operation on a busy lease returns
// errLeaseBusy) and runs the resume.
//
// A resuming lease needs its hugepages back, so it passes the memory
// check like any create (#128): only running leases are charged, and
// the charge returns when the sandbox comes back. A nil error has
// already dropped the (zero-cost) reservation; errQuotaExceeded leaves
// the lease suspended.
func (s *Service) resumeLease(ctx context.Context, l *Lease) (*Lease, error) {
	s.store.mu.Lock()
	if l.busy {
		s.store.mu.Unlock()
		return nil, errLeaseBusy
	}
	// A lost lease cannot be resumed: its sandbox is gone and the crash
	// reconcile already gave up on it. Return the reason so the caller
	// answers 410 lease_lost.
	if err := lostErr(l); err != nil {
		s.store.mu.Unlock()
		return nil, err
	}
	// A running lease has nothing to resume. Restoring its pause build
	// again would roll the guest's memory back to that snapshot, so the
	// resume is a no-op that returns the lease as it is.
	if !l.Suspended && l.State == "running" {
		s.store.mu.Unlock()
		return l, nil
	}
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)
	// The lease's own stamp is the charge to re-acquire (#128): the
	// number admission reserved with is the number released when the
	// lease suspends again. A resume adds no lease, so only the memory
	// cap applies — the lease keeps its max_leases slot.
	memPer := l.MemoryMB
	if err := s.reserveQuota(l.Owner, 1, memPer, false); err != nil {
		return nil, err
	}
	defer func() { s.releaseQuotaReservation(l.Owner, 1, memPer) }()
	// Class re-admission (#128 part 2): a demand-burst lease stays
	// burst (and held to the reserve); a lease that burst because the
	// guarantee was full may come back guaranteed now that the charge
	// has room — the class follows the owner's current standing.
	class, err := s.admitClass(ctx, l.Owner, memPer, l.Burst, l.ID)
	if err != nil {
		return nil, err
	}
	l.Class = class
	if _, err := s.resumeLeaseBody(ctx, l); err != nil {
		return nil, err
	}
	// Written on every resume too, so a lease created before generations
	// existed gets the file the first time it comes back.
	s.writeGeneration(l)
	return l, nil
}

// resumeLeaseBody is the sub work of a resume (U08's resume steps 1–4).
// Callers own the busy window; it must not be called with s.store.mu
// held.
func (s *Service) resumeLeaseBody(ctx context.Context, l *Lease) (*Lease, error) {
	resumeBuild := l.ResumeBuildID
	image := l.Image

	img, err := s.db.GetImage(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("load image %s: %w", image, err)
	}
	b, err := s.db.GetBuild(ctx, resumeBuild)
	if err != nil {
		return nil, fmt.Errorf("load build %s: %w", resumeBuild, err)
	}
	sb, err := s.createSandbox(ctx, img, b, true, l.SandboxID, l)
	if err != nil {
		// createSandbox already removed any half-started sandbox after a
		// failed Create, inside the busy window, so the retry can reuse
		// the same sandbox id (spoond-52c S2/NIT). A create refused because
		// the lease was released has already stopped its fresh sandbox
		// there too (spoond-775), so nothing is deleted twice.
		return nil, err
	}
	// A release that landed just after the create returned must not be
	// undone by the save below: stop the fresh guest and leave the lease
	// released (spoond-775, spoond-63a). The store lock below re-checks
	// the race.
	if s.leaseReleased(l) {
		s.log.Printf("resume: lease %s was released during its resume; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return nil, errLeaseReleased
	}
	s.store.mu.Lock()
	if l.released {
		// Released while the resume started: stop the fresh sandbox
		// (bounded retries, spoond-63a) and leave no lease or sandbox row
		// behind (spoond-775, spoond-52c S4).
		s.store.mu.Unlock()
		s.log.Printf("resume: lease %s was released during its resume; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return nil, errLeaseReleased
	}
	l.HostIP = sb.HostIP
	l.ExposedIP = sb.HostIP
	l.BuildID = resumeBuild
	// A preempted lease comes back here: read the preemption before
	// setState clears it, for the event's detail below (#128 part 3).
	preempted := !l.PreemptedAt.IsZero()
	l.setState("running")
	l.Suspended = false
	// The drain paused this lease into its pause build; an owner's own
	// resume (or any other resume) brings it back running, so the flag
	// must not linger — otherwise it stays disabled for resume-on-next-
	// call and a later undrain would resume a lease the owner suspended
	// by hand (spoond-52c L4).
	l.Drained = false
	l.LastActive = time.Now()
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.endCreatingSandbox(sb.ID)
	// The secrets tmpfs does not survive a snapshot cycle: re-write the
	// lease's create-time secrets after the sandbox is back (#80).
	s.restageCreateSecrets(ctx, l, "resume")
	// The lease is no longer drained, so its self-heal backoff must not
	// outlive the flag: a later planned restart's deferral starts fresh
	// (spoond-52c B3).
	s.clearDrainHeal(l.ID)
	if len(l.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	if preempted {
		s.emitLeaseEvent(l.ID, l.Owner, LeaseResumed, "after preemption")
	} else {
		s.emitLeaseEvent(l.ID, l.Owner, LeaseResumed, "resumed from build "+resumeBuild)
	}
	// The lease is running again from its sandbox: any crash-recovery
	// budget keyed by that sandbox is stale (spoond-dxq B2).
	s.clearRecoveryRetries(l)
	// A resume frees its prior preemption and can move capacity: retry
	// waiting creates (#129).
	s.wakeAdmissionQueue()
	return l, nil
}

// restart restarts a lease. mode "" or "warm" is the default: a
// persistent running lease suspends then resumes (lossless through the
// pause build); a persistent suspended lease resumes; a non-persistent
// lease gets a fresh sandbox. mode "cold" runs the fresh-sandbox path
// for any lease, persistent or not (#120): the sandbox is deleted and a
// new one is created from the image's current build, keeping the lease
// id, owner, holder, name, network policy and exposed ports, re-writing
// the create-time secrets, bumping the generation and rewriting
// /run/spoond/generation. A persistent lease stays persistent but loses
// its resume point (its pause builds stop being its resume point; the
// next suspend sets resume_build_id again), and a suspended lease comes
// back running.
func (s *Service) restart(ctx context.Context, owner, id, mode string) (*Lease, error) {
	if mode != "" && mode != "warm" && mode != "cold" {
		return nil, errBadRestartMode
	}
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		s.store.mu.Unlock()
		return nil, err
	}
	if l.busy {
		s.store.mu.Unlock()
		return nil, errLeaseBusy
	}
	l.busy = true
	persistent := l.Persistent
	suspended := l.Suspended
	s.store.mu.Unlock()
	defer s.endBusy(l)

	if mode == "cold" {
		return s.restartCold(ctx, owner, l)
	}
	if suspended {
		// A suspended lease holds no hugepages, so its charge was freed
		// at suspend; warm-restarting it brings the guest back, so it
		// re-passes the memory check before the sandbox comes back,
		// exactly like a resume (#128). The restart adds no lease, so
		// only the memory cap applies.
		if err := s.reserveQuota(owner, 1, l.MemoryMB, false); err != nil {
			return nil, err
		}
		defer func() { s.releaseQuotaReservation(owner, 1, l.MemoryMB) }()
		// Class re-admission (#128 part 2), as for a resume: a
		// demand-burst lease stays burst, a guarantee-burst one may
		// fall back to guaranteed.
		class, err := s.admitClass(ctx, owner, l.MemoryMB, l.Burst, l.ID)
		if err != nil {
			return nil, err
		}
		l.Class = class
	}

	if persistent {
		if !suspended {
			if _, err := s.pauseLeaseBody(ctx, l, false, suspendPolicy{}); err != nil {
				return nil, err
			}
		}
		// A persistent restart is a snapshot round-trip: the guest resumes
		// from the pause build it just wrote, so its processes continue
		// where they were and the generation stays (the resume rewrites
		// the guest file with the current value). Only the non-persistent
		// path below, a fresh sandbox, loses the memory and bumps.
		if _, err := s.resumeLeaseBody(ctx, l); err != nil {
			return nil, err
		}
		s.emitLeaseEvent(l.ID, owner, LeaseRestarted, "restarted (snapshot round-trip)")
		return l, nil
	}

	_ = s.sub.Delete(ctx, l.SandboxID)
	s.deleteSandboxRow(l.SandboxID)
	img, b, err := s.imageBuild(ctx, l.Image)
	if err != nil {
		return nil, err
	}
	sb, err := s.createSandbox(ctx, img, b, false, "", l)
	if err != nil {
		return nil, err
	}
	// A release that landed while the create ran must not be undone by the
	// save below: createSandbox's own released check can miss a release
	// whose Delete ran before the fresh guest was registered, so stop it
	// here too and skip the save (spoond-775, spoond-63a). Re-check under
	// the store lock, as the release can land after the first check.
	if s.leaseReleased(l) {
		s.log.Printf("restart: lease %s was released during its restart; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return nil, errLeaseReleased
	}
	if s.restartBeforeRecheck != nil {
		s.restartBeforeRecheck()
	}
	s.store.mu.Lock()
	if l.released {
		// Released while the fresh guest started: stop it (bounded
		// retries, spoond-63a) and write no lease or sandbox row back
		// (spoond-775).
		s.store.mu.Unlock()
		s.log.Printf("restart: lease %s was released during its restart; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return nil, errLeaseReleased
	}
	l.SandboxID = sb.ID
	l.HostIP = sb.HostIP
	l.ExposedIP = sb.HostIP
	l.BuildID = b.BuildID
	l.setState("running")
	l.Suspended = false
	l.LastActive = time.Now()
	s.bumpGenerationLocked(l)
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.endCreatingSandbox(sb.ID)
	s.writeGeneration(l)
	// The fresh sandbox has no crash-recovery budget: a stale one must not
	// bypass reconcile's 'present' check (spoond-dxq B2).
	s.clearRecoveryRetries(l)
	// The fresh guest runs none of the old jobs: every running one is
	// lost (2.6, #135).
	s.markLeaseJobsLost(ctx, l.ID, l.Owner, "lease restarted; the job did not survive")
	// A fresh sandbox never had the lease's secrets: re-write them
	// (create-time only; exec-time secrets ride their request) (#80).
	s.restageCreateSecrets(ctx, l, "restart")
	if len(l.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	s.emitLeaseEvent(l.ID, owner, LeaseRestarted, "cold-restarted from image "+l.Image)
	return l, nil
}

// restartCold is the cold path of restart (#120): any lease, persistent
// or not, running or suspended, gets a fresh guest from the image's
// current build. The lease keeps its id, owner, holder, name, network
// policy and exposed ports — everything else about the guest starts
// over: the generation bumps and the create-time secrets are re-written
// into the new sandbox. A persistent lease keeps being persistent, but
// its resume_build_id is cleared: the pause builds it accumulated are no
// longer its resume point (the next suspend sets it as usual). A
// suspended lease comes back running.
func (s *Service) restartCold(ctx context.Context, owner string, l *Lease) (*Lease, error) {
	// The fresh guest runs the image's current memory_mb, so that is the
	// charge to re-admit for a suspended lease (#128) — fetched before
	// anything else, so a catalog failure answers before any change. A
	// cold restart adds no lease, so only the memory cap applies. A
	// running lease is already charged; its stamp is re-set below.
	img, b, err := s.imageBuild(ctx, l.Image)
	if err != nil {
		return nil, err
	}
	if l.Suspended {
		// A suspended lease holds no hugepages, so its charge was freed
		// at suspend; the fresh guest needs it back, so the cold restart
		// re-passes the memory check before any sandbox is created.
		if err := s.reserveQuota(owner, 1, img.MemoryMB, false); err != nil {
			return nil, err
		}
		defer func() { s.releaseQuotaReservation(owner, 1, img.MemoryMB) }()
		// Class re-admission (#128 part 2) against the image's current
		// charge — the number the fresh guest runs (and is stamped with
		// below), as for the memory check above.
		class, err := s.admitClass(ctx, owner, img.MemoryMB, l.Burst, l.ID)
		if err != nil {
			return nil, err
		}
		l.Class = class
	}
	// The fresh guest is created before the old one goes: a failed
	// create (no capacity) leaves the lease exactly as it was, running
	// or suspended, instead of live with no sandbox.
	sb, err := s.createSandbox(ctx, img, b, false, "", l)
	if err != nil {
		return nil, err
	}
	// A release that landed just after the create returned must not be
	// undone by the save below (spoond-775, spoond-63a). Re-check under
	// the store lock, as the release can land after the first check.
	if s.leaseReleased(l) {
		s.log.Printf("restart: lease %s was released during its cold restart; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return nil, errLeaseReleased
	}
	if old := l.SandboxID; old != "" && old != sb.ID {
		_ = s.sub.Delete(ctx, old)
		s.deleteSandboxRow(old)
	}
	s.store.mu.Lock()
	if l.released {
		// Released while the fresh guest started: stop it (bounded
		// retries, spoond-63a) and write no lease or sandbox row back
		// (spoond-775).
		s.store.mu.Unlock()
		s.log.Printf("restart: lease %s was released during its cold restart; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return nil, errLeaseReleased
	}
	l.SandboxID = sb.ID
	l.HostIP = sb.HostIP
	l.ExposedIP = sb.HostIP
	l.BuildID = b.BuildID
	l.setState("running")
	l.Suspended = false
	// The fresh guest comes from the image's current build, so the
	// lease's charge is re-stamped from that image row (#128) — the
	// number the check above (or the running charge) was admitted with.
	l.MemoryMB = img.MemoryMB
	// The pause builds stop being the lease's resume point: the next
	// suspend writes a fresh one.
	l.ResumeBuildID = ""
	l.LastActive = time.Now()
	s.bumpGenerationLocked(l)
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.endCreatingSandbox(sb.ID)
	s.writeGeneration(l)
	// The fresh sandbox has no crash-recovery budget (spoond-dxq B2).
	s.clearRecoveryRetries(l)
	// The fresh guest runs none of the old jobs: every running one is
	// lost (2.6, #135).
	s.markLeaseJobsLost(ctx, l.ID, l.Owner, "lease restarted; the job did not survive")
	// A fresh sandbox never had the lease's secrets: re-write them
	// (create-time only; exec-time secrets ride their request) (#80).
	s.restageCreateSecrets(ctx, l, "restart")
	if len(l.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	s.emitLeaseEvent(l.ID, owner, LeaseRestarted, "cold")
	return l, nil
}

// checkpointLease checkpoints a running sandbox into a new build,
// inserts the checkpoint build row (versions and sizes copied from the
// parent build row; refs stored from the Checkpoint response), and
// applies the checkpoint bookkeeping to the source lease (item 18). The
// fresh build's size_bytes is measured at write time (2.3 #125), so it
// shows in /api/snapshots before the next hourly accounting pass. It
// returns the new build row.
func (s *Service) checkpointLease(ctx context.Context, src *Lease) (store.BuildRow, error) {
	release, err := s.snapshotAcquire(ctx, false)
	if err != nil {
		return store.BuildRow{}, err
	}
	defer release()
	start := time.Now()
	buildID, refs, err := s.sub.Checkpoint(ctx, src.SandboxID)
	pause := time.Since(start)
	if s.metrics != nil {
		s.metrics.CheckpointDur.Observe(pause.Seconds())
		s.metrics.CheckpointPause.Observe(pause.Seconds())
	}
	// The pause is what guests feel (2.3, #122): one line per checkpoint
	// with the lease, how long the guest was frozen and how much memory
	// had to be snapshotted.
	memMB, _ := s.imageMemoryMB(ctx, src.Image) // 0 when the catalog cannot say
	s.log.Printf("checkpoint: lease %s paused %.3fs (memory_mb %d, err %v)", src.ID, pause.Seconds(), memMB, err)
	if err != nil {
		return store.BuildRow{}, err
	}
	parent, err := s.db.GetBuild(ctx, src.BuildID)
	if err != nil {
		return store.BuildRow{}, fmt.Errorf("load parent build %s: %w", src.BuildID, err)
	}
	now := time.Now()
	b := store.BuildRow{
		BuildID:            buildID,
		Kind:               "checkpoint",
		TemplateID:         src.TemplateID,
		Image:              src.Image,
		ParentBuildID:      src.BuildID,
		SourceSandboxID:    src.SandboxID,
		Owner:              src.Owner,
		State:              "ready",
		KernelVersion:      parent.KernelVersion,
		FirecrackerVersion: parent.FirecrackerVersion,
		EnvdVersion:        parent.EnvdVersion,
		VCPU:               parent.VCPU,
		MemoryMB:           parent.MemoryMB,
		DiskMB:             parent.DiskMB,
		SizeBytes:          s.measureNewBuildOnDisk(buildID),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := s.db.InsertBuild(ctx, b); err != nil {
		return store.BuildRow{}, fmt.Errorf("insert checkpoint build: %w", err)
	}
	s.settleBuildSize(buildID)
	// The new build's headers reference the blocks of other builds
	// (A3 C3); GC keeps them (U11).
	if err := s.db.AddBuildRefs(ctx, buildID, append(refs.RootfsBuildIDs, refs.MemfileBuildIDs...)); err != nil {
		return store.BuildRow{}, fmt.Errorf("store checkpoint build refs: %w", err)
	}
	// The source keeps running from the new build (A2 §3.5, §3.6, the
	// "resume-fresh" path), so its build id — and possibly changed — host
	// IP are re-recorded (item 18). If the lease was released while the
	// checkpoint ran, afterCheckpoint leaves the build unreferenced and
	// reports it: the caller must not use the build as a new lease's
	// source (spoond-775).
	if !s.afterCheckpoint(ctx, src, buildID, start) {
		return b, errLeaseReleased
	}
	return b, nil
}

// afterCheckpoint records a checkpoint on the source lease: it now runs
// from the checkpoint build; its host IP is re-read from the substrate
// because the resume-fresh path may move it. start is when
// checkpointLease began, so the checkpointed event carries how long the
// checkpoint took (2.5, #132 part 2). Call without s.store.mu. It reports
// false when the lease was released while the checkpoint ran: nothing is
// saved and the build is left unreferenced (spoond-775).
func (s *Service) afterCheckpoint(ctx context.Context, src *Lease, buildID string, start time.Time) bool {
	sbs, err := s.sub.List(ctx)
	if err != nil {
		s.log.Printf("checkpoint: list sandboxes: %v", err)
	}
	s.store.mu.Lock()
	if src.released {
		// The lease was released while the checkpoint was in flight: the
		// build the checkpoint just wrote is left unreferenced for the GC
		// and neither the lease nor a sandboxes row is written back
		// (spoond-775). The checkpoint's resume-fresh may have started a
		// new sandbox under the same id; if the release's own delete
		// landed before that create, nothing is left to remove it and the
		// resumed guest would run on, holding hugepages until a backend
		// restart. Stop it here, detached from this request and with
		// bounded retries, and drop its row (spoond-d76).
		s.store.mu.Unlock()
		s.log.Printf("checkpoint: lease %s was released during its checkpoint; build %s left unreferenced", src.ID, buildID)
		// Stop the guest the checkpoint's resume-fresh may have started
		// under the source id. List's answer is not trusted: it fails with
		// the cancelled request context (the released check above runs
		// before the list error is meaningful) or may not yet show a
		// sandbox that started after it, and Delete is idempotent when
		// the id is already gone (spoond-15i).
		s.deleteSandboxBounded(ctx, src.SandboxID)
		s.deleteSandboxRow(src.SandboxID)
		return false
	}
	src.BuildID = buildID
	src.LastCheckpointBuildID = buildID
	src.LastCheckpointAt = time.Now()
	for _, sb := range sbs {
		if sb.ID == src.SandboxID {
			src.HostIP = sb.HostIP
			src.ExposedIP = sb.HostIP
			s.upsertSandboxRow(store.SandboxRow{
				SandboxID:   sb.ID,
				LeaseID:     src.ID,
				BuildID:     sb.BuildID,
				ExecutionID: sb.ExecutionID,
				HostIP:      sb.HostIP,
				VCPU:        int(sb.VCPU),
				MemoryMB:    int(sb.MemoryMB),
				StartedAt:   sb.StartedAt,
				EndAt:       sb.EndAt,
			})
		}
	}
	s.saveLeaseLocked(src)
	s.store.mu.Unlock()
	if len(src.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	s.emitLeaseEvent(src.ID, src.Owner, LeaseCheckpointed,
		fmt.Sprintf("%s · build %s", eventDuration(time.Since(start)), shortEventBuildID(buildID)))
	return true
}

// clone checkpoints a running sandbox into a new build and grants a new
// persistent lease on it. NetPolicy, NetAllow and ExposePorts are copied
// from the source; the image is the source's image name (the TemplateID
// is looked up by image name); the clone expires after maxTTL. The
// request's optional tag is accepted and ignored (A1 §17 item 5).
func (s *Service) clone(ctx context.Context, owner, srcID string) (*Lease, string, error) {
	s.store.mu.Lock()
	src := s.store.leases[srcID]
	if src == nil || src.Owner != owner || src.released {
		s.store.mu.Unlock()
		return nil, "", errNotFound
	}
	if err := lostErr(src); err != nil {
		s.store.mu.Unlock()
		return nil, "", err
	}
	if src.busy {
		s.store.mu.Unlock()
		return nil, "", errLeaseBusy
	}
	src.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(src)

	// The clone costs its image's memory_mb like any create (#128).
	memPer := s.imageMiB(src.Image)
	if err := s.reserveQuota(owner, 1, memPer, true); err != nil {
		return nil, "", err
	}
	defer func() { s.releaseQuotaReservation(owner, 1, memPer) }()

	img, _, err := s.imageBuild(ctx, src.Image)
	if err != nil {
		return nil, "", err
	}
	b, err := s.checkpointLease(ctx, src)
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	lease := &Lease{
		ID:          newID(),
		Owner:       owner,
		Image:       src.Image,
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.cfg.MaxTTL),
		Persistent:  true,
		LastActive:  now,
		NetPolicy:   src.NetPolicy,
		NetAllow:    append([]string(nil), src.NetAllow...),
		ExposePorts: append([]int(nil), src.ExposePorts...),
		// The clone continues the source's work (2.3, #122): its
		// checkpoint and idle policies continue too.
		CheckpointInterval: src.CheckpointInterval,
		IdleSuspend:        src.IdleSuspend,
		State:              "running",
		MemoryMB:           img.MemoryMB, // the admitted charge (#128)
		Generation:         1,            // every lease starts on generation 1 (2.2)
		TemplateID:         img.TemplateID,
	}
	// Class admission (#128 part 2): a clone takes its own class from
	// the owner's guarantee — a full one bursts the clone — and never
	// carries a request's burst flag or priority.
	class, err := s.admitClass(ctx, owner, img.MemoryMB, false, "")
	if err != nil {
		return nil, "", err
	}
	lease.Class = class
	// Hold the clone's egress memo in flight until it is in the store,
	// so a refresh mid-create does not prune it (spoond-ob18).
	s.beginAppliedEgress(lease.ID)
	defer s.endAppliedEgress(lease.ID)
	sb, err := s.createSandbox(ctx, img, b, false, "", lease)
	if err != nil {
		return nil, "", err
	}
	lease.SandboxID = sb.ID
	lease.HostIP = sb.HostIP
	lease.ExposedIP = sb.HostIP
	lease.BuildID = b.BuildID
	s.writeGeneration(lease)
	s.store.mu.Lock()
	// Final guard against a delete that raced this clone (spoond-q4j B1).
	// The clone reserved quota before the owner could be marked and then
	// spent seconds in checkpointLease, which never checks that its
	// source was released. deleteUserData sets the mark before its lease
	// re-scan and this commit holds the store lock across the check, so
	// either this guard sees the mark and refuses the ownerless lease, or
	// the commit lands first and the cleanup's re-scan releases it. Stop
	// the fresh sandbox here so no guest is left running.
	s.ownerDeleteMu.Lock()
	deleted := s.deletedOwners[owner]
	s.ownerDeleteMu.Unlock()
	if deleted {
		s.store.mu.Unlock()
		_ = s.sub.Delete(ctx, sb.ID)
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		// The sandbox is gone, so its applied-egress memo must not
		// linger for the life of the process (spoond-966); the deferred
		// endAppliedEgress clears the in-flight mark.
		s.forgetAppliedEgress(lease.ID)
		return nil, "", errOwnerGone
	}
	s.store.leases[lease.ID] = lease
	s.saveLeaseLocked(lease)
	s.store.mu.Unlock()
	s.endCreatingSandbox(sb.ID)
	if len(lease.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	s.emitLeaseEvent(lease.ID, owner, LeaseCreated, fmt.Sprintf("cloned from %s (build %s)", srcID, b.BuildID))
	return lease, b.BuildID, nil
}

// fork checkpoints a running sandbox once and creates count sandboxes
// from the checkpoint build (1..20). Quota for all count leases is
// reserved up front, all or nothing; if any create fails, every sandbox
// created in this call is deleted, the reservations are released, and
// the error is returned. The source keeps running (item 18 bookkeeping).
func (s *Service) fork(ctx context.Context, owner, srcID string, count int, persistent bool, ttl time.Duration, holder, holderURL string) ([]*Lease, string, error) {
	s.store.mu.Lock()
	src := s.store.leases[srcID]
	if src == nil || src.Owner != owner || src.released {
		s.store.mu.Unlock()
		return nil, "", errNotFound
	}
	if err := lostErr(src); err != nil {
		s.store.mu.Unlock()
		return nil, "", err
	}
	if src.Suspended {
		s.store.mu.Unlock()
		return nil, "", errSuspended
	}
	if src.busy {
		s.store.mu.Unlock()
		return nil, "", errLeaseBusy
	}
	src.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(src)

	if count < 1 || count > 20 {
		return nil, "", errBadForkCount
	}
	if ttl <= 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	if s.identities != nil {
		if u := s.identities.UserByID(owner); u != nil && u.MaxTTL > 0 {
			if userMax := time.Duration(u.MaxTTL) * time.Second; ttl > userMax {
				ttl = userMax
			}
		}
	}

	// Every fork child costs the image's memory_mb, count times (#128);
	// the whole batch is reserved up front, all or nothing.
	memPer := s.imageMiB(src.Image)
	if err := s.reserveQuota(owner, count, memPer, true); err != nil {
		return nil, "", err
	}

	img, _, err := s.imageBuild(ctx, src.Image)
	if err != nil {
		s.releaseQuotaReservation(owner, count, memPer)
		return nil, "", err
	}
	b, err := s.checkpointLease(ctx, src)
	if err != nil {
		s.releaseQuotaReservation(owner, count, memPer)
		return nil, "", err
	}

	var created []*Lease
	rollback := func(err error) ([]*Lease, string, error) {
		for _, l := range created {
			_ = s.sub.Delete(ctx, l.SandboxID)
			s.deleteSandboxRow(l.SandboxID)
			s.endCreatingSandbox(l.SandboxID)
			// The child's egress memo goes with its deleted sandbox, so a
			// rolled-back fork leaves none behind (spoond-966 follow-up).
			s.forgetAppliedEgress(l.ID)
			s.store.mu.Lock()
			delete(s.store.leases, l.ID)
			s.deleteLeaseLocked(l.ID)
			s.store.mu.Unlock()
		}
		s.releaseQuotaReservation(owner, count, memPer)
		return nil, "", err
	}

	now := time.Now()
	for range count {
		idleSuspend := src.IdleSuspend
		if !persistent {
			// A non-persistent fork cannot be idle-suspended (there is
			// no snapshot to resume), so it takes the host default
			// rather than the source's value (2.5, #129 part 2).
			idleSuspend = idleSuspendHost
		}
		lease := &Lease{
			ID:          newID(),
			Owner:       owner, // quota is charged to the caller
			Image:       src.Image,
			CreatedAt:   now,
			ExpiresAt:   now.Add(ttl),
			Persistent:  persistent,
			LastActive:  now,
			NetPolicy:   src.NetPolicy,
			NetAllow:    append([]string(nil), src.NetAllow...),
			ExposePorts: append([]int(nil), src.ExposePorts...),
			Holder:      holder,
			HolderUrl:   holderURL,
			// The forks continue the source's work (2.3, #122): its
			// checkpoint and idle policies continue too (a non-persistent
			// fork falls back to the host default idle policy above).
			CheckpointInterval: src.CheckpointInterval,
			IdleSuspend:        idleSuspend,
			State:              "running",
			MemoryMB:           img.MemoryMB, // the admitted charge (#128)
			Generation:         1,            // every lease starts on generation 1 (2.2)
			TemplateID:         img.TemplateID,
		}
		// Class admission (#128 part 2), per child: the whole batch was
		// reserved against max_mib above, but each child's class follows
		// the owner's charge as the children land. Every child of one
		// fork call gets the same class (the charge the class measures
		// moves only when a sandbox is created, one at a time below),
		// decided once here so the batch behaves as one.
		class, err := s.admitClass(ctx, owner, img.MemoryMB, false, "")
		if err != nil {
			return rollback(err)
		}
		lease.Class = class
		// Hold this child's egress memo in flight until it is in the store
		// (or rolled back), so a refresh mid-create does not prune it
		// (spoond-ob18).
		s.beginAppliedEgress(lease.ID)
		sb, err := s.createSandbox(ctx, img, b, false, "", lease)
		if err != nil {
			s.endAppliedEgress(lease.ID)
			return rollback(err)
		}
		lease.SandboxID = sb.ID
		lease.HostIP = sb.HostIP
		lease.ExposedIP = sb.HostIP
		lease.BuildID = b.BuildID
		s.writeGeneration(lease)
		s.store.mu.Lock()
		// Final guard against a delete that raced this fork (spoond-q4j
		// B1): each child commits under the store lock, so either the
		// mark is set and this child is refused (and every already
		// committed child is rolled back), or the commit lands before
		// the cleanup's re-scan and the cleanup releases it. Stop this
		// child's sandbox exactly once; rollback stops the earlier
		// ones.
		s.ownerDeleteMu.Lock()
		deleted := s.deletedOwners[owner]
		s.ownerDeleteMu.Unlock()
		if deleted {
			s.store.mu.Unlock()
			_ = s.sub.Delete(ctx, sb.ID)
			s.deleteSandboxRow(sb.ID)
			s.endCreatingSandbox(sb.ID)
			// The child's egress memo goes with its deleted sandbox, so
			// the refused fork leaves none behind (spoond-966).
			s.forgetAppliedEgress(lease.ID)
			s.endAppliedEgress(lease.ID)
			return rollback(errOwnerGone)
		}
		s.store.leases[lease.ID] = lease
		s.saveLeaseLocked(lease)
		s.store.mu.Unlock()
		s.endCreatingSandbox(sb.ID)
		s.endAppliedEgress(lease.ID)
		s.emitLeaseEvent(lease.ID, owner, LeaseCreated, fmt.Sprintf("forked from %s (build %s)", srcID, b.BuildID))
		created = append(created, lease)
	}
	s.releaseQuotaReservation(owner, count, memPer)
	if len(src.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	return created, b.BuildID, nil
}

// held reports whether the lease is held by something that must outlive
// the sweepers: a non-empty holder keeps it past its TTL and out of the
// idle sweep.
func (l *Lease) held() bool { return l.Holder != "" }

// Checkpoint interval bounds (2.3, #122): the create field and the
// policy PUT accept 0 (never) or 60..604800 seconds (a minute to a
// week). -1 on the stored lease alone means "the host default".
const (
	checkpointIntervalMin  = 60
	checkpointIntervalMax  = 604800
	checkpointIntervalHost = -1
)

// Idle-suspend bounds (2.5, #129 part 2). They happen to match the
// checkpoint interval's, but the names stay separate so the two fields
// can move independently. -1 on the stored lease means "the host
// default" (IdleSuspendDefault), exactly as for checkpoint_interval.
const (
	idleSuspendMin  = 60
	idleSuspendMax  = 604800
	idleSuspendHost = -1
)

// validateCheckpointInterval checks a requested checkpoint_interval:
// 0 (never) or 60..604800 seconds. The message names the field.
func validateCheckpointInterval(secs int64) error {
	if secs == 0 {
		return nil
	}
	if secs < checkpointIntervalMin || secs > checkpointIntervalMax {
		return fmt.Errorf("checkpoint_interval must be 0 (never) or %d..%d seconds",
			checkpointIntervalMin, checkpointIntervalMax)
	}
	return nil
}

// validateIdleSuspend checks a requested idle_suspend: 0 (never) or
// 60..604800 seconds. The message names the field.
func validateIdleSuspend(secs int64) error {
	if secs == 0 {
		return nil
	}
	if secs < idleSuspendMin || secs > idleSuspendMax {
		return fmt.Errorf("idle_suspend must be 0 (never) or %d..%d seconds",
			idleSuspendMin, idleSuspendMax)
	}
	return nil
}

// effectiveIdleSuspend resolves the lease's idle_suspend to seconds: its
// own value when set (0 = never, >0 seconds), otherwise the host default
// (IdleSuspendDefault; 0 = never). A non-persistent lease can never be
// idle-suspended — there is no snapshot to resume from — so its
// effective value is always 0 (never), whatever the host default is.
// This also keeps held rule 1 in force for non-persistent held leases
// (2.5, #129 part 2).
func (s *Service) effectiveIdleSuspend(l *Lease) int64 {
	if !l.Persistent {
		return 0
	}
	if l.IdleSuspend != idleSuspendHost {
		return l.IdleSuspend
	}
	return s.cfg.IdleSuspendDefault
}

// setIdlePolicy stores the lease's own idle reclamation threshold
// (2.5, #129 part 2) and emits an idle_policy event naming the new
// effective seconds. Call without s.store.mu.
func (s *Service) setIdlePolicy(l *Lease, secs int64) (*Lease, error) {
	s.store.mu.Lock()
	if err := lostErr(l); err != nil {
		s.store.mu.Unlock()
		return nil, err
	}
	l.IdleSuspend = secs
	s.saveLeaseLocked(l)
	effective := s.effectiveIdleSuspend(l)
	s.store.mu.Unlock()
	s.emitLeaseEvent(l.ID, l.Owner, LeaseIdlePolicy,
		fmt.Sprintf("idle_suspend %d", effective))
	return l, nil
}

// effectiveCheckpointInterval resolves the lease's checkpoint interval
// to seconds: its own value when set (0 = never, >0 seconds), otherwise
// the host default (CheckpointIntervalDefault; 0 = never).
func (s *Service) effectiveCheckpointInterval(l *Lease) int64 {
	if l.CheckpointInterval != checkpointIntervalHost {
		return l.CheckpointInterval
	}
	return s.cfg.CheckpointIntervalDefault
}

// setCheckpointPolicy stores the lease's own checkpoint interval
// (2.3, #122) and emits a checkpoint_policy event naming the new
// effective seconds. Call without s.store.mu.
func (s *Service) setCheckpointPolicy(l *Lease, secs int64) (*Lease, error) {
	s.store.mu.Lock()
	if err := lostErr(l); err != nil {
		s.store.mu.Unlock()
		return nil, err
	}
	l.CheckpointInterval = secs
	s.saveLeaseLocked(l)
	effective := s.effectiveCheckpointInterval(l)
	s.store.mu.Unlock()
	s.emitLeaseEvent(l.ID, l.Owner, LeaseCheckpointPolicy,
		fmt.Sprintf("checkpoint_interval %d", effective))
	return l, nil
}

// validateHolder checks the create/fork/holder fields: holder at most
// 128 printable characters; holderURL empty or an absolute http(s) URL
// of at most 512 characters. The message names the offending field.
func validateHolder(holder, holderURL string) error {
	if !utf8.ValidString(holder) {
		return fmt.Errorf("holder must be valid UTF-8")
	}
	if utf8.RuneCountInString(holder) > 128 {
		return fmt.Errorf("holder must be at most 128 characters")
	}
	for _, r := range holder {
		// Letters, marks, numbers, punctuation, symbols and the plain
		// space only: no control or format characters (zero-width
		// spaces, bidi overrides), which would mislead a reader of the
		// dashboard or a terminal.
		if r != ' ' && !unicode.IsGraphic(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("holder must be printable characters")
		}
	}
	if holderURL == "" {
		return nil
	}
	u, err := url.Parse(holderURL)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("holder_url must be an absolute http(s) URL")
	}
	if len(holderURL) > 512 {
		return fmt.Errorf("holder_url must be at most 512 characters")
	}
	return nil
}

// setHolder sets or clears a lease's holder fields. Both empty clears
// the holder and restores normal sweeping. The route admits the owner
// and admins; the hold clock lives in api/held.go (setHolder there).
func (s *Service) setHolder(owner, id, holder, holderURL string) (*Lease, error) {
	return s.setHolderWithTTL(owner, id, holder, holderURL, 0)
}

// setName assigns a friendly name to a lease. Names must be non-empty,
// at most 63 chars, [a-z0-9][a-z0-9-]* (lowercase, hyphenable, no dots —
// dots belong to the proxy hostname), and unique per owner.
func (s *Service) setName(owner, id, name string) (*Lease, error) {
	if name == "" || len(name) > 63 {
		return nil, fmt.Errorf("name must be 1-63 chars")
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0
		if !ok {
			return nil, fmt.Errorf("name must match [a-z0-9][a-z0-9-]*")
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		return nil, err
	}
	for _, other := range s.store.leases {
		if other != l && other.Owner == owner && !other.released && other.Name == name {
			return nil, fmt.Errorf("name %q already in use by lease %s", name, other.ID)
		}
	}
	l.Name = name
	s.saveLeaseLocked(l)
	return l, nil
}

// setComment sets or clears the free-text annotation on a lease.
// Empty string clears it; comments are informational only (no
// uniqueness constraint, unlike names).
func (s *Service) setComment(owner, id, comment string) (*Lease, error) {
	if len(comment) > 512 {
		return nil, fmt.Errorf("comment must be <= 512 chars")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		return nil, err
	}
	l.Comment = comment
	s.saveLeaseLocked(l)
	return l, nil
}

// setNetwork updates a lease's egress policy and allowlist (U09): the
// lease is saved, the egress config is re-applied to its sandbox, and
// every lease's peer allowances are refreshed asynchronously.
func (s *Service) setNetwork(ctx context.Context, owner, id, policy string, allow []string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		s.store.mu.Unlock()
		return nil, err
	}
	if l.Suspended {
		s.store.mu.Unlock()
		return nil, errSuspended
	}
	l.NetPolicy = policy
	l.NetAllow = allow
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()

	eg := s.egressFor(l)
	if err := s.sub.UpdateEgress(ctx, l.SandboxID, eg); err != nil {
		return nil, fmt.Errorf("update egress: %w", err)
	}
	// The UpdateEgress ran without the store lock; a release can have
	// landed meanwhile. Do not re-add the memo for a lease that is gone:
	// that would resurrect the entry the release cleared (spoond-966
	// follow-up). The lease itself is still returned (the caller set the
	// policy before the release), but a released lease gets no memo. A
	// suspended lease still gets one: its memo is re-checked when it
	// resumes, and present (not live) is the bookkeeping guard
	// (spoond-ob18).
	if s.lookupPresent(l.ID) != nil {
		s.recordAppliedEgress(l.ID, eg)
	}
	s.refreshPeersAsync(ctx)
	return l, nil
}

// lookupByName returns a live lease with the given name regardless of
// owner. Used by the SSH gateway (username = name) and the public proxy
// (<name>.<proxy suffix>); both treat the name as the capability, the
// same model as lease ids. Names are unique per owner.
func (s *Service) lookupByName(name string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if !l.released && l.Name == name {
			return l
		}
	}
	return nil
}

// lookupByNameForOwner returns a live lease with the given name owned by
// the given owner (names are unique per owner). Used by the API's
// /api/names endpoint so a caller can only resolve their own names.
func (s *Service) lookupByNameForOwner(owner, name string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if !l.released && l.Owner == owner && l.Name == name {
			return l
		}
	}
	return nil
}

// lookupUserScoped resolves a proxy hostname label (hex lease id or
// friendly name) to a lease owned by the given user. Used by the HTTP
// proxy under forward-auth (U7/T7): the proxy never resolves another
// owner's leases by id or name.
func (s *Service) lookupUserScoped(owner, label string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if l.released || l.Owner != owner {
			continue
		}
		if l.ID == label || l.Name == label {
			return l
		}
	}
	return nil
}

// GrantShare shares a lease with another user (T6/#33). Only the owner
// can grant; an existing share is replaced. mode selects the surface
// (ssh | http); ttl limits the share lifetime (0 = never expires).
func (s *Service) GrantShare(owner, leaseID, grantee string, mode ShareMode, ttl time.Duration) error {
	l := s.lookup(owner, leaseID)
	if l == nil {
		return fmt.Errorf("lease not found")
	}
	if err := lostErr(l); err != nil {
		return err
	}
	if grantee == "" || grantee == owner {
		return fmt.Errorf("grantee must be a different user")
	}
	if s.identities != nil && s.identities.UserByID(grantee) == nil {
		return fmt.Errorf("no such user: %s", grantee)
	}
	if mode != ShareSSH && mode != ShareHTTP {
		return fmt.Errorf("share mode must be ssh or http")
	}
	sh := &Share{
		LeaseID:   leaseID,
		Grantee:   grantee,
		Mode:      mode,
		CreatedAt: time.Now(),
	}
	if ttl > 0 {
		sh.ExpiresAt = time.Now().Add(ttl)
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.shares[leaseID] == nil {
		s.store.shares[leaseID] = make(map[string]*Share)
	}
	s.store.shares[leaseID][grantee] = sh
	s.saveShareLocked(sh)
	return nil
}

// RevokeShare removes a grant (T6/#33). Only the owner can revoke.
// Idempotent.
func (s *Service) RevokeShare(owner, leaseID, grantee string) error {
	l := s.lookup(owner, leaseID)
	if l == nil {
		return fmt.Errorf("lease not found")
	}
	if err := lostErr(l); err != nil {
		return err
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if m := s.store.shares[leaseID]; m != nil {
		delete(m, grantee)
		if len(m) == 0 {
			delete(s.store.shares, leaseID)
		}
	}
	s.deleteShareLocked(leaseID, grantee)
	return nil
}

// ListShares returns shares granted on the caller's leases (T6/#33),
// newest first.
func (s *Service) ListShares(owner string) []*Share {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []*Share
	for _, m := range s.store.shares {
		for _, sh := range m {
			l := s.store.leases[sh.LeaseID]
			if l == nil || l.released || l.Owner != owner {
				continue
			}
			cp := *sh
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// lookupWithShare returns a live lease the caller may access, either as
// owner or via a valid share covering mode. Used by exec/proxy/endpoint
// so shared leases work without weakening owner scoping.
func (s *Service) lookupWithShare(caller, leaseID string, mode ShareMode) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[leaseID]
	if l == nil || l.released {
		return nil
	}
	if l.Owner == caller {
		return l
	}
	sh := s.store.shares[leaseID][caller]
	if sh == nil || sh.Mode != mode {
		return nil
	}
	if !sh.ExpiresAt.IsZero() && time.Now().After(sh.ExpiresAt) {
		return nil
	}
	return l
}

// lookup returns the lease with the given id if it belongs to owner.
func (s *Service) lookup(owner, id string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil
	}
	return l
}

// lookupPresent returns a lease by id when it is still in the store and
// unreleased, regardless of whether its sandbox is live. Paths that
// guard per-lease bookkeeping (the egress memo, deferred secret
// removals) only need presence: release clears that bookkeeping under
// the same lock that marks the lease released, so a lease that is gone
// can never have stale state re-added, and a suspended lease still owns
// the bookkeeping that resumes with it (spoond-ob18).
func (s *Service) lookupPresent(id string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.released {
		return nil
	}
	return l
}

// lookupAny returns a live lease by id regardless of owner. Used by the
// public HTTP proxy where the lease id in the hostname is the capability
// (same model as the SSH gateway).
func (s *Service) lookupAny(id string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.released {
		return nil
	}
	return l
}

// leaseSuspendReason returns a live lease's structured suspension reason
// ("" for a hand or drain suspend, or an unknown/released lease) with
// the store lock held. The 409 lease_suspended writers call it instead
// of reading Lease.SuspendReason after a lock-free lookup, so a
// concurrent resume that clears the field under the lock never races
// (#145 D6).
func (s *Service) leaseSuspendReason(id string) string {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.released {
		return ""
	}
	return l.SuspendReason
}

// holdState is "active" while a hold runs until hold_expires_at,
// "lapsed" once it ran out unrenewed (the lease was suspended and is
// released by the stale rule unless renewed or used), and "" for an
// unheld lease.
func holdState(l *Lease) string {
	if !l.held() {
		return ""
	}
	if l.HoldExpiresAt.IsZero() {
		return "lapsed"
	}
	return "active"
}

// leaseMap renders a lease as one GET /api/sandboxes row. The
// checkpointInterval and idleSuspend arguments are the lease's effective
// values in seconds (the host default already resolved; 0 = never).
func leaseMap(l *Lease, checkpointInterval, idleSuspend int64) map[string]any {
	m := map[string]any{
		"id":               l.ID,
		"owner":            l.Owner,
		"image":            l.Image,
		"address":          l.HostIP,
		"expires":          l.ExpiresAt.Unix(),
		"persistent":       l.Persistent,
		"suspended":        l.Suspended,
		"state":            l.State,
		"build_id":         l.BuildID,
		"resume_build_id":  l.ResumeBuildID,
		"name":             l.Name,
		"comment":          l.Comment,
		"holder":           l.Holder,
		"holder_url":       l.HolderUrl,
		"net_policy":       l.NetPolicy,
		"egress_allowlist": l.NetAllow,
		"exposed":          exposedMap(l),
		// Continuity generation (2.2): bumped when the guest's memory
		// does not continue from where its processes left it.
		"generation": l.Generation,
		// Effective per-lease checkpoint interval in seconds (2.3,
		// #122): the host default resolved; 0 = never.
		"checkpoint_interval": checkpointInterval,
		// Effective per-lease idle reclamation threshold in seconds
		// (2.5, #129 part 2): the host default resolved; 0 = never.
		"idle_suspend": idleSuspend,
		// Admission class and scheduling priority (#128 part 2):
		// guaranteed within the owner's guaranteed_mib, burst above it
		// (preemptible, held to the node's reserve); a lower priority
		// is preempted first.
		"class":    leaseClassRow(l),
		"priority": l.Priority,
		// Preemption (#128 part 3): true when this burst lease was
		// suspended to make room for a guaranteed lease; the resume
		// queue brings it back when capacity allows.
		"preempted": !l.PreemptedAt.IsZero(),
	}
	if !l.HoldExpiresAt.IsZero() {
		m["hold_expires_at"] = l.HoldExpiresAt.UTC().Format(time.RFC3339)
	}
	if st := holdState(l); st != "" {
		m["hold_state"] = st
	}
	if l.LastAction != "" {
		m["last_action"] = l.LastAction
		m["last_action_at"] = formatRFC3339(l.LastActionAt)
	}
	// Structured suspension facts (#145 D6), additive and omitted while
	// the lease is not suspended: why an automatic suspend happened
	// (idle|idle_suspend|hold_lapsed|pressure|preempt), the pressure
	// order's step ("" until it names steps), the pause build and when.
	// A hand or drain suspend carries none of them and the fields stay
	// off.
	if l.SuspendReason != "" {
		m["suspend_reason"] = l.SuspendReason
	}
	if l.SuspendPolicyStep != "" {
		m["suspend_policy_step"] = l.SuspendPolicyStep
	}
	if l.SuspendBuildID != "" {
		m["suspend_build_id"] = l.SuspendBuildID
	}
	if !l.SuspendedAt.IsZero() {
		m["suspended_at"] = formatRFC3339(l.SuspendedAt)
	}
	return m
}

// leaseDetailMap is a list row plus the lifecycle fields served by
// GET /api/sandboxes/{id}. The lease's kept checkpoints (#126) ride
// along: build_id, size_bytes and kept_at each, oldest keep first —
// what the caller may restore, and what unpins would free. The owner's
// memory-quota view rides along too (#128): charged_mib, guaranteed_mib
// and max_mib, the same numbers as GET /api/users/me scoped to this
// lease's owner.
func (s *Service) leaseDetailMap(l *Lease) map[string]any {
	m := leaseMap(l, s.effectiveCheckpointInterval(l), s.effectiveIdleSuspend(l))
	m["state"] = l.State
	m["recovered_from"] = formatRFC3339(l.RecoveredFrom)
	m["last_checkpoint_at"] = formatRFC3339(l.LastCheckpointAt)
	if !l.LostAt.IsZero() {
		m["lost_at"] = formatRFC3339(l.LostAt)
	}
	// The reason the lease was lost, so a GET names the cause beside the
	// state (the same text every 410 lease_lost response carries).
	if l.State == "lost" && l.LostReason != "" {
		m["lost_reason"] = l.LostReason
	}
	// A pending crash-recovery retry (spoond-dxq): the owner sees the
	// attempt, the limit and since when, rather than a silent wait for the
	// next reconcile pass.
	if attempt, of, since, ok := s.recoveryStatus(l); ok {
		m["recovery"] = map[string]any{
			"attempt": attempt,
			"of":      of,
			"since":   formatRFC3339(since),
		}
	}
	kept := []map[string]any{}
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if rows, err := s.db.ListKeptBuildRows(ctx, l.ID); err != nil {
		s.log.Printf("lease detail: kept builds of %s: %v", l.ID, err)
	} else {
		for _, r := range rows {
			kept = append(kept, map[string]any{
				"build_id":   r.BuildID,
				"size_bytes": r.SizeBytes,
				"kept_at":    formatRFC3339(r.KeptAt),
			})
		}
	}
	m["kept_builds"] = kept
	// Pause-chain size (spoond-p9j): how many builds the lease's chain
	// holds and their summed recorded size_bytes. A persistent lease that
	// suspends daily keeps every pause build (GC keeps the chain's
	// ancestors) until a cold restart, so this shows what such a chain
	// costs before any compaction decision. Best effort: a catalog hiccup
	// leaves the fields off rather than failing the read.
	if depth, bytes, err := s.leaseChainStats(ctx, l); err != nil {
		s.log.Printf("lease detail: chain stats of %s: %v", l.ID, err)
	} else if depth > 0 {
		m["chain_depth"] = depth
		m["chain_bytes"] = bytes
	}
	// The named-snapshot version this lease started from (A3): the same
	// object the create response carries.
	if view := s.snapshotView(ctx, l); view != nil {
		m["snapshot"] = view
	}
	// Background jobs (2.6, #135): how many of the lease's jobs run and
	// its most recent exit, for the lease view.
	m["jobs"] = s.leaseJobsView(ctx, l.ID)
	// Memory quota of the lease's owner (#128): the running-lease charge
	// plus the user's limits, 0 = unset. Best effort: an identity-store
	// hiccup leaves the fields off rather than failing the read.
	if s.identities != nil {
		if u := s.identities.UserByID(l.Owner); u != nil {
			m["charged_mib"] = s.usedMiB(u.ID)
			m["guaranteed_mib"] = u.GuaranteedMiB
			m["max_mib"] = u.MaxMiB
		}
	}
	return m
}

// formatRFC3339 renders t in UTC, "" for the zero time.
func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// list returns the caller's live leases as plain maps.
func (s *Service) list(owner string) []map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	// One pass over the job tables for every lease, rather than two
	// queries per row.
	running, err := s.db.CountRunningJobsByLease(ctx)
	if err != nil {
		s.storeError("count_running_jobs", owner, err)
	}
	exits, err := s.db.LatestJobExit(ctx)
	if err != nil {
		s.storeError("latest_job_exits", owner, err)
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []map[string]any
	for _, l := range s.store.leases {
		if l.Owner == owner && !l.released {
			m := leaseMap(l, s.effectiveCheckpointInterval(l), s.effectiveIdleSuspend(l))
			m["jobs"] = jobsSummaryView(running[l.ID], exits[l.ID])
			out = append(out, m)
		}
	}
	return out
}

// jobsSummaryView renders the lease view's jobs field from an already
// fetched running count and latest exited row.
func jobsSummaryView(running int, last store.JobRow) map[string]any {
	view := map[string]any{"running": running, "last_exit": nil}
	if last.JobID == "" {
		return view
	}
	exit := 0
	if last.ExitCode != nil {
		exit = *last.ExitCode
	}
	view["last_exit"] = map[string]any{
		"job_id":    last.JobID,
		"exit_code": exit,
		"ended_at":  formatRFC3339(last.EndedAt),
	}
	return view
}

// integrityProbe is run inside a sandbox before it is pooled or leased. It
// checks what the toolchain files DO, not whether they execute: a corrupted
// /usr/bin/uname can be a valid ELF of plausible size that exits 0 while
// printing some other program's name, so an exit-status check passes it. A
// name-based check is no better — it has to guess at non-GNU tools (busybox
// uname announces itself as BusyBox). Behaviour is the thing that stays true
// across images.
//
// The carriers here are the ones observed in practice: a swapped or
// unexecutable uname (which breaks every `OS=$(uname -s)` build hook) and a
// swapped tr. Extend the script if a new carrier shows up; a probe that does
// not answer at all counts as a failure, since the guest is the only vantage
// point from which this corruption is visible.
const integrityProbe = `p=""
if command -v uname >/dev/null 2>&1; then
  v=$(uname -s 2>&1)
  case "$v" in Linux|linux) ;; *) p="uname -s -> $v" ;; esac
fi
if [ -z "$p" ] && command -v tr >/dev/null 2>&1; then
  v=$(echo a-b | tr - _ 2>&1)
  case "$v" in a_b) ;; *) p="tr a-b - _ -> $v" ;; esac
fi
if [ -z "$p" ]; then echo PROBE_OK; else echo "PROBE_FAIL $p"; exit 1; fi
`

// probeSandbox runs integrityProbe inside a sandbox. nil means usable. An
// unreachable guest agent is a failure rather than a pass: a probe that
// cannot run has told us nothing about the sandbox.
func (s *Service) probeSandbox(ctx context.Context, id string) error {
	if !s.probeEnabled {
		return nil
	}
	res, err := s.sub.Exec(ctx, id, substrate.ExecRequest{Args: []string{"sh", "-c", integrityProbe}, Timeout: s.probeTimeout})
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	out := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || out != "PROBE_OK" {
		if out == "" {
			out = strings.TrimSpace(res.Stderr)
		}
		return fmt.Errorf("integrity probe failed (exit %d): %s", res.ExitCode, out)
	}
	return nil
}

// warmPool pre-creates cfg.PoolSize sandboxes of img under the placeholder
// lease {ID:"pool", Owner:"pool", ExpiresAt: now+24h, NetPolicy:"none"}.
// Their sandboxes rows carry lease_id "".
func (s *Service) warmPool(ctx context.Context, img store.ImageRow) {
	if s.cfg.PoolSize <= 0 {
		return
	}
	s.store.mu.Lock()
	cur := len(s.store.pool[img.Name])
	s.store.mu.Unlock()
	if cur >= s.cfg.PoolSize {
		return
	}
	b, err := s.db.GetBuild(ctx, img.CurrentBuildID)
	if err != nil {
		s.log.Printf("warmPool: %s: load build %s: %v", img.Name, img.CurrentBuildID, err)
		return
	}
	placeholder := &Lease{
		ID:         "pool",
		Owner:      "pool",
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		NetPolicy:  string(PolicyNone),
		State:      "running",
		Generation: 1, // the grant overwrites the file with the lease's own
		TemplateID: img.TemplateID,
	}
	// Create one at a time and verify before stocking, so a bad build is
	// recycled here rather than served to a job. Stop rather than loop: a
	// build that fails the probe will keep failing it, and retrying
	// creates a sandbox per attempt.
	for i := cur; i < s.cfg.PoolSize; i++ {
		sb, err := s.createSandbox(ctx, img, b, false, "", placeholder)
		if err != nil {
			s.log.Printf("warmPool: create %s: %v", img.Name, err)
			return
		}
		if err := s.probeSandbox(ctx, sb.ID); err != nil {
			s.log.Printf("warmPool: %s sandbox %s failed the integrity probe, recycling: %v", img.Name, sb.ID, err)
			_ = s.sub.Delete(ctx, sb.ID)
			s.deleteSandboxRow(sb.ID)
			s.endCreatingSandbox(sb.ID)
			return
		}
		s.store.mu.Lock()
		s.store.pool[img.Name] = append(s.store.pool[img.Name], sb.ID)
		s.addPoolLocked(sb.ID, img.Name)
		s.store.mu.Unlock()
		s.endCreatingSandbox(sb.ID)
	}
}

// leaseToRow maps an in-memory lease to its store row. An empty State
// is derived from Suspended so the row always carries a valid state.
// HostIP lives in the address column; TemplateID is looked up from
// images and BuildID from sandboxes (neither is a lease column).
func leaseToRow(l *Lease) store.LeaseRow {
	state := l.State
	if state == "" {
		if l.Suspended {
			state = "suspended"
		} else {
			state = "running"
		}
	}
	return store.LeaseRow{
		ID:                    l.ID,
		Owner:                 l.Owner,
		Image:                 l.Image,
		SandboxID:             l.SandboxID,
		Address:               l.HostIP,
		CreatedAt:             l.CreatedAt,
		ExpiresAt:             l.ExpiresAt,
		Persistent:            l.Persistent,
		LastActive:            l.LastActive,
		Suspended:             l.Suspended,
		Name:                  l.Name,
		NetPolicy:             l.NetPolicy,
		NetAllow:              l.NetAllow,
		ExposePorts:           l.ExposePorts,
		ExposedIP:             l.ExposedIP,
		Comment:               l.Comment,
		State:                 state,
		ResumeBuildID:         l.ResumeBuildID,
		LastCheckpointBuildID: l.LastCheckpointBuildID,
		LastCheckpointAt:      l.LastCheckpointAt,
		RecoveredFrom:         l.RecoveredFrom,
		LostAt:                l.LostAt,
		LostReason:            l.LostReason,
		Drained:               l.Drained,
		Holder:                l.Holder,
		HolderUrl:             l.HolderUrl,
		HoldSetAt:             l.HoldSetAt,
		HoldExpiresAt:         l.HoldExpiresAt,
		HoldTTL:               int64(l.HoldTTL / time.Second),
		LastAction:            l.LastAction,
		LastActionAt:          l.LastActionAt,
		Generation:            l.Generation,
		CheckpointInterval:    l.CheckpointInterval,
		IdleSuspend:           l.IdleSuspend,
		MemoryMB:              l.MemoryMB,
		Class:                 leaseClassRow(l),
		Priority:              l.Priority,
		PreemptedAt:           l.PreemptedAt,
		SnapshotBuildID:       l.SnapshotBuildID,
		SuspendReason:         l.SuspendReason,
		SuspendPolicyStep:     l.SuspendPolicyStep,
		SuspendBuildID:        l.SuspendBuildID,
		SuspendedAt:           l.SuspendedAt,
	}
}

// leaseClassRow fills the row's class: an unstamped lease (from before
// #128 part 2) reads as guaranteed — the class every lease had before
// classes existed.
func leaseClassRow(l *Lease) string {
	if l.Class == "" {
		return ClassGuaranteed
	}
	return l.Class
}

// rowToLease maps a store row back to an in-memory lease. The Suspended
// flag follows the persisted state.
func rowToLease(r store.LeaseRow) *Lease {
	return &Lease{
		ID:                    r.ID,
		Owner:                 r.Owner,
		Image:                 r.Image,
		SandboxID:             r.SandboxID,
		HostIP:                r.Address,
		CreatedAt:             r.CreatedAt,
		ExpiresAt:             r.ExpiresAt,
		Persistent:            r.Persistent,
		LastActive:            r.LastActive,
		Suspended:             r.State == "suspended",
		Name:                  r.Name,
		NetPolicy:             r.NetPolicy,
		NetAllow:              r.NetAllow,
		ExposePorts:           r.ExposePorts,
		ExposedIP:             r.ExposedIP,
		Comment:               r.Comment,
		State:                 r.State,
		ResumeBuildID:         r.ResumeBuildID,
		LastCheckpointBuildID: r.LastCheckpointBuildID,
		LastCheckpointAt:      r.LastCheckpointAt,
		RecoveredFrom:         r.RecoveredFrom,
		LostAt:                r.LostAt,
		LostReason:            r.LostReason,
		Drained:               r.Drained,
		Holder:                r.Holder,
		HolderUrl:             r.HolderUrl,
		HoldSetAt:             r.HoldSetAt,
		HoldExpiresAt:         r.HoldExpiresAt,
		HoldTTL:               time.Duration(r.HoldTTL) * time.Second,
		LastAction:            r.LastAction,
		LastActionAt:          r.LastActionAt,
		Generation:            r.Generation,
		CheckpointInterval:    r.CheckpointInterval,
		IdleSuspend:           r.IdleSuspend,
		MemoryMB:              r.MemoryMB,
		Class:                 r.Class,
		Priority:              r.Priority,
		PreemptedAt:           r.PreemptedAt,
		SnapshotBuildID:       r.SnapshotBuildID,
		SuspendReason:         r.SuspendReason,
		SuspendPolicyStep:     r.SuspendPolicyStep,
		SuspendBuildID:        r.SuspendBuildID,
		SuspendedAt:           r.SuspendedAt,
	}
}

// storeWriteTimeout bounds a single store write.
const storeWriteTimeout = 5 * time.Second

// storeError records a failed store write: a log line plus the
// spoond_store_errors_total counter. Store errors never fail the
// request — availability wins over durability for a single write.
func (s *Service) storeError(op, id string, err error) {
	s.log.Printf("store: %s %s: %v", op, id, err)
	if s.metrics != nil {
		s.metrics.StoreErrors.WithLabelValues(op).Inc()
	}
}

// The store helpers below are called with s.store.mu held (the sandbox
// row helpers excepted: they run outside lease mutations) and write
// through with a bounded timeout.

// endBusy clears a lease's busy flag. It is the deferred counterpart of
// the busy set every lifecycle operation performs under s.store.mu
// before its first sub call (U10).
func (s *Service) endBusy(l *Lease) {
	s.store.mu.Lock()
	l.busy = false
	s.store.mu.Unlock()
}

// trySetBusy marks a lease busy unless it is already busy, released or
// no longer running. It returns false when another operation won the
// race, in which case the caller must not run its own. The caller owns
// the busy window and must pair a true result with endBusy.
func (s *Service) trySetBusy(l *Lease) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if l.busy || l.released || !l.live() {
		return false
	}
	l.busy = true
	return true
}

func (s *Service) saveLeaseLocked(l *Lease) {
	// A lease released while an asynchronous operation (checkpoint, pause,
	// resume, restore, recovery, reconcile) was in flight must never be
	// written back: the release removed the in-memory entry and the lease
	// row, and a late save would resurrect it as a phantom lease that
	// holds the owner's quota until the lost-lease grace lapses
	// (spoond-775). The operation's own cleanup is the caller's job; this
	// guard is the last line of defence for every path.
	if l.released {
		s.log.Printf("store: upsert_lease %s: dropped, lease was released", l.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.UpsertLease(ctx, leaseToRow(l)); err != nil {
		s.storeError("upsert_lease", l.ID, err)
	}
}

// leaseReleased reports whether l was released, under the store lock.
// A nil lease is not released.
func (s *Service) leaseReleased(l *Lease) bool {
	if l == nil {
		return false
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return l.released
}

// The guest's generation file (2.2): /run/spoond/generation, one line
// with the lease's current generation. Processes in the guest read it to
// notice that the memory they run in did not continue from where they
// left it (crash recovery, restart) and re-derive whatever they keep in
// process. The file is written 0644; WriteFile creates missing parents
// (/run/spoond) 0755. Best effort: a failure is logged and otherwise
// ignored — the file is an announcement, not a constraint.
const (
	generationPath = "/run/spoond/generation"
	generationMode = 0o644

	// leaseIDPath is the guest file carrying the current lease id (2.7,
	// #83). Processes captured in a memory snapshot keep the source's
	// SPOOND_LEASE_ID; a process that needs the current one reads this
	// file, which every path that (re)creates a guest rewrites.
	leaseIDPath = "/run/spoond/lease-id"
	leaseIDMode = 0o644

	// lastSavePath is the source-side marker of a named snapshot save
	// (2.7, #83 A4): JSON {name, version, build_id, idempotency_key},
	// written after the checkpoint completes and before the save
	// answers. A copy's memory was captured before this write, so a copy
	// never sees this save's marker.
	lastSavePath = "/run/spoond/last-save"
	// startedFromPath is the copy-side marker of a lease started from a
	// named snapshot (2.7, #83 A4): JSON {name, version, build_id}.
	startedFromPath = "/run/spoond/started-from"
)

// writeGeneration writes the lease's current generation into the guest
// at /run/spoond/generation (0644, parent /run/spoond 0755) and the
// lease id at /run/spoond/lease-id (0644). Best effort: failures are
// logged, never returned. Both files are written atomically (temp file,
// then rename), so inotify on /run/spoond sees each whole write. Call
// without s.store.mu.
func (s *Service) writeGeneration(l *Lease) error {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	data := fmt.Sprintf("%d\n", l.Generation)
	if err := s.writeGuestFileAtomic(ctx, l, generationPath, []byte(data), generationMode); err != nil {
		return err
	}
	return s.writeGuestFileAtomic(ctx, l, leaseIDPath, []byte(l.ID+"\n"), leaseIDMode)
}

// writeGuestFileAtomic writes data to path in the lease's guest through
// a sibling temp file plus a rename, so a reader (an inotify watcher
// included) never sees a partially written file. It returns an error
// when the write or the rename fails; callers that treat the file as an
// announcement ignore it (and it is logged here), while a snapshot
// start's copy-side markers must fail the create (A4/S3). Call without
// s.store.mu.
func (s *Service) writeGuestFileAtomic(ctx context.Context, l *Lease, path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := s.sub.WriteFile(ctx, l.SandboxID, tmp, data, mode); err != nil {
		s.log.Printf("guest file: write %s into %s: %v", tmp, l.ID, err)
		return err
	}
	if err := s.sub.Rename(ctx, l.SandboxID, tmp, path); err != nil {
		s.log.Printf("guest file: rename %s -> %s in %s: %v", tmp, path, l.ID, err)
		return err
	}
	return nil
}

// bumpGenerationLocked moves the lease to the next generation and
// persists it. Bumped on every path that puts the lease into a state its
// processes did not continue from: crash recovery and restart. A planned
// pause/resume and the admin drain/undrain continue the memory and do
// not bump. Call with s.store.mu held; the guest file is rewritten by
// the caller after the sandbox exists again.
func (s *Service) bumpGenerationLocked(l *Lease) {
	l.Generation++
	s.saveLeaseLocked(l)
}

func (s *Service) deleteLeaseLocked(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.DeleteLease(ctx, id); err != nil {
		s.storeError("delete_lease", id, err)
	}
}

func (s *Service) saveShareLocked(sh *Share) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.UpsertShare(ctx, store.ShareRow{
		LeaseID:   sh.LeaseID,
		Grantee:   sh.Grantee,
		Mode:      string(sh.Mode),
		ExpiresAt: sh.ExpiresAt,
		CreatedAt: sh.CreatedAt,
	}); err != nil {
		s.storeError("upsert_share", sh.LeaseID+"/"+sh.Grantee, err)
	}
}

func (s *Service) deleteShareLocked(leaseID, grantee string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.DeleteShare(ctx, leaseID, grantee); err != nil {
		s.storeError("delete_share", leaseID+"/"+grantee, err)
	}
}

func (s *Service) addPoolLocked(id, image string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.AddPool(ctx, id, image); err != nil {
		s.storeError("add_pool", id, err)
	}
}

func (s *Service) removePoolLocked(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.RemovePool(ctx, id); err != nil {
		s.storeError("remove_pool", id, err)
	}
}

// upsertSandboxRow records a created/resumed/checkpointed sandbox.
func (s *Service) upsertSandboxRow(row store.SandboxRow) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.UpsertSandbox(ctx, row); err != nil {
		s.storeError("upsert_sandbox", row.SandboxID, err)
	}
}

// deleteSandboxRow drops a sandboxes entry.
func (s *Service) deleteSandboxRow(sandboxID string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.DeleteSandbox(ctx, sandboxID); err != nil {
		s.storeError("delete_sandbox", sandboxID, err)
	}
}

// sandboxDeleteRetries is how many times a detached cleanup delete is
// attempted before it is given up to the next orphan sweep.
const sandboxDeleteRetries = 3

// sandboxDeleteBackoff is the pause between those attempts.
const sandboxDeleteBackoff = 500 * time.Millisecond

// deleteSandboxBounded stops a sandbox a late operation created, on a
// context detached from the request that none of its callers can cancel
// and with bounded retries, so a transient substrate error does not leak
// the guest. A delete that keeps failing is logged and remembered as an
// orphan, so the periodic orphan sandbox sweep retries it (spoond-63a).
func (s *Service) deleteSandboxBounded(ctx context.Context, sandboxID string) {
	dctx := context.WithoutCancel(ctx)
	var err error
	for attempt := 1; attempt <= sandboxDeleteRetries; attempt++ {
		if err = s.sub.Delete(dctx, sandboxID); err == nil {
			return
		}
		if attempt == sandboxDeleteRetries {
			break
		}
		select {
		case <-dctx.Done():
			return
		case <-time.After(sandboxDeleteBackoff):
		}
	}
	s.log.Printf("delete sandbox %s failed after %d attempt(s): %v", sandboxID, sandboxDeleteRetries, err)
	s.rememberOrphanSandbox(sandboxID)
}

// flushLastActiveLocked writes the batched touch() updates in one
// transaction, then clears the dirty set. A failed flush keeps the set
// so the next tick retries. Called with s.store.mu held.
func (s *Service) flushLastActiveLocked(ctx context.Context) {
	if len(s.store.lastActiveDirty) == 0 {
		return
	}
	dirty := s.store.lastActiveDirty
	ctx, cancel := context.WithTimeout(ctx, storeWriteTimeout)
	defer cancel()
	if err := s.db.UpdateLastActive(ctx, dirty); err != nil {
		ids := make([]string, 0, len(dirty))
		for id := range dirty {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		s.storeError("update_last_active", strings.Join(ids, ","), err)
		return
	}
	s.store.lastActiveDirty = make(map[string]time.Time)
}

// LoadState reads every lease, share and pool entry from the store into
// memory. Each lease's TemplateID comes from its image row; its BuildID
// comes from the sandboxes row carrying its id (a suspended lease has
// none, and its BuildID stays ""). It must run before Start and before
// ReconcileOrphans.
func (s *Service) LoadState(ctx context.Context) error {
	leases, err := s.db.ListLeases(ctx)
	if err != nil {
		return fmt.Errorf("load leases: %w", err)
	}
	shareRows, err := s.db.ListShares(ctx)
	if err != nil {
		return fmt.Errorf("load shares: %w", err)
	}
	pool, err := s.db.ListPool(ctx)
	if err != nil {
		return fmt.Errorf("load pool: %w", err)
	}
	loaded := make(map[string]*Lease, len(leases))
	for _, r := range leases {
		l := rowToLease(r)
		if img, err := s.db.GetImage(ctx, l.Image); err == nil {
			l.TemplateID = img.TemplateID
		}
		if row, err := s.db.GetSandboxByLease(ctx, l.ID); err == nil {
			l.BuildID = row.BuildID
		}
		loaded[l.ID] = l
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.store.leases = loaded
	for _, r := range shareRows {
		if s.store.shares[r.LeaseID] == nil {
			s.store.shares[r.LeaseID] = make(map[string]*Share)
		}
		s.store.shares[r.LeaseID][r.Grantee] = &Share{
			LeaseID:   r.LeaseID,
			Grantee:   r.Grantee,
			Mode:      ShareMode(r.Mode),
			ExpiresAt: r.ExpiresAt,
			CreatedAt: r.CreatedAt,
		}
	}
	for img, ids := range pool {
		s.store.pool[img] = ids
	}
	// Running background jobs (2.6, #135): seed the in-memory counts the
	// idle sweeps and the running gauge read, so a job that ended while
	// the backend was down is noticed by the reconcile pass.
	s.loadRunningJobsLocked(ctx, loaded)
	// A backend that stopped mid-save wrote no version: a checkpoint
	// build still "building" with no named row is marked failed (A7), so
	// the orchestrator's later completion is an ordinary GC candidate or
	// orphan.
	if n, err := s.db.MarkUnnamedCheckpointsFailed(ctx); err != nil {
		s.log.Printf("snapshot: mark unnamed checkpoints failed: %v", err)
	} else if n > 0 {
		s.log.Printf("snapshot: marked %d interrupted checkpoint build(s) failed", n)
	}
	return nil
}

// loadRunningJobsLocked seeds the running-job counts from the store.
// Call with s.store.mu held.
func (s *Service) loadRunningJobsLocked(ctx context.Context, leases map[string]*Lease) {
	rows, err := s.db.ListRunningJobs(ctx)
	if err != nil {
		s.storeError("list_running_jobs", "load", err)
		return
	}
	n := 0
	for _, j := range rows {
		if leases[j.LeaseID] == nil {
			continue // the lease is gone; its record goes with it
		}
		s.store.runningJobs[j.LeaseID]++
		n++
	}
	if s.metrics != nil {
		s.metrics.JobsRunning.Set(float64(n))
	}
}

// LiveLeases returns the ids of active (unreleased) leases. Used by
// the shutdown log line.
func (s *Service) LiveLeases() []string {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	ids := make([]string, 0, len(s.store.leases))
	for id, l := range s.store.leases {
		if !l.released {
			ids = append(ids, id)
		}
	}
	return ids
}

// Shutdown stops the background loops and flushes the batched
// LastActive updates to the store. It does NOT release leases or delete
// pooled sandboxes: state persists in the SQLite store and the next
// incarnation loads it via LoadState (U05).
func (s *Service) Shutdown(ctx context.Context) {
	if s.stopLoops != nil {
		s.stopLoops()
		s.stopLoops = nil // a new Start(ctx2) gets a fresh sweeper
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.flushLastActiveLocked(ctx)
}

// CollectMetrics updates live gauge metrics from the current service
// state (issue #20). Called by the Server before gathering metrics
// for /metrics. All store access is under the store lock.
func (s *Service) CollectMetrics(m *metrics.BackendMetrics) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	// Pool: ready count per image.
	for img, ids := range s.store.pool {
		m.PoolReady.WithLabelValues(img).Set(float64(len(ids)))
	}
	// Pool cap = poolSize × number of stocked images.
	if s.cfg.PoolSize > 0 {
		m.PoolCap.Set(float64(s.cfg.PoolSize * len(s.store.pool)))
	}

	// Leases: count non-released, per state (U11) and total.
	active := 0
	byState := map[string]int{}
	byImage := map[string]int{}
	for _, l := range s.store.leases {
		if !l.released {
			active++
			byState[l.State]++
			byImage[l.Image]++
		}
	}
	m.LeasesActive.Set(float64(active))
	for _, state := range []string{"running", "suspended", "recovered", "lost"} {
		m.LeasesByState.WithLabelValues(state).Set(float64(byState[state]))
	}
	// Reset first so an image whose last lease ended drops out.
	m.LeasesByImage.Reset()
	for img, n := range byImage {
		m.LeasesByImage.WithLabelValues(img).Set(float64(n))
	}

	// Quota reservations.
	pending := 0
	for _, n := range s.store.pending {
		pending += n
	}
	m.QuotaReserved.Set(float64(pending))

	// Shares.
	shares := 0
	for _, grantees := range s.store.shares {
		shares += len(grantees)
	}
	m.SharesActive.Set(float64(shares))

	// Preempted leases (#128 part 3): live leases suspended by
	// preemption, awaiting the resume queue.
	preempted := s.preemptedCountLocked()

	// Queued creates waiting for admission (#129 part 1).
	s.admitQ.mu.Lock()
	queued := len(s.admitQ.tickets)
	var oldestQueued time.Time
	for _, t := range s.admitQ.tickets {
		if oldestQueued.IsZero() || t.queuedAt.Before(oldestQueued) {
			oldestQueued = t.queuedAt
		}
	}
	s.admitQ.mu.Unlock()

	s.store.mu.Unlock()
	m.PreemptedLeases.Set(float64(preempted))
	m.LeasesQueued.Set(float64(queued))
	oldest := float64(0)
	if !oldestQueued.IsZero() {
		oldest = s.now().Sub(oldestQueued).Seconds()
	}
	m.LeasesQueuedOldest.Set(oldest)
	// Template bakes in flight (N1): the catalog's still-`building`
	// template builds, the same count the orphan sweep guards on. A read
	// failure leaves the gauge at its last value.
	if n, err := s.bakesRunning(context.Background()); err == nil {
		m.BuildsInFlight.Set(float64(n))
	}
	// Kept checkpoints (#126): pins of live leases and their recorded
	// bytes, refreshed on every scrape. CollectMetrics holds the store
	// lock only for the lease set it needs; the kept rows live in the
	// catalog.
	s.UpdateKeptMetrics(context.Background())
	s.UpdateNamedSnapshotMetrics(context.Background())
	s.store.mu.Lock()
}

var (
	errNotFound       = &leaseError{"lease not found"}
	errNotPersistent  = &leaseError{"lease is not a persistent lease"}
	errUnknownImage   = &leaseError{"unknown image"}
	errSuspended      = &leaseError{"lease is suspended"}
	errBadForkCount   = &leaseError{"count must be 1..20"}
	errLeaseBusy      = &leaseError{"lease is busy; retry"}
	errLeaseReleased  = &leaseError{"lease was released"}
	errHolderMismatch = &leaseError{"holder does not match the lease's current holder"}
	errBadRestartMode = &leaseError{"mode must be warm or cold"}
)

// leaseError is a simple sentinel error.
type leaseError struct{ msg string }

func (e *leaseError) Error() string { return e.msg }
