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
)

// diskRoom is one snapshot of how much disk a new request may use.
// All fields are bytes. Usable may be negative: that is the shortfall.
type diskRoom struct {
	Total    int64
	Free     int64
	Reserved int64 // bytes still to be written by running VMs
	Floor    int64 // reserve kept free (DISK_RESERVE_PCT of Total)
	Pending  int64 // admitted requests that have not written yet
	Freeing  int64 // deletions issued but not yet visible in statfs
	Usable   int64
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

// diskVictims picks paused leases to release so needBytes become free for
// requester. The owner with the highest disk ratio goes first, and only
// while that ratio stays above the requester's (used+need over slice) and
// above 1 (never inside its slice). Within an owner the oldest unpinned
// paused lease goes first; pinned leases never. Returns nil when the need
// cannot be met.
func diskVictims(owners []diskOwner, requester string, needBytes int64) []diskVictim {
	if needBytes <= 0 {
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
	for freed < needBytes {
		var best *state
		var bestRatio float64
		for _, st := range states {
			if len(st.leaps) == 0 || st.o.SliceBytes <= 0 || st.used <= st.o.SliceBytes {
				continue
			}
			r := diskRatio(st.used, st.o.SliceBytes)
			if r <= reqRatio {
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
	// TODO(FS3a step 3)
	return 0, errors.New("not implemented")
}
