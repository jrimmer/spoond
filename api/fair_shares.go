package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v3/store"
)

// Fair shares (#145 FS1, amended 2026-10-14). Every owner gets an equal
// floating slice of the box — 1/N of the hugepage memory pool and of
// the snapshot volume's usable bytes, where N is the number of ACTIVE
// owners, not every identity: an owner is ACTIVE when it holds an
// unreleased lease whose state is running, recovered or suspended
// (paused) — not lost — or when it has an admission in flight, so a
// requester counts for its own first lease. "Definitely only fair share
// amongst actual platform users and not potential users": an idle
// identity and its legacy token do not count against everyone else's
// slice; starting to use the box grows N and shrinks every slice.
// Paused counts as active: the owner still occupies the box (its pause
// snapshot holds disk and its leases want their pool back). There are
// no per-owner settings and no weights; the slice is recomputed when an
// owner is added or deleted or becomes active or idle.
//
// Usage is measured from recorded state, never a filesystem walk:
//
//	memory = the memory of the owner's RUNNING leases
//	disk   = the owner's pause snapshots + kept checkpoints + named snapshots
//
// ratio = usage / slice (0 when the slice is 0, e.g. a box with no
// capacity or no active owners). This task only computes and reports
// the shares; the take-back policy that acts on them is a later unit.
//
// The disk slice basis is the snapshot volume's **usable** bytes: the
// statfs free-to-unprivileged bytes plus the snapshot bytes spoond
// already accounts for, so it is the space spoond can hand out (not the
// raw filesystem total, which includes the root reserve). When the
// node-info cache is cold or a store read fails the capacity is unknown:
// CapacityKnown is false, the slices are 0 and the result is not cached.

// FairShareOwner is one owner's slice and usage as the API reports it.
type FairShareOwner struct {
	Owner string `json:"owner"`
	Name  string `json:"name,omitempty"`
	// SlicePct is the owner's share of the box as a percentage: 100/N
	// for an active owner, 100/(N+1) — the slice starting would give
	// it — for an inactive one.
	SlicePct float64 `json:"slice_pct"`
	Memory   struct {
		SliceMiB int `json:"slice_mib"`
		UsedMiB  int `json:"used_mib"`
	} `json:"memory"`
	Disk struct {
		SliceBytes  int64 `json:"slice_bytes"`
		UsedBytes   int64 `json:"used_bytes"`
		PausedBytes int64 `json:"paused_bytes"`
		KeptBytes   int64 `json:"kept_bytes"`
		NamedBytes  int64 `json:"named_bytes"`
	} `json:"disk"`
	// Ratio is usage/slice, the larger of the memory and disk ratios. An
	// owner inside their slice is below 1; the owner furthest over is
	// taken from first.
	Ratio float64 `json:"ratio"`
	// Active reports whether this owner counts toward N: it holds an
	// unreleased running or paused lease or has an admission in
	// flight. Every owner is listed; an inactive one shows the slice
	// it would get on starting (total/(N+1)).
	Active bool `json:"active"`
	// CapacityKnown reports whether the box totals behind this slice
	// were readable: false means the node-info cache was cold or a store
	// read failed, so the slices are not computed yet and only the usage
	// buckets that were readable are meaningful. It is a box property
	// copied onto each share so a share object is self-describing.
	CapacityKnown bool `json:"capacity_known"`
}

// fairShareSnapshot is the computed per-owner view plus the box totals
// it was derived from. It is cached on the Service and invalidated when
// lease state changes or an owner is added or deleted.
type fairShareSnapshot struct {
	owners []*FairShareOwner
	byID   map[string]*FairShareOwner
	// ownersN is N: the number of ACTIVE owners (holding a running or
	// paused lease, or an admission in flight). Inactive owners are
	// still listed in owners; they just do not count here.
	ownersN int
	// memoryTotalMiB is the hugepage memory pool spoond admits against;
	// diskTotalBytes is the snapshot volume's usable bytes.
	memoryTotalMiB int
	diskTotalBytes int64
	// at is when the snapshot was computed.
	at time.Time
	// capacityKnown reports whether every capacity read behind the
	// snapshot succeeded. A snapshot with capacityKnown false is still
	// returned to the caller (it carries whatever was readable) but is
	// never cached.
	capacityKnown bool
}

