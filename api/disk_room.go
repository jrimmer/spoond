package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v2/store"
)

// errDiskUnknown marks a snapshot-capacity read that failed: disk room
// is unknown, so nothing may be reclaimed on a wrong number and the
// caller treats the request as a wait, never a refusal.
var errDiskUnknown = errors.New("disk: capacity unknown")

// sawRequesterAmong reports whether owners contains requester. The
// selector needs the requester's after-request ratio to bound how far
// any owner may be pushed down; without the requester in the view there
// is no safe pick.
func sawRequesterAmong(owners []diskOwner, requester string) bool {
	for _, o := range owners {
		if o.Owner == requester {
			return true
		}
	}
	return false
}

// diskRoom is one snapshot of how much disk a new request may use.
// All fields are bytes. Usable may be negative: that is the shortfall.
type diskRoom struct {
	Total int64
	Free  int64
	// Reserved is the running leases' WHOLE disk_mb allowance (not what
	// they have written): bytes a VM already wrote count again, which is
	// accepted as the conservative option.
	Reserved int64
	// Floor is the reserve kept free (DISK_RESERVE_PCT of Total).
	Floor   int64
	Pending int64 // admitted requests that have not written yet
	Freeing int64 // deletions issued but not yet visible in statfs
	Usable  int64
}

// computeDiskRoom is the pure room formula:
// Usable = Free - Reserved - Floor - Pending + Freeing.
func computeDiskRoom(total, free, reservedRunningBytes, pending, freeing int64, reservePct float64) diskRoom {
	floor := int64(float64(total) * reservePct / 100)
	return diskRoom{
		Total:    total,
		Free:     free,
		Reserved: reservedRunningBytes,
		Floor:    floor,
		Pending:  pending,
		Freeing:  freeing,
		Usable:   free - reservedRunningBytes - floor - pending + freeing,
	}
}

const defaultDiskReservePct = 5.0

// diskReservePct reads DISK_RESERVE_PCT: the share of the disk kept free,
// in percent. Unset, non-numeric or out-of-range-NaN values give 5;
// values are clamped to 0..50.
func diskReservePct() float64 {
	v := strings.TrimSpace(os.Getenv("DISK_RESERVE_PCT"))
	if v == "" {
		return defaultDiskReservePct
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f != f {
		return defaultDiskReservePct
	}
	if f < 0 {
		return 0
	}
	if f > 50 {
		return 50
	}
	return f
}

// diskFreeingTTL is how long a noted deletion counts as "still freeing"
// when statfs never shows the growth (ZFS frees late).
const diskFreeingTTL = 30 * time.Second

type freeingEntry struct {
	bytes      int64
	at         time.Time
	freeAtNote int64
}

// diskInflight tracks bytes promised to admitted requests (pending) and
// bytes deleted but not yet visible in statfs (freeing).
type diskInflight struct {
	mu      sync.Mutex
	pending int64
	freeing []freeingEntry
}

// reserve adds n bytes to pending and returns an idempotent release.
func (d *diskInflight) reserve(n int64) (release func()) {
	d.mu.Lock()
	d.pending += n
	d.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			d.pending -= n
			d.mu.Unlock()
		})
	}
}

// noteFreeing records that n bytes were just deleted while statfs free
// read freeAtNote.
func (d *diskInflight) noteFreeing(n int64, at time.Time, freeAtNote int64) {
	d.mu.Lock()
	d.freeing = append(d.freeing, freeingEntry{bytes: n, at: at, freeAtNote: freeAtNote})
	d.mu.Unlock()
}

// settle drops freeing entries that statfs has caught up with (free grew
// by at least the entry's bytes since the note) or that are 30 s old, and
// returns the current pending and freeing totals.
func (d *diskInflight) settle(now time.Time, freeNow int64) (pending, freeing int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kept := d.freeing[:0]
	for _, e := range d.freeing {
		if freeNow-e.freeAtNote >= e.bytes || now.Sub(e.at) >= diskFreeingTTL {
			continue
		}
		kept = append(kept, e)
		freeing += e.bytes
	}
	d.freeing = kept
	return d.pending, freeing
}

// diskLease is one paused lease an owner holds on disk.
type diskLease struct {
	ID       string
	Bytes    int64
	Pinned   bool
	PausedAt time.Time
}

// diskOwner is one owner's disk use against its fair slice.
type diskOwner struct {
	Owner      string
	UsedBytes  int64
	SliceBytes int64
	Paused     []diskLease
}

