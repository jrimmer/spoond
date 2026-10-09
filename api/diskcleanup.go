package api

import (
	"context"
	"fmt"
	"time"
)

// Proactive disk cleanup (#145 D5, owner refinement 2026-10-08): spoond
// should not run itself out of disk because it was never aggressive
// enough about cleaning up. Below DISK_CLEAN_START_PCT free, each sweep
// tick reclaims spoond's own garbage first, in this order, until free
// space is above DISK_CLEAN_STOP_PCT:
//
//  1. orphan build/snapshot directories with no store row (the existing
//     GC; GC_DELETE still guards the catalog deletes);
//  2. leftovers of released/lost leases (unreferenced pause/checkpoint
//     builds the catalog still holds);
//  3. expired kept checkpoints past their TTL (their pins are dropped so
//     the GC may reclaim the builds in this same tick);
//  4. unreferenced template builds.
//
// None of these is owner data that is still live: every category is
// either a directory the catalog does not know or a build no live lease
// or image can reach. The tier runs before the critical-disk FIFO, which
// stays the last resort below CRITICAL_DISK_FREE_PCT. A tick that frees
// anything emits one `disk.cleanup` event naming the bytes per category.

// defaultDiskCleanStartPct / defaultDiskCleanStopPct are the proactive
// tier's thresholds when DISK_CLEAN_START_PCT / DISK_CLEAN_STOP_PCT are
// unset: start reclaiming at 20 % free, stop once 25 % is free.
const (
	DefaultDiskCleanStartPct = 20
	DefaultDiskCleanStopPct  = 25
)

// DefaultKeptCheckpointTTL is how long a kept checkpoint stays pinned
// before the proactive tier may expire it (KEPT_CHECKPOINT_TTL_SECS):
// seven days, matching the held-lease stale limit — long enough for a
// restore point to be useful, short enough that a keep-happy loop cannot
// pin the catalog forever.
const DefaultKeptCheckpointTTL = 7 * 24 * time.Hour

// diskCleanupTick is one sweep tick's proactive disk cleanup: below
// DISK_CLEAN_START_PCT free it reclaims spoond's own garbage and emits
// one disk.cleanup event with the bytes freed per category. It never
// touches a live lease; callers run it before the critical-disk FIFO.
func (s *Service) diskCleanupTick(ctx context.Context, now time.Time) {
	start := s.cfg.DiskCleanStartPct
	stop := s.cfg.DiskCleanStopPct
	if start <= 0 || s.cfg.TemplateStoragePath == "" {
		return
	}
	if stop < start {
		stop = start // a stop below the start never terminates
	}
	pct, ok := s.freePercent(s.cfg.TemplateStoragePath)
	if !ok || pct >= start {
		return
	}
	stats := s.reclaimSpoondGarbage(ctx, now)
	if !stats.nonEmpty() {
		return
	}
	s.log.Printf("disk cleanup: %.1f%% free < %.0f%%: orphans %s, released builds %s, kept checkpoints %s, template builds %s (stop at %.0f%%)",
		pct, start,
		formatEventBytes(stats.Orphans), formatEventBytes(stats.ReleasedBuilds),
		formatEventBytes(stats.KeptCheckpoints), formatEventBytes(stats.TemplateBuilds), stop)
	s.emitDiskCleanupEvent(fmt.Sprintf(
		"orphans %s · released builds %s · kept checkpoints %s · template builds %s · %s freed",
		formatEventBytes(stats.Orphans), formatEventBytes(stats.ReleasedBuilds),
		formatEventBytes(stats.KeptCheckpoints), formatEventBytes(stats.TemplateBuilds),
		formatEventBytes(stats.total())))
}

// reclaimSpoondGarbage reclaims the proactive tier's categories in one
// pass and returns the bytes each freed. It is best-effort: a failed
// catalog or storage read skips the category and reports nothing rather
// than failing the sweep. GC_DELETE still governs the catalog deletes
// (gcCandidates), and ORPHAN_REAP still governs the orphan reap.
func (s *Service) reclaimSpoondGarbage(ctx context.Context, now time.Time) gcStats {
	var stats gcStats
	// Fail stale building rows and drop long-deleted rows first, exactly
	// as a normal GC pass does: a stale building row would otherwise pin
	// its ancestor chain, and a long-deleted row is pure catalog litter.
	s.failStaleBuildingBuilds(ctx)
	s.pruneDeletedBuilds(ctx)
	// Release lost leases whose grace period has lapsed first: their
	// builds then leave the kept set and become ordinary candidates in
	// this same pass.
	s.releaseExpiredLostLeases(ctx)
	// Expire kept checkpoints past their TTL before the kept set is
	// computed, so their builds are candidates in this same tick. The
	// bytes are reported even when GC_DELETE is off: the pin (not the
	// file) is what expired.
	stats.KeptCheckpoints += s.expireKeptCheckpoints(ctx, now)
	kept, err := s.keptBuilds(ctx)
	if err != nil {
		s.log.Printf("disk cleanup: kept set: %v", err)
		return stats
	}
	// The catalog GC reclaims the leftovers of released/lost leases and
	// the unreferenced template builds; stats splits the two. Its own
	// gc event may also fire, which is unchanged maintenance.
	if _, _, err := s.gcCandidates(ctx, kept, &stats); err != nil {
		s.log.Printf("disk cleanup: catalog: %v", err)
	}
	// The orphan reap sweeps directories the catalog never sees. It is
	// governed by ORPHAN_REAP, not GC_DELETE, like any GC pass.
	_, orphanFreed := s.reapOrphans(ctx)
	stats.Orphans += orphanFreed
	return stats
}

// expireKeptCheckpoints drops every kept pin older than the configured
// kept-checkpoint TTL and returns the recorded bytes its builds hold, so
// the GC may reclaim them. 0 (or a negative value) disables the expiry.
// A pin whose build row is already gone counts nothing.
func (s *Service) expireKeptCheckpoints(ctx context.Context, now time.Time) int64 {
	ttl := s.cfg.KeptCheckpointTTL
	if ttl <= 0 {
		return 0
	}
	pins, err := s.db.ListKeptBuildPins(ctx)
	if err != nil {
		s.log.Printf("disk cleanup: list kept pins: %v", err)
		return 0
	}
	if len(pins) == 0 {
		return 0
	}
	sizes, err := s.buildSizeIndex(ctx)
	if err != nil {
		s.log.Printf("disk cleanup: build sizes: %v", err)
		return 0
	}
	var freed int64
	for _, p := range pins {
		if p.KeptAt.IsZero() || now.Sub(p.KeptAt) < ttl {
			continue
		}
		if err := s.db.DeleteKeptBuild(ctx, p.LeaseID, p.BuildID); err != nil {
			s.log.Printf("disk cleanup: expire kept %s: %v", p.BuildID, err)
			continue
		}
		freed += sizes[p.BuildID]
		s.log.Printf("disk cleanup: expired kept checkpoint %s of lease %s (kept at %s, ttl %s)",
			p.BuildID, p.LeaseID, p.KeptAt.Format(time.RFC3339), ttl)
	}
	return freed
}

// buildSizeIndex maps build id -> recorded size_bytes, for the disk
// cleanup's byte reporting.
func (s *Service) buildSizeIndex(ctx context.Context) (map[string]int64, error) {
	builds, err := s.db.ListBuilds(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(builds))
	for _, b := range builds {
		out[b.BuildID] = b.SizeBytes
	}
	return out, nil
}
