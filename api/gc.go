package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jrimmer/spoond/v2/store"
)

// Snapshot catalog GC, disk accounting and the snapshot API (U11).
//
// Builds form chains (parent_build_id) and their headers reference other
// builds' blocks (build_refs, from E2B's scheduling metadata). The GC
// computes a root set — every image's current build, every live lease's
// resume/checkpoint builds, every kept build of a live lease (2.3,
// #121), every live sandbox's build, every in-flight build — keeps the
// closure of that set, and deletes (dry-run by default) ready/failed
// builds older than an hour that fall outside it.
// A lease lost in a substrate crash keeps its resume/checkpoint builds
// for a grace period after the loss (7 d persistent, 1 d otherwise)
// before they may be reclaimed (owner decision 2026-10-02). A lease
// that was already lost when lost_at began to be recorded, so the
// column is empty, has its lost_at stamped by the first GC pass that
// sees it: the grace period then starts once instead of restarting on
// every pass.
//
// The selection rule: a build is never a candidate while it is the
// parent of any non-deleted build, or a ref_build_id in build_refs of a
// kept build. Both protections are structural — they do not depend on
// any root row still naming the build — so a lease whose lease/sandbox
// rows lag (a crash between a checkpoint and its sandbox upsert, or a
// drain mid-rotation) can never have the layers under its live builds
// removed.

// gcAge is how long a build must be unreferenced and idle before the GC
// considers it.
const gcAge = time.Hour

// The lost-lease snapshot grace periods (owner decision 2026-10-02): a
// lost lease's resume_build_id and last_checkpoint_build_id stay kept
// roots for 7 days after lost_at when the lease is persistent, 1 day
// otherwise. Overridable per service via ServiceConfig.
const (
	defaultLostGracePersistent = 7 * 24 * time.Hour
	defaultLostGrace           = 24 * time.Hour
)

// lostGraces resolves the configured grace periods, substituting the
// defaults for zero values.
func (s *Service) lostGraces() (persistent, other time.Duration) {
	p, o := s.cfg.LostGracePersistent, s.cfg.LostGrace
	if p <= 0 {
		p = defaultLostGracePersistent
	}
	if o <= 0 {
		o = defaultLostGrace
	}
	return p, o
}

// lostGrace returns one lease's grace period: 7 days for a persistent
// lease, 1 day for any other, from the configured (or default) values.
func (s *Service) lostGrace(l store.LeaseRow) time.Duration {
	p, o := s.lostGraces()
	if l.Persistent {
		return p
	}
	return o
}

// lostKeepUntil is the instant from which a lost lease's snapshots may
// be reclaimed: lost_at plus its grace period. A lease still carrying
// an empty lost_at, lost before the column was recorded, counts as
// lost at the given now — the GC stamps such leases on sight (see
// stampLostAt), so this fallback covers only a row the stamp has not
// reached yet.
func (s *Service) lostKeepUntil(l store.LeaseRow, now time.Time) time.Time {
	lostAt := l.LostAt
	if lostAt.IsZero() {
		lostAt = now
	}
	return lostAt.Add(s.lostGrace(l))
}

// stampLostAt records the loss time of a lease that was already lost
// when lost_at began to be recorded (its column is empty): the first GC
// pass that sees it counts as the moment of loss, so the grace period
// has a fixed start instead of moving forward on every pass and
// holding the snapshots forever. It takes the same path setState uses
// when the lease is in memory and falls back to the stored row
// otherwise; an existing stamp is never overwritten, and a lease that
// has since left the lost state is left alone (leaving "lost" clears
// the stamp).
func (s *Service) stampLostAt(ctx context.Context, l store.LeaseRow, now time.Time) {
	s.store.mu.Lock()
	if mem, ok := s.store.leases[l.ID]; ok {
		if mem.State != "lost" || !mem.LostAt.IsZero() {
			s.store.mu.Unlock()
			return
		}
		// The state is already "lost", so this is only the entering-
		// "lost" half of setState, stamped with this pass's clock.
		mem.LostAt = now
		s.saveLeaseLocked(mem)
		s.store.mu.Unlock()
		return
	}
	s.store.mu.Unlock()
	l.LostAt = now
	if err := s.db.UpsertLease(ctx, l); err != nil {
		s.storeError("upsert_lease", l.ID, err)
	}
}