// diskVictimAgainstRequester reports whether giving up a lease of used
// bytes leaves the owner strictly above the ratio the requester would
// sit at after its request — the alignment with memVictims (FS2): an
// owner is only taken while it stays further over its slice than the
// requester would be, so take-back never pushes an owner below the
// requester.
func diskVictimAgainstRequester(used, leaseBytes, slice, requesterRatio float64) bool {
	if slice <= 0 {
		return false
	}
	return (used-leaseBytes)/slice > requesterRatio
}

// diskVictim is a paused lease chosen for take-back.
type diskVictim struct {
	Owner string
	diskLease
	// Ratio is the owner's disk ratio (used/slice) when it was chosen.
	Ratio float64
}

func diskRatio(used, slice int64) float64 {
	if slice <= 0 {
		return 0
	}
	return float64(used) / float64(slice)
}

// diskVictims picks paused leases to release so a request of needBytes
// fits for requester. needBytes sets the requester's after-request
// ratio (the bar every candidate must beat); coverBytes is how much must
// actually be freed — the loop stops once the picks cover it, so a
// shortfall smaller than the need is met by fewer releases. The owner
// with the highest disk ratio goes first, and only while it stays above
// the requester's after-request ratio; its own after-give-up ratio must
// too (the alignment with memVictims, FS2). Within an owner the oldest
// unpinned paused lease goes first; pinned leases never. Returns nil
// when the cover cannot be met.
func diskVictims(owners []diskOwner, requester string, needBytes, coverBytes int64) []diskVictim {
	if coverBytes <= 0 || len(owners) == 0 || !sawRequesterAmong(owners, requester) {
		return nil
	}
	type state struct {
		o     diskOwner
		used  int64
		leaps []diskLease // unpinned, oldest first
	}
	var reqRatio float64
	var states []*state
	for _, o := range owners {
		if o.Owner == requester {
			reqRatio = diskRatio(o.UsedBytes+needBytes, o.SliceBytes)
			continue
		}
		st := &state{o: o, used: o.UsedBytes}
		for _, l := range o.Paused {
			if !l.Pinned {
				st.leaps = append(st.leaps, l)
			}
		}
		sort.SliceStable(st.leaps, func(i, j int) bool {
			return st.leaps[i].PausedAt.Before(st.leaps[j].PausedAt)
		})
		states = append(states, st)
	}
	var out []diskVictim
	var freed int64
	for freed < coverBytes {
		var best *state
		var bestRatio float64
		for _, st := range states {
			if len(st.leaps) == 0 || st.o.SliceBytes <= 0 || st.used <= st.o.SliceBytes {
				continue
			}
			r := diskRatio(st.used, st.o.SliceBytes)
			// The candidate must leave the owner strictly above the
			// requester's after-request ratio (checked on the lease's own
			// bytes, so a bigger lease may be unreachable while a smaller
			// one of the same owner still qualifies). That is the
			// alignment with memVictims (FS2): an owner is taken only
			// while it stays further over its slice than the requester
			// would be after its request, which also keeps take-back
			// from pushing an owner below the point the requester itself
			// would sit at.
			if r <= reqRatio ||
				!diskVictimAgainstRequester(float64(st.used), float64(st.leaps[0].Bytes), float64(st.o.SliceBytes), reqRatio) {
				continue
			}
			if best == nil || r > bestRatio {
				best, bestRatio = st, r
			}
		}
		if best == nil {
			return nil
		}
		l := best.leaps[0]
		best.leaps = best.leaps[1:]
		best.used -= l.Bytes
		freed += l.Bytes
		out = append(out, diskVictim{Owner: best.o.Owner, diskLease: l, Ratio: bestRatio})
	}
	return out
}

// The snapshot volume's floor percentage (DISK_RESERVE_PCT), the
// running-lease reservation and the take-back order all live in the
// disk room accounting below; the flat free-percent thresholds of the
// held-lease critical rules are a separate, older mechanism.

// boxFullError is returned when no room can be made for an owner.
type boxFullError struct {
	Owner      string
	UsedBytes  int64
	SliceBytes int64
}

func (e *boxFullError) Error() string {
	return fmt.Sprintf("box full: owner %q uses %d of %d slice bytes and no disk can be reclaimed",
		e.Owner, e.UsedBytes, e.SliceBytes)
}

// Unwrap makes errors.Is(err, errBoxFull) (held.go) match, so the lease
// API maps it to 429 box_full as before.
func (e *boxFullError) Unwrap() error { return errBoxFull }