// fairShareComputeTimeout bounds one computation of the box-wide view.
// The computation runs on a context detached from the request that
// happened to trigger it, so a client disconnect cannot poison the cache
// and a hung store or statfs cannot pin a compute forever.
const fairShareComputeTimeout = 5 * time.Second

// fairSharesCache holds the last computed snapshot behind a mutex.
// invalidate() bumps the epoch so a concurrent reader that computed
// against stale data does not store it.
type fairSharesCache struct {
	mu    sync.Mutex
	snap  *fairShareSnapshot
	epoch uint64
}

// invalidateFairShares drops the cached snapshot. Every lease state
// change, every owner add/delete and every pending-admission 0↔>0
// transition (an owner becoming active or idle, requirement 3) calls
// it. Cheap: the next read recomputes.
func (s *Service) invalidateFairShares() {
	s.fairShareCache.mu.Lock()
	s.fairShareCache.epoch++
	s.fairShareCache.snap = nil
	s.fairShareCache.mu.Unlock()
}

// ownersOfBox returns the owner ids that exist: every identity user,
// plus any legacy consumer token's owner. A token owner that is also an
// identity user is listed once. The set is sorted by id.
func (s *Service) ownersOfBox() []string {
	seen := map[string]struct{}{}
	add := func(id string) {
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	if s.identities != nil {
		for _, u := range s.identities.Users() {
			add(u.ID)
		}
	}
	for _, id := range s.tokens {
		add(id)
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// activeOwners reports which owners are ACTIVE — they count toward N —
// reading s.store.mu exactly once. An owner is active when it holds an
// unreleased lease whose state is running, recovered or suspended
// (paused) — not lost, whose memory cannot be taken back and whose
// hugepages were lost — or when it has an admission in flight, so a
// requester counts for its own first lease.
func (s *Service) activeOwners() map[string]struct{} {
	active := map[string]struct{}{}
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.released || l.Owner == "" {
			continue
		}
		if l.live() || l.Suspended {
			active[l.Owner] = struct{}{}
		}
	}
	for owner, n := range s.store.pendingMiB {
		if n > 0 && owner != "" {
			active[owner] = struct{}{}
		}
	}
	s.store.mu.Unlock()
	return active
}

// ownerDisplayName resolves an owner id to a username when one exists;
// "" for a legacy consumer-token owner.
func (s *Service) ownerDisplayName(owner string) string {
	if s.identities == nil {
		return ""
	}
	if u := s.identities.UserByID(owner); u != nil {
		return u.Name
	}
	return ""
}

// totalHugepageMiB reports the box's hugepage memory pool in MiB from
// the existing NodeInfo cache (kept warm by updateNodeMetrics and the
// admission path). It deliberately makes no RPC: the fair-share view is
// read on a request path, and calling s.sub.NodeInfo here would put a
// substrate round trip (under nodeInfoMu) behind every read and could
// stampede the orchestrator. When the cache is cold the capacity is
// unknown: ok is false and the caller reports a not-ok, uncached
// snapshot. A warm reading with no hugepage size is a real zero capacity
// and reports ok true.
func (s *Service) totalHugepageMiB() (int, bool) {
	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	if s.nodeInfoAt.IsZero() {
		return 0, false
	}
	if s.nodeInfoCache.HugepageSizeBytes == 0 {
		return 0, true
	}
	total := s.nodeInfoCache.HugepagesTotal * s.nodeInfoCache.HugepageSizeBytes
	return int(total / (1024 * 1024)), true
}

// usableSnapshotBytes reports the snapshot volume's usable bytes: the
// statfs bytes available to an unprivileged writer (Bavail, the second
// return of diskCapacity) plus the snapshot bytes spoond itself already
// accounts for (the box-level sum over DISTINCT build_id from
// store.AccountedSnapshotBytes, which collapses the pause, kept-pin and
// named arms to one size per build). That is the space spoond can hand out
// to owners, not the raw filesystem total: the root-reserved blocks are
// not spoond's to give, and the bytes spoond already holds are still
// part of the pool the next snapshot can draw on once reclaimed. A
// statfs failure reports 0, false so the caller reports unknown capacity
// rather than a wrong slice.
func (s *Service) usableSnapshotBytes(accounted int64) (int64, bool) {
	_, free, err := s.diskCapacity(s.cfg.TemplateStoragePath)
	if err != nil {
		return 0, false
	}
	return int64(free) + accounted, true
}

// fairShares computes and caches every owner's slice and usage. It reads
// the active owners and running leases under one store lock (memory),
// then the recorded snapshot sizes in a handful of store queries — never
// a filesystem walk per request. A cached snapshot is returned until a
// lease change, a pending-admission change or an owner add/delete
// invalidates it. ok is false when any read failed: the
// partial snapshot is still returned (the caller reports what it knows)
// but it is not cached.
func (s *Service) fairShares(ctx context.Context) *fairShareSnapshot {
	// A short TTL bounds how stale a snapshot can be when a change was
	// missed (e.g. an activity that does not change any accounted
	// quantity); the lifecycle calls invalidate explicitly, so this is a
	// backstop.
	const ttl = 5 * time.Second
	s.fairShareCache.mu.Lock()
	if snap := s.fairShareCache.snap; snap != nil && s.now().Sub(snap.at) < ttl {
		s.fairShareCache.mu.Unlock()
		return snap
	}
	epoch := s.fairShareCache.epoch
	s.fairShareCache.mu.Unlock()

	// Compute on a context detached from the request and bounded in time:
	// the snapshot covers the whole box, so one client disconnect or one
	// hung store read must not be turned into cached zeros.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fairShareComputeTimeout)
	defer cancel()
	snap := s.computeFairShares(cctx)
	if !snap.capacityKnown {
		// A failed read must not be cached: the next caller retries.
		return snap
	}

	s.fairShareCache.mu.Lock()
	// Only store it when no invalidation happened while computing: a
	// stale snapshot must not overwrite a fresh one.
	if s.fairShareCache.epoch == epoch {
		s.fairShareCache.snap = snap
	}
	s.fairShareCache.mu.Unlock()
	return snap
}

// computeFairShares builds a fresh snapshot from the current state. The
// snapshot's capacityKnown is false when any capacity read failed; it
// then carries only the values that were readable.
func (s *Service) computeFairShares(ctx context.Context) *fairShareSnapshot {
	owners := s.ownersOfBox()
	// N counts the ACTIVE owners only — a running or paused lease, or an
	// admission in flight — not every identity (2026-10-14 owner
	// decision). store.mu is read once (here) and the whole active set
	// and memory view are built under it, so no second acquisition can
	// interleave between them.
	s.store.mu.Lock()
	active := map[string]struct{}{}
	memUsed := map[string]int{}
	for _, l := range s.store.leases {
		if l.released || l.Owner == "" {
			continue
		}
		if l.live() || l.Suspended {
			active[l.Owner] = struct{}{}
		}
		// The memory charge stays the running leases only: a suspended
		// lease holds no hugepages and a lost one cannot be taken back.
		if l.live() {
			memUsed[l.Owner] += l.MemoryMB
		}
	}
	for owner, n := range s.store.pendingMiB {
		if owner == "" || n <= 0 {
			continue
		}
		active[owner] = struct{}{}
	}
	s.store.mu.Unlock()
	n := len(active)

	capacityKnown := true
	memTotalMiB, memOK := s.totalHugepageMiB()
	if !memOK {
		capacityKnown = false
	}

	// Disk usage by recorded snapshot size, one query for the box. A
	// failed query must not be cached as zero disk. A build that is both
	// kept and named counts once in its owner's Used.
	usageByOwner, err := s.db.DiskUsageByOwner(ctx)
	if err != nil {
		s.log.Printf("fair shares: disk usage: %v", err)
		capacityKnown = false
		usageByOwner = map[string]store.OwnerUsage{}
	}
	// The disk slice basis adds the snapshot bytes spoond already holds,
	// counted per build. A build shared by several owners (one pins
	// another's build, or a named snapshot is owned apart from its
	// build's owner) must count once for the box, not once per owner, so
	// this is a box-level sum over DISTINCT build_id rather than the sum
	// of each owner's Used (FS1 follow-up b). A failed read is the same
	// unknown capacity as a failed DiskUsageByOwner: do not cache it.
	accounted, err := s.db.AccountedSnapshotBytes(ctx)
	if err != nil {
		s.log.Printf("fair shares: accounted snapshot bytes: %v", err)
		capacityKnown = false
	}

	// The disk slice basis is the volume's usable bytes: free-to-
	// unprivileged plus the snapshot bytes spoond already accounts for.
	diskTotalBytes, diskOK := s.usableSnapshotBytes(accounted)
	if !diskOK {
		capacityKnown = false
	}

	// Memory slice: 1/N of the pool for each ACTIVE owner, integer MiB.
	// Disk slice: 1/N of the volume's usable bytes. When the capacity was
	// not readable the slices are not computed yet (capacity_known false)
	// and stay 0, so a caller never reads a wrong number for a real one.
	// N=0 (no active owners) keeps the slice math off entirely: nothing
	// divides by zero and every reported slice is 0.
	memSliceMiB := 0
	var diskSliceBytes int64
	if capacityKnown && n > 0 {
		memSliceMiB = memTotalMiB / n
		diskSliceBytes = diskTotalBytes / int64(n)
	}

	snap := &fairShareSnapshot{
		byID:           map[string]*FairShareOwner{},
		ownersN:        n,
		memoryTotalMiB: memTotalMiB,
		diskTotalBytes: diskTotalBytes,
		at:             s.now(),
		capacityKnown:  capacityKnown,
	}
	for _, owner := range owners {
		_, isActive := active[owner]
		o := &FairShareOwner{Owner: owner, Name: s.ownerDisplayName(owner), CapacityKnown: capacityKnown, Active: isActive}
		if isActive && n > 0 {
			// An active owner's slice is 1/N of the box (N=0 is the
			// guard above: no capacity, no slice, no ratio).
			o.SlicePct = 100 / float64(n)
			o.Memory.SliceMiB = memSliceMiB
			o.Disk.SliceBytes = diskSliceBytes
		} else if !isActive {
			// An inactive owner shows the slice it would get on
			// starting: total/(N+1).
			o.SlicePct, o.Memory.SliceMiB, o.Disk.SliceBytes = idleSlice(memTotalMiB, diskTotalBytes, n, capacityKnown)
		}
		o.Memory.UsedMiB = memUsed[owner]
		u := usageByOwner[owner]
		o.Disk.PausedBytes = u.Paused
		o.Disk.KeptBytes = u.Kept
		o.Disk.NamedBytes = u.Named
		o.Disk.UsedBytes = u.Used
		// Ratio is usage over the owner's own displayed slice, so an
		// active owner reads against 1/N and an inactive one (whose
		// usage is 0 even when it has a stale named snapshot) reads
		// 0. Passing the owner's own slice values keeps the ratio
		// consistent with the slice fields above it.
		o.Ratio = ratioOf(o, o.Memory.SliceMiB, o.Disk.SliceBytes)
		snap.owners = append(snap.owners, o)
		snap.byID[owner] = o
	}
	sort.SliceStable(snap.owners, func(i, j int) bool {
		return snap.owners[i].Owner < snap.owners[j].Owner
	})
	return snap
}

// ratioOf is usage/slice across memory and disk: the larger of the two
// per-resource ratios. A zero slice contributes no ratio, so a box with
// no capacity (or no owners) reports a ratio of 0 for everyone.
func ratioOf(o *FairShareOwner, memSliceMiB int, diskSliceBytes int64) float64 {
	var ratio float64
	if memSliceMiB > 0 {
		ratio = float64(o.Memory.UsedMiB) / float64(memSliceMiB)
	}
	if diskSliceBytes > 0 {
		if r := float64(o.Disk.UsedBytes) / float64(diskSliceBytes); r > ratio {
			ratio = r
		}
	}
	return ratio
}

// idleSlice is the slice an inactive owner is shown: the share it would
// get on starting, total/(N+1), since N is the active owners and it is
// not among them. N=0 shows the whole box: nothing is divided while no
// one is active, and starting would take the pool entire. With unknown
// capacity no slice is computed and everything stays 0.
func idleSlice(memTotalMiB int, diskTotalBytes int64, n int, capacityKnown bool) (float64, int, int64) {
	if !capacityKnown {
		return 0, 0, 0
	}
	return 100 / float64(n+1), memTotalMiB / (n + 1), diskTotalBytes / int64(n+1)
}

// fairShareFor returns one owner's view from the current snapshot. ok is
// false when the owner does not exist (no identity row and no legacy
// token). A failed box read still yields the owner's (partial) view: the
// owner lookup itself is independent of the capacity reads.
func (s *Service) fairShareFor(ctx context.Context, owner string) (*FairShareOwner, bool) {
	snap := s.fairShares(ctx)
	o, ok := snap.byID[owner]
	return o, ok
}

// fairSharesView returns the sorted owner list and the snapshot's
// capacityKnown for the current box view. The owner list and the
// capacity flag come from one snapshot, so a cold compute does not run
// twice. The list is sorted by ratio descending (the owner furthest over
// their slice first; ties by owner id).
func (s *Service) fairSharesView(ctx context.Context) ([]*FairShareOwner, bool) {
	snap := s.fairShares(ctx)
	out := append([]*FairShareOwner(nil), snap.owners...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ratio != out[j].Ratio {
			return out[i].Ratio > out[j].Ratio
		}
		return out[i].Owner < out[j].Owner
	})
	return out, snap.capacityKnown
}

// handleUserUsage is GET /api/users/{id} (admin only): one identity
// user's record plus their fair-share slice and usage (#145 FS1). It is
// the admin view; a caller reads their own through GET /api/users/me.
func (s *Server) handleUserUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	u := s.svc.identities.UserByID(id)
	if u == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	share, _ := s.svc.fairShareFor(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":  toUserView(u, s.svc.usedMiB(u.ID)),
		"share": share,
	})
}