// runGCCatalogLoop runs the GC once 10 minutes after the backend starts
// and then once an hour. Disk accounting runs with it. Never while the
// admin drain is running (U10).
func (s *Service) runGCCatalogLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Minute):
	}
	s.gcOnce(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.gcOnce(ctx)
		}
	}
}

// gcOnce runs one GC pass: the kept-set closure, the candidate deletes
// (dry-run unless GC_DELETE=1) and the hourly disk accounting. The pass's
// outcome is recorded for the notify checks (gc.failed, 2.2 #117); a
// drain-skipped pass records nothing — it did not run, so it did not fail.
func (s *Service) gcOnce(ctx context.Context) error {
	// The drain pauses and resumes every lease; a GC pass racing it
	// could delete builds mid-rotation (U10).
	if s.draining.Load() {
		return nil
	}
	err := s.gcPass(ctx)
	s.recordGCOutcome(err)
	return err
}

// gcPass is gcOnce's body: one full pass.
func (s *Service) gcPass(ctx context.Context) error {
	kept, err := s.keptBuilds(ctx)
	if err != nil {
		return err
	}
	if err := s.gcCandidates(ctx, kept); err != nil {
		return err
	}
	return s.accountDisk(ctx)
}

// keptBuilds computes the GC keep set. Roots are every image's current
// build, every live lease's resume and checkpoint builds, every kept
// build of a live lease (2.3, #121), every live sandbox's build (pool
// sandboxes included), and every in-flight build. A lost lease's resume
// and checkpoint builds stay roots for a grace period after the loss —
// 7 days for a persistent lease, 1 day otherwise — so its snapshots
// outlive the crash that lost it. Every root's ancestor chain is kept
// in full, and every kept build's header-referenced builds (build_refs)
// are kept in full — including their own ancestors and refs,
// transitively.
//
// The chain walk is over *non-deleted* builds only: a deleted build
// keeps nothing, so the files a GC pass or an owner delete already
// removed cannot hold live builds' layers hostage. Ref ids absent from
// builds are still kept (they are never candidates: only builds rows
// are).
func (s *Service) keptBuilds(ctx context.Context) (map[string]bool, error) {
	builds, err := s.db.ListBuilds(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: list builds: %w", err)
	}
	parent := make(map[string]string, len(builds))
	deleted := make(map[string]bool, len(builds))
	for _, b := range builds {
		parent[b.BuildID] = b.ParentBuildID
		if b.State == "deleted" {
			deleted[b.BuildID] = true
		}
	}
	refs, err := s.db.ListBuildRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: list build refs: %w", err)
	}
	kept := make(map[string]bool, len(builds))
	// keep adds id and, transitively, its ancestors (up to the first
	// deleted or unknown build) and its header-referenced builds (each
	// with the same treatment). It is cycle-safe.
	var keep func(id string)
	keep = func(id string) {
		for {
			if id == "" || kept[id] {
				return
			}
			kept[id] = true
			for _, ref := range refs[id] {
				keep(ref)
			}
			if deleted[id] {
				return // a deleted build keeps nothing above it
			}
			id = parent[id]
		}
	}
	for _, b := range builds {
		if b.State == "building" {
			keep(b.BuildID)
		}
		if b.State == "deleted" {
			continue // its files are gone; it protects nothing
		}
		// The structural roots: a build is kept while any non-deleted
		// build names it as a parent, and while any non-deleted build's
		// headers reference its blocks. These do not depend on an image,
		// lease or sandbox row naming the build, so they hold even when
		// those rows lag or were dropped.
		keep(b.ParentBuildID)
		for _, ref := range refs[b.BuildID] {
			keep(ref)
		}
	}
	imgs, err := s.db.ListImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: list images: %w", err)
	}
	for _, img := range imgs {
		keep(img.CurrentBuildID)
	}
	leases, err := s.db.ListLeases(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: list leases: %w", err)
	}
	keptBy, err := s.db.ListKeptBuilds(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: list kept builds: %w", err)
	}
	now := s.now()
	for _, l := range leases {
		if l.State == "lost" {
			// A lease lost before lost_at was recorded counts as lost at
			// this pass and only this one: the stamp fixes the start of
			// its grace period for every later pass.
			if l.LostAt.IsZero() {
				s.stampLostAt(ctx, l, now)
				l.LostAt = now
			}
			// A lost lease keeps its snapshots for a grace period after
			// the loss, so the owner can still reclaim them; past it the
			// builds are candidates like any other unreferenced build.
			// The lease's kept-builds rows go with the grace period too:
			// only a release deletes them otherwise, and a lease stuck in
			// lost is never released by its owner.
			if !now.Before(s.lostKeepUntil(l, now)) {
				if len(keptBy[l.ID]) > 0 {
					if err := s.db.DeleteKeptBuilds(ctx, l.ID); err != nil {
						s.log.Printf("gc: delete kept builds of lost lease %s: %v", l.ID, err)
					}
				}
				continue
			}
		}
		keep(l.ResumeBuildID)
		keep(l.LastCheckpointBuildID)
		// The builds the lease pinned with {"keep":true} are roots while
		// the lease lives (2.3, #121); releasing the lease drops its kept
		// rows, and a lost lease's keeps lapse with its grace period.
		for _, b := range keptBy[l.ID] {
			keep(b)
		}
	}
	sbs, err := s.db.ListSandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: list sandboxes: %w", err)
	}
	for _, sb := range sbs {
		keep(sb.BuildID)
	}
	return kept, nil
}