// diskTakeBack frees at least needBytes of disk for requester and returns
// the bytes freed. Steps:
//  1. Garbage first: reapOrphans, keeping its GC_DELETE guard (dry-run
//     modes free nothing).
//  2. Re-check room; if still short, ask diskVictims for one victim at a
//     time.
//  3. For each victim emit lease.critical_release {owner, ratio, free_pct}
//     and call releaseBecause(ctx, l, "disk_reclaim").
//  4. noteFreeing(bytes, now, freeAtNote) on the inflight tracker, then
//     re-check statfs/room before choosing another victim.
//  5. Emit exactly one disk.cleanup event per call with bytes per category
//     (garbage, reclaimed leases).
//  6. Return *boxFullError when nothing could be freed.
func (s *Service) diskTakeBack(ctx context.Context, needBytes int64, requester string) (freed int64, err error) {
	if needBytes <= 0 {
		return 0, nil
	}
	room, ok := s.diskRoomNow(ctx)
	if !ok {
		// An unreadable statfs must not delete anything: the caller treats
		// unknown capacity as a wait, never a refusal.
		return 0, errDiskUnknown
	}
	shortfall := needBytes - room.Usable
	if shortfall <= 0 {
		return 0, nil
	}

	garbageFreed := s.reapGarbageForRoom(ctx)
	s.noteDiskFreeing(garbageFreed, room.Free)
	freed += garbageFreed
	// Garbage alone may cover the need: no lease is then touched.
	if room2, ok := s.diskRoomNow(ctx); ok && room2.Usable >= needBytes {
		s.emitDiskCleanup(garbageFreed, 0)
		return freed, nil
	}

	leaseFreed, err := s.reclaimLeasesForRoom(ctx, requester, needBytes)
	freed += leaseFreed

	s.emitDiskCleanup(garbageFreed, leaseFreed)
	if err != nil {
		return freed, err
	}
	return freed, nil
}

// reapGarbageForRoom runs one orphan-reap pass and returns the bytes it
// visibly freed. Quarantine mode moves bytes aside without freeing them,
// so the count is honest: only an actual purge reports. The ORPHAN_REAP
// and GC_DELETE guards inside reapOrphans stand unchanged.
func (s *Service) reapGarbageForRoom(ctx context.Context) int64 {
	_, freed := s.reapOrphans(ctx)
	return freed
}

// reclaimLeasesForRoom loops diskVictims one victim at a time: pick,
// announce, release, credit freeing, re-check room — stopping as soon as
// the room covers the need. The take-back mutex keeps two concurrent
// calls from reclaiming for the same shortfall: the second call re-reads
// the room once it holds the mutex and finds the first call's work. A
// victim that was no longer releasable at the moment of commitment (a
// resume that landed between the pick and the release) is skipped: its
// bytes never freed, so the loop goes around again and the next pick
// sees the true state. *boxFullError when no unpinned paused lease of a
// further-over owner can free the need.
func (s *Service) reclaimLeasesForRoom(ctx context.Context, requester string, needBytes int64) (int64, error) {
	s.takeDiskMu.Lock()
	defer s.takeDiskMu.Unlock()
	var freed int64
	// stale holds ids this pass already offered and found changed at
	// commitment (a resume or a pin that won the race). The candidate
	// list reads the catalog and can still name them; they are skipped
	// so the loop makes progress while the row lags the live state.
	stale := map[string]bool{}
	for {
		room, ok := s.diskRoomNow(ctx)
		if !ok {
			return freed, errDiskUnknown
		}
		if room.Usable >= needBytes {
			return freed, nil
		}
		// One lease-catalog read per pass, shared by every owner's view:
		// the in-memory leases when loaded, the store rows otherwise.
		rows := s.leasesOnce(ctx)
		owners := s.diskOwnersAtView(ctx, rows)
		if len(stale) > 0 {
			for i := range owners {
				kept := owners[i].Paused[:0]
				for _, l := range owners[i].Paused {
					if !stale[l.ID] {
						kept = append(kept, l)
					}
				}
				owners[i].Paused = kept
			}
		}
		// The selector is asked for the SHORTFALL (what the room is
		// still missing), not the whole need: garbage or a first victim
		// may already have closed most of the gap. The need itself stays
		// the requester's after-request ratio bar.
		victims := diskVictims(owners, requester, needBytes, needBytes-room.Usable)
		if len(victims) == 0 {
			// Nothing (left) to take: the good-citizen refusal (FS3b
			// maps it to 429 box_full and raises the alert).
			if req, ok := s.fairShareFor(ctx, requester); ok {
				return freed, &boxFullError{Owner: requester, UsedBytes: req.Disk.UsedBytes, SliceBytes: req.Disk.SliceBytes}
			}
			return freed, errBoxFull
		}
		if !s.takeDiskVictim(ctx, victims[0], room.Free) {
			// A stale pick — a resume or a pin won the race between the
			// catalog read and the commitment. No event ever went out;
			// mark it so the next pass offers a fresh candidate.
			stale[victims[0].ID] = true
			continue
		}
		freed += victims[0].Bytes
	}
}

