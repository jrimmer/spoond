package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Fair shares (#145 FS1). Every owner gets an equal floating slice of
// the box — 1/N of the hugepage memory pool and of the snapshot volume's
// usable bytes, where N is the number of owners that exist (each
// identity user, plus the legacy consumer token as one owner). There are
// no per-owner settings and no weights; the slice is recomputed when an
// owner is added or deleted.
//
// Usage is measured from recorded state, never a filesystem walk:
//
//	memory = the memory of the owner's RUNNING leases
//	disk   = the owner's pause snapshots + kept checkpoints + named snapshots
//
// ratio = usage / slice (0 when the slice is 0, e.g. a box with no
// capacity or no owners). This task only computes and reports the
// shares; the take-back policy that acts on them is a later unit.

// FairShareOwner is one owner's slice and usage as the API reports it.
type FairShareOwner struct {
	Owner string `json:"owner"`
	Name  string `json:"name,omitempty"`
	// SlicePct is the owner's share of the box as a percentage (equal
	// for every owner: 100/N).
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
}

// fairShareSnapshot is the computed per-owner view plus the box totals
// it was derived from. It is cached on the Service and invalidated when
// lease state changes or an owner is added or deleted.
type fairShareSnapshot struct {
	owners []*FairShareOwner
	byID   map[string]*FairShareOwner
	// ownersN is N: the number of owners that exist.
	ownersN int
	// memoryTotalMiB is the hugepage memory pool spoond admits against;
	// diskTotalBytes is the snapshot volume's usable bytes.
	memoryTotalMiB int
	diskTotalBytes int64
	// at is when the snapshot was computed.
	at time.Time
}

// fairSharesCache holds the last computed snapshot behind a mutex.
// invalidate() bumps the epoch so a concurrent reader that computed
// against stale data does not store it.
type fairSharesCache struct {
	mu    sync.Mutex
	snap  *fairShareSnapshot
	epoch uint64
}

// invalidateFairShares drops the cached snapshot. Every lease state
// change and every owner add/delete calls it. Cheap: the next read
// recomputes.
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

// totalHugepageMiB reports the box's hugepage memory pool in MiB, from
// the same cached NodeInfo admissions read (nodeInfoCache). ok is false
// when no reading can be had or the reading has no hugepage size; the
// caller then reports 0 slices rather than guess.
func (s *Service) totalHugepageMiB(ctx context.Context) (int, bool) {
	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	if s.nodeInfoAt.IsZero() || s.nodeInfoCache.HugepageSizeBytes == 0 {
		// Refresh once so a fresh backend reports a real slice.
		info, err := s.sub.NodeInfo(ctx)
		if err != nil {
			return 0, false
		}
		s.nodeInfoCache = info
		s.nodeInfoAt = s.now()
		if info.HugepageSizeBytes == 0 {
			return 0, false
		}
	}
	total := s.nodeInfoCache.HugepagesTotal * s.nodeInfoCache.HugepageSizeBytes
	return int(total / (1024 * 1024)), true
}

// totalSnapshotBytes reports the snapshot volume's usable bytes. A
// statfs failure reports 0, false so the caller falls back to a 0 slice
// rather than a wrong number.
func (s *Service) totalSnapshotBytes() (int64, bool) {
	total, _, err := s.diskCapacity(s.cfg.TemplateStoragePath)
	if err != nil {
		return 0, false
	}
	return int64(total), true
}

// fairShares computes and caches every owner's slice and usage. It reads
// the running leases under the store lock (memory), then the recorded
// snapshot sizes in a handful of store queries — never a filesystem walk
// per request. A cached snapshot is returned until a lease change or an
// owner add/delete invalidates it.
func (s *Service) fairShares(ctx context.Context) *fairShareSnapshot {
	// A short TTL bounds how stale a snapshot can be when a change was
	// missed (e.g. a lease state change that does not call invalidate);
	// the lifecycle calls invalidate explicitly, so this is a backstop.
	const ttl = 5 * time.Second
	s.fairShareCache.mu.Lock()
	if snap := s.fairShareCache.snap; snap != nil && s.now().Sub(snap.at) < ttl {
		s.fairShareCache.mu.Unlock()
		return snap
	}
	epoch := s.fairShareCache.epoch
	s.fairShareCache.mu.Unlock()

	snap := s.computeFairShares(ctx)

	s.fairShareCache.mu.Lock()
	// Only store it when no invalidation happened while computing: a
	// stale snapshot must not overwrite a fresh one.
	if s.fairShareCache.epoch == epoch {
		s.fairShareCache.snap = snap
	}
	s.fairShareCache.mu.Unlock()
	return snap
}

// computeFairShares builds a fresh snapshot from the current state.
func (s *Service) computeFairShares(ctx context.Context) *fairShareSnapshot {
	owners := s.ownersOfBox()
	n := len(owners)

	memTotalMiB, _ := s.totalHugepageMiB(ctx)
	diskTotalBytes, _ := s.totalSnapshotBytes()

	// Running-lease memory, under the store lock so a concurrent
	// release/suspend cannot tear the view. The disk sums come from
	// recorded sizes in the store below.
	s.store.mu.Lock()
	memUsed := map[string]int{}
	for _, l := range s.store.leases {
		if l.released || l.Owner == "" || !l.live() {
			continue
		}
		memUsed[l.Owner] += l.MemoryMB
	}
	s.store.mu.Unlock()

	// Disk usage by recorded snapshot size, three queries for the box.
	pausedByOwner, _ := s.db.PausedBytesByOwner(ctx)
	keptByOwner, _ := s.db.KeptBytesByOwner(ctx)
	namedByOwner, _ := s.db.NamedSnapshotBytesByOwner(ctx)

	// Memory slice: 1/N of the pool, integer MiB. Disk slice: 1/N of the
	// volume's usable bytes.
	memSliceMiB := 0
	if n > 0 {
		memSliceMiB = memTotalMiB / n
	}
	var diskSliceBytes int64
	if n > 0 {
		diskSliceBytes = diskTotalBytes / int64(n)
	}

	snap := &fairShareSnapshot{
		byID:           map[string]*FairShareOwner{},
		ownersN:        n,
		memoryTotalMiB: memTotalMiB,
		diskTotalBytes: diskTotalBytes,
		at:             s.now(),
	}
	for _, owner := range owners {
		o := &FairShareOwner{Owner: owner, Name: s.ownerDisplayName(owner)}
		if n > 0 {
			o.SlicePct = 100 / float64(n)
		}
		o.Memory.SliceMiB = memSliceMiB
		o.Memory.UsedMiB = memUsed[owner]
		o.Disk.SliceBytes = diskSliceBytes
		o.Disk.PausedBytes = pausedByOwner[owner]
		o.Disk.KeptBytes = keptByOwner[owner]
		o.Disk.NamedBytes = namedByOwner[owner]
		o.Disk.UsedBytes = o.Disk.PausedBytes + o.Disk.KeptBytes + o.Disk.NamedBytes
		o.Ratio = ratioOf(o, memSliceMiB, diskSliceBytes)
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

// fairShareFor returns one owner's view from the cached snapshot. ok is
// false when the owner does not exist (no identity row and no legacy
// token).
func (s *Service) fairShareFor(ctx context.Context, owner string) (*FairShareOwner, bool) {
	snap := s.fairShares(ctx)
	o, ok := snap.byID[owner]
	return o, ok
}

// fairShareOwners returns every owner's view, sorted by ratio descending
// (the owner furthest over their slice first; ties by owner id).
func (s *Service) fairShareOwners(ctx context.Context) []*FairShareOwner {
	snap := s.fairShares(ctx)
	out := append([]*FairShareOwner(nil), snap.owners...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ratio != out[j].Ratio {
			return out[i].Ratio > out[j].Ratio
		}
		return out[i].Owner < out[j].Owner
	})
	return out
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
	owners := s.svc.fairShareOwners(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"owners": owners})
}