// gcCandidates deletes (GC_DELETE=1) or logs (dry-run, the default)
// every ready/failed build outside kept with an updated_at older than
// gcAge.
func (s *Service) gcCandidates(ctx context.Context, kept map[string]bool) error {
	builds, err := s.db.ListBuilds(ctx)
	if err != nil {
		return fmt.Errorf("gc: list builds: %w", err)
	}
	cutoff := time.Now().Add(-gcAge)
	delete := os.Getenv("GC_DELETE") == "1"
	for _, b := range builds {
		if b.State != "ready" && b.State != "failed" {
			continue
		}
		if kept[b.BuildID] || b.UpdatedAt.After(cutoff) {
			continue
		}
		if !delete {
			s.log.Printf("gc: would delete %s kind=%s image=%s", b.BuildID, b.Kind, b.Image)
			continue
		}
		if err := s.sub.DeleteBuild(ctx, b.TemplateID, b.BuildID); err != nil {
			s.log.Printf("gc: delete %s: %v", b.BuildID, err)
			continue
		}
		if err := s.db.UpdateBuildState(ctx, b.BuildID, "deleted", "", nil); err != nil {
			s.log.Printf("gc: mark %s deleted: %v", b.BuildID, err)
			continue
		}
		if s.metrics != nil {
			s.metrics.GCDeleted.WithLabelValues(b.Kind).Inc()
		}
	}
	return nil
}

// buildSizeOnDisk measures one build's directory under the template
// storage root (allocated blocks × 512 per regular file): the per-build
// half of the hourly disk accounting and of the write-time recording a
// fresh build's row gets (#125, measureNewBuildOnDisk). A missing or
// unreadable directory measures 0; the error says which, so the
// write-time path can tell a real failure from an empty build.
func (s *Service) buildSizeOnDisk(buildID string) (int64, error) {
	return buildDiskUsage(filepath.Join(s.cfg.TemplateStoragePath, buildID))
}

// measureNewBuildOnDisk is the Service's write-time measurer (#125):
// the substrate has just written buildID's files, so the size goes into
// the fresh build's row — /api/snapshots then shows a size immediately,
// not only after the next hourly accounting pass. A build directory the
// substrate never wrote or that cannot be read logs and measures 0; the
// hourly pass corrects the row once it can read the files. A readable
// but empty directory measures 0 without a complaint.
func (s *Service) measureNewBuildOnDisk(buildID string) int64 {
	size, err := s.buildSizeOnDisk(buildID)
	if err != nil {
		s.log.Printf("build size: %s: %v; storing 0 until the hourly pass", buildID, err)
	}
	return size
}