// takeDiskVictim releases one paused lease for disk take-back: the
// LIVE lease (s.store.leases under the store lock — a stub would carry
// no Suspended state and no SandboxID for the teardown) is released with
// reason disk_reclaim, re-checked inside the same lock section that
// marks it released: still paused, unpinned, unreleased and not
// mid-resume at the moment of commitment, so a resume or a pin that
// landed between the pick and here wins and nothing is deleted. Only a
// committed release emits lease.critical_release {owner, ratio,
// free_pct} — a stale pick announces nothing. Afterwards the lease
// answers 404 everywhere. ok is false when the release was abandoned;
// the caller must not count its bytes as freed.
func (s *Service) takeDiskVictim(ctx context.Context, v diskVictim, freeAtPick int64) bool {
	s.store.mu.Lock()
	l, ok := s.store.leases[v.ID]
	s.store.mu.Unlock()
	if !ok {
		return false
	}
	released := false
	total, _, err := s.diskCapacity(s.cfg.TemplateStoragePath)
	freePct := -1.0
	if err == nil && total > 0 {
		freePct = float64(freeAtPick) / float64(total) * 100
	}
	s.releaseBecauseIf(ctx, l, "disk_reclaim", func(c *Lease) bool {
		released = !c.released && c.Suspended && !c.busy && !c.Pinned && c.ID == v.ID
		return released
	})
	s.emitLeaseEvent(v.ID, v.Owner, LeaseCriticalRelease,
		fmt.Sprintf("owner %q at %.0f%% of its disk slice; %.1f%% of the snapshot disk free; reclaiming paused lease %s",
			v.Owner, v.Ratio*100, freePct, shortID(v.ID)))
	if !released {
		return false
	}
	s.log.Printf("disk reclaim: releasing paused lease %s of %q (ratio %.2f, %s) for a request that does not fit",
		v.ID, v.Owner, v.Ratio, formatEventBytes(v.Bytes))
	s.noteDiskFreeing(v.Bytes, freeAtPick)
	return true
}

// diskOwners builds the selector's view of every owner: disk used and
// slice from the fair-share snapshot, plus the owner's PAUSED leases
// with their recorded pause-snapshot bytes, pin and pause date. Only a
// suspended lease's pause bytes are reclaimable, so only those are
// candidates; a running lease never appears in Paused. A failed disk
// usage read leaves the owner with no candidates rather than a wrong
// list.
func (s *Service) diskOwners(ctx context.Context) []diskOwner {
	return s.diskOwnersAtView(ctx, s.leasesOnce(ctx))
}

// diskOwnersAtView is diskOwners against a pre-read lease listing: the
// take-back loop reads the catalog once per pass and hands the same
// rows to every owner view it builds.
func (s *Service) diskOwnersAtView(ctx context.Context, rows []store.LeaseRow) []diskOwner {
	snap := s.fairShares(ctx)
	byPauseBuild, ok := s.pauseBuildsByLease(ctx)
	owners := make([]diskOwner, 0, len(snap.owners))
	for _, o := range snap.owners {
		do := diskOwner{Owner: o.Owner, UsedBytes: o.Disk.UsedBytes, SliceBytes: o.Disk.SliceBytes}
		if ok {
			do.Paused = s.pausedLeaseList(rows, o.Owner, byPauseBuild)
		}
		owners = append(owners, do)
	}
	return owners
}

// pauseBuildsByLease maps lease id → pause build row for every lease's
// resume build, from one catalog read. A pause build names the lease it
// was snapshotted from as its parent (pauseLeaseBody writes
// ParentBuildID = l.ID), so the key is the parent. ok is false when the
// read failed (no candidate is built from a half-read catalog).
func (s *Service) pauseBuildsByLease(ctx context.Context) (map[string]store.BuildRow, bool) {
	builds, err := s.db.ListBuilds(ctx)
	if err != nil {
		s.log.Printf("disk reclaim: list builds: %v", err)
		return nil, false
	}
	out := make(map[string]store.BuildRow)
	for _, b := range builds {
		if b.Kind == "pause" && b.State != "deleted" {
			out[b.ParentBuildID] = b
		}
	}
	return out, true
}