// handleSharesList is GET /api/fair-shares (admin only): every owner's
// slice and usage, sorted by ratio descending so the owner furthest over
// their slice comes first (#145 FS1). Read-only; the take-back policy
// reads the same computed view. It is deliberately not /api/shares,
// which still lists lease grants.
func (s *Server) handleSharesList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	owners, known := s.svc.fairSharesView(r.Context())
	out := map[string]any{
		"capacity_known": known,
		"owners":         owners,
	}
	// The snapshot disk's room, when it can be read: the same numbers the
	// take-back policy decides with, so an operator sees why a request may
	// be refused before it is. Unknown capacity omits the field entirely.
	if room, ok := s.svc.diskRoomNow(r.Context()); ok {
		out["disk"] = diskRoomJSON(room)
	}
	writeJSON(w, http.StatusOK, out)
}

// diskRoomJSON is one room snapshot as the GET /api/fair-shares "disk"
// object: every size in bytes, named so a byte count is never mistaken
// for a lease count.
func diskRoomJSON(room diskRoom) map[string]int64 {
	return map[string]int64{
		"total_bytes":    room.Total,
		"free_bytes":     room.Free,
		"reserved_bytes": room.Reserved,
		"floor_bytes":    room.Floor,
		"pending_bytes":  room.Pending,
		"freeing_bytes":  room.Freeing,
		"usable_bytes":   room.Usable,
	}
}