// accountDisk measures every non-deleted build's directory (allocated
// blocks × 512 per regular file) into size_bytes, and sets the
// snapshot/storage gauges. Runs with the GC, once an hour; it also
// re-measures builds whose write-time recording (#125) measured 0
// because the files were missing or unreadable at write time.
func (s *Service) accountDisk(ctx context.Context) error {
	builds, err := s.db.ListBuilds(ctx)
	if err != nil {
		return fmt.Errorf("disk accounting: list builds: %w", err)
	}
	perKind := make(map[string]int64, 3)
	for _, b := range builds {
		if b.State == "deleted" {
			continue
		}
		size, _ := s.buildSizeOnDisk(b.BuildID)
		perKind[b.Kind] += size
		if size != b.SizeBytes {
			if err := s.db.UpdateBuildSize(ctx, b.BuildID, size); err != nil {
				s.log.Printf("disk accounting: %s: %v", b.BuildID, err)
			}
		}
	}
	if s.metrics != nil {
		for _, kind := range []string{"template", "pause", "checkpoint"} {
			s.metrics.SnapshotBytes.WithLabelValues(kind).Set(float64(perKind[kind]))
		}
		s.metrics.StorageFree.Set(float64(storageFreeBytes(s.cfg.TemplateStoragePath)))
	}
	return nil
}

// buildDiskUsage sums the allocated size (stat blocks × 512) of every
// regular file under dir. A missing directory measures 0 with no error
// (an unwritten build is empty, not failed); any read or stat failure —
// the root itself or one file's Info — is returned, so callers can tell
// a failed measurement from a legitimately 0-byte build.
func buildDiskUsage(dir string) (int64, error) {
	var total int64
	var firstErr error
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			switch {
			case path == dir && os.IsNotExist(err):
			case firstErr == nil:
				firstErr = fmt.Errorf("stat %s: %w", path, err)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("stat %s: %w", path, err)
			}
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += int64(st.Blocks) * 512
		}
		return nil
	})
	return total, firstErr
}

// storageFreeBytes reports the statfs free bytes of dir; 0 when statfs
// fails.
func storageFreeBytes(dir string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}

// statfsCapacity reports dir's total (f_blocks × f_bsize) and free
// (f_bavail × f_bsize) bytes for the held-lease pressure and critical
// rules; an unreadable path returns an error and the rules stay off.
func statfsCapacity(dir string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return st.Blocks * bsize, st.Bavail * bsize, nil
}

// handleSnapshots lists the caller's non-deleted builds (U11).
func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	kept, err := s.svc.keptBuilds(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	builds, err := s.svc.db.ListBuilds(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, b := range builds {
		if b.Owner != owner || b.State == "deleted" {
			continue
		}
		out = append(out, map[string]any{
			"build_id":        b.BuildID,
			"kind":            b.Kind,
			"image":           b.Image,
			"parent_build_id": b.ParentBuildID,
			"size_bytes":      b.SizeBytes,
			"created_at":      formatRFC3339(b.CreatedAt),
			"in_use":          kept[b.BuildID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

// handleSnapshotDelete deletes one of the caller's snapshot builds
// (U11). Checks in order: unknown or deleted → 404; a template build →
// 403 (templates have no owner, so this precedes the owner check);
// another owner's → 404; referenced by the kept set → 409; otherwise
// the build's files go and it is marked deleted → 204.
func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("build_id")
	b, err := s.svc.db.GetBuild(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b.State == "deleted" {
		writeError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	if b.Kind == "template" {
		writeError(w, http.StatusForbidden, "template builds are not snapshots")
		return
	}
	if b.Owner != owner {
		writeError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	// DELETE /api/snapshots/{build_id} also unpins a kept build (2.3,
	// #121): the owner deleting the snapshot by id counts as releasing
	// the pin, so a kept build can be removed ahead of the lease's own
	// release. The rows go first, so a pinned build deletes instead of
	// answering 409 forever.
	if err := s.svc.db.UnkeepBuildAny(r.Context(), b.BuildID); err != nil {
		s.svc.log.Printf("snapshot: unkeep %s: %v", b.BuildID, err)
	}
	kept, err := s.svc.keptBuilds(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if kept[b.BuildID] {
		writeError(w, http.StatusConflict, "snapshot in use")
		return
	}
	if err := s.svc.sub.DeleteBuild(r.Context(), b.TemplateID, b.BuildID); err != nil {
		s.svc.log.Printf("snapshot: delete %s: %v", b.BuildID, err)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if err := s.svc.db.UpdateBuildState(r.Context(), b.BuildID, "deleted", "", nil); err != nil {
		s.svc.log.Printf("snapshot: mark %s deleted: %v", b.BuildID, err)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