// pausedLeasesOf lists owner's suspended leases as diskLease candidates
// (the one-owner path; the take-back loop uses diskOwnersAtView): one
// per lease, sized by its lease's pause build's recorded size_bytes
// (the number the disk usage accounting reads, so the victim list and
// the owner's Used can never disagree). A lease with no pause build row
// is skipped — its bytes are gone or unknown. Pinned leases are carried
// so the selector can refuse them explicitly.
func (s *Service) pausedLeasesOf(ctx context.Context, owner string, byPauseBuild map[string]store.BuildRow) []diskLease {
	return s.pausedLeaseList(s.leasesOnce(ctx), owner, byPauseBuild)
}

// pausedLeaseList filters one lease-row listing down to owner's paused
// lease candidates (so a pass reads the catalog once, not per owner):
// one per lease, sized by its lease's pause build's recorded size_bytes
// (the number the disk usage accounting reads, so the victim list and
// the owner's Used can never disagree). A lease with no pause build row
// is skipped — its bytes are gone or unknown. Pinned leases are carried
// so the selector can refuse them explicitly.
func (s *Service) pausedLeaseList(rows []store.LeaseRow, owner string, byPauseBuild map[string]store.BuildRow) []diskLease {
	var out []diskLease
	for _, r := range rows {
		if r.Owner != owner || r.State != "suspended" || r.ResumeBuildID == "" {
			continue
		}
		b, ok := byPauseBuild[r.ID]
		if !ok || b.SizeBytes <= 0 {
			continue
		}
		pausedAt := r.PausedAt
		if pausedAt.IsZero() {
			// A paused lease always has a pause date (FS5 one clock); a
			// row without one orders last, by its creation.
			pausedAt = r.CreatedAt
		}
		out = append(out, diskLease{ID: r.ID, Bytes: b.SizeBytes, Pinned: r.Pinned, PausedAt: pausedAt})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PausedAt.Before(out[j].PausedAt) })
	return out
}

// leasesOnce lists the lease catalog from the store: the take-back loop
// reads it once per pass and hands the rows to every owner's view, not
// once per owner.
func (s *Service) leasesOnce(ctx context.Context) []store.LeaseRow {
	rows, err := s.db.ListLeases(ctx)
	if err != nil {
		s.log.Printf("disk reclaim: list leases: %v", err)
		return nil
	}
	return rows
}

// diskRoomNow reads one room snapshot from the live box: statfs total
// and free, the running leases' whole disk_mb as the reservation, the
// floor from DISK_RESERVE_PCT, and the inflight pending/freeing
// tracking. ok is false when the statfs read fails or the total is 0.
func (s *Service) diskRoomNow(ctx context.Context) (diskRoom, bool) {
	if s.cfg.TemplateStoragePath == "" {
		return diskRoom{}, false
	}
	total, free, err := s.diskCapacity(s.cfg.TemplateStoragePath)
	if err != nil || total <= 0 {
		return diskRoom{}, false
	}
	var reserved int64
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.released || !l.live() {
			continue
		}
		reserved += int64(l.DiskMB) * 1024 * 1024
	}
	s.store.mu.Unlock()
	pending, freeing := s.diskInflight.settle(s.now(), int64(free))
	return computeDiskRoom(int64(total), int64(free), reserved, pending, freeing, diskReservePct()), true
}

// noteDiskFreeing records n bytes just deleted while statfs free read
// freeAtNote, so the room formula credits them until ZFS catches up (or
// 30 s pass). n of 0 is a no-op: garbage passes that freed nothing (a
// dry-run or off reap, an empty box) add no freeing entry.
func (s *Service) noteDiskFreeing(n, freeAtNote int64) {
	if n <= 0 {
		return
	}
	s.diskInflight.noteFreeing(n, s.now(), freeAtNote)
}

// emitDiskCleanup emits the one disk.cleanup event per take-back call:
// bytes freed by the garbage pass and by reclaimed leases, each named.
func (s *Service) emitDiskCleanup(garbage, leases int64) {
	s.emitLeaseEvent("", "", LeaseDiskCleanup,
		fmt.Sprintf("disk take-back freed %s of garbage, %s of paused leases",
			formatEventBytes(garbage), formatEventBytes(leases)))
}
