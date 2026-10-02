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

	"github.com/jrimmer/spoond/store"
)

// Snapshot catalog GC, disk accounting and the snapshot API (U11).
//
// Builds form chains (parent_build_id) and their headers reference other
// builds' blocks (build_refs, from E2B's scheduling metadata). The GC
// computes a root set — every image's current build, every live lease's
// resume/checkpoint builds, every live sandbox's build, every in-flight
// build — keeps the closure of that set, and deletes (dry-run by
// default) ready/failed builds older than an hour that fall outside it.
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
// (dry-run unless GC_DELETE=1) and the hourly disk accounting.
func (s *Service) gcOnce(ctx context.Context) error {
	// The drain pauses and resumes every lease; a GC pass racing it
	// could delete builds mid-rotation (U10).
	if s.draining.Load() {
		return nil
	}
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
// build, every live lease's resume and checkpoint builds, every live
// sandbox's build (pool sandboxes included), and every in-flight build.
// Every root's ancestor chain is kept in full, and every kept build's
// header-referenced builds (build_refs) are kept in full — including
// their own ancestors and refs, transitively.
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
	for _, l := range leases {
		if l.State == "lost" {
			continue
		}
		keep(l.ResumeBuildID)
		keep(l.LastCheckpointBuildID)
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

// accountDisk measures every non-deleted build's directory (allocated
// blocks × 512 per regular file) into size_bytes, and sets the
// snapshot/storage gauges. Runs with the GC, once an hour.
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
		size := buildDiskUsage(filepath.Join(s.cfg.TemplateStoragePath, b.BuildID))
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
// regular file under dir. A missing or unreadable directory counts as 0.
func buildDiskUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += int64(st.Blocks) * 512
		}
		return nil
	})
	return total
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
