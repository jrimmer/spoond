package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Snapshot catalog GC: orphan build directories.
//
// The catalog GC (gcCandidates) only walks builds rows, so it never sees
// two kinds of directory that pile up on the snapshot disk: a build
// spoond already marked deleted at creation (a failed or abandoned pause
// or checkpoint whose memory file the orchestrator finishes writing
// seconds later, so the directory reappears after the delete), and a
// directory spoond never recorded at all (an image build's intermediate
// layers, or a build whose catalog insert failed).
//
// After the catalog candidates, a GC pass therefore sweeps the direct
// child directories of E2B_TEMPLATE_STORAGE_PATH itself. A directory is
// needed, and left alone, while any of these holds:
//
//   - its id is a catalog build that is not `deleted`;
//   - its id is named by any lease (resume_build_id,
//     last_checkpoint_build_id), any kept build (lease_kept_builds), any
//     sandbox row or any image's current build;
//   - its id is reachable from a needed build through that build's
//     memfile.header / rootfs.ext4.header, transitively — a header
//     records the build ids it maps pages from as 16 raw UUID bytes, and
//     matching those bytes (or the textual id) in the header file is
//     enough, as the manual cleanup did;
//   - it was modified within ORPHAN_MIN_AGE_SECS (default 3600): it may
//     be in use or still being written.
//
// Everything else is an orphan. What happens to one is ORPHAN_REAP:
//
//   - off        — no reaping at all;
//   - dryrun     — log what would happen, change nothing (the default);
//   - quarantine — move the orphan to <storage path>/../quarantine/<id>
//     with a marker, restore a quarantined directory the moment a later
//     pass needs it again, and delete it only after it has sat in
//     quarantine for ORPHAN_QUARANTINE_SECS (default 86400).
//
// Quarantine is the safe middle ground: a misclassification is
// recoverable for a day, and GC_DELETE never governs this path.

// defaultOrphanMinAge is how recently an unneeded directory must have
// changed to be spared the orphan reap (ORPHAN_MIN_AGE_SECS).
const defaultOrphanMinAge = time.Hour

// defaultOrphanQuarantine is how long a quarantined orphan waits before
// it may be deleted (ORPHAN_QUARANTINE_SECS).
const defaultOrphanQuarantine = 24 * time.Hour

// orphanQuarantineMarker is the file quarantining drops in each
// quarantined directory; it records when the directory was quarantined,
// so the age survives a backend restart (the directory's own mtime would
// move with it and with any later write).
const orphanQuarantineMarker = ".spoond-quarantine"

// orphanReapMode is the ORPHAN_REAP policy.
type orphanReapMode string

const (
	orphanReapOff        orphanReapMode = "off"
	orphanReapDryRun     orphanReapMode = "dryrun"
	orphanReapQuarantine orphanReapMode = "quarantine"
)

// orphanReapModeFromEnv reads ORPHAN_REAP: off, dryrun or quarantine.
// An unset or unrecognised value is the safe dry-run default.
func orphanReapModeFromEnv() orphanReapMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ORPHAN_REAP"))) {
	case "off":
		return orphanReapOff
	case "quarantine":
		return orphanReapQuarantine
	default:
		return orphanReapDryRun
	}
}

// orphanMinAge reads ORPHAN_MIN_AGE_SECS, falling back to
// defaultOrphanMinAge for an unset, non-numeric or non-positive value.
func orphanMinAge() time.Duration {
	if v := os.Getenv("ORPHAN_MIN_AGE_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return defaultOrphanMinAge
}

// orphanQuarantineAge reads ORPHAN_QUARANTINE_SECS, falling back to
// defaultOrphanQuarantine for an unset, non-numeric or non-positive
// value.
func orphanQuarantineAge() time.Duration {
	if v := os.Getenv("ORPHAN_QUARANTINE_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return defaultOrphanQuarantine
}

// orphanDir is one direct child directory of the storage (or quarantine)
// path whose name parses as a UUID. Symlinks and non-UUID names are never
// listed, so the reap can never follow a link out of the storage path or
// touch an unrelated file.
type orphanDir struct {
	id      uuid.UUID
	name    string
	path    string
	modtime time.Time
}

// listOrphanDirs reads root's direct entries and returns the real
// directories named by a UUID (the only things the reap may touch).
func listOrphanDirs(root string) ([]orphanDir, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	var out []orphanDir
	for _, e := range entries {
		// A symlink is skipped explicitly: IsDir is false for one, so a
		// link to a directory elsewhere is never followed or removed.
		if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		id, err := uuid.Parse(e.Name())
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, orphanDir{
			id:      id,
			name:    e.Name(),
			path:    filepath.Join(root, e.Name()),
			modtime: info.ModTime(),
		})
	}
	return out, nil
}

// orphanRoots computes the directly needed build ids from the catalog:
// every non-deleted build row, every image's current build, every
// lease's resume and last-checkpoint builds, every kept build, and every
// sandbox's build. A read failure fails the whole walk, and the caller
// refuses to reap anything (rather than treating an unreadable catalog as
// "nothing is needed").
func (s *Service) orphanRoots(ctx context.Context) (map[string]bool, error) {
	needed := map[string]bool{}
	builds, err := s.db.ListBuilds(ctx)
	if err != nil {
		return nil, fmt.Errorf("list builds: %w", err)
	}
	// Every catalog build that is not deleted is needed: the catalog GC
	// already decides which of them may be deleted, and the orphan reap
	// must not race or duplicate that decision.
	for _, b := range builds {
		if b.State != "deleted" {
			needed[b.BuildID] = true
		}
	}
	imgs, err := s.db.ListImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	for _, img := range imgs {
		needed[img.CurrentBuildID] = true
	}
	leases, err := s.db.ListLeases(ctx)
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	for _, l := range leases {
		needed[l.ResumeBuildID] = true
		needed[l.LastCheckpointBuildID] = true
		// A live lease started from a named snapshot needs its version's
		// build even after a forced row delete (2.7, #83).
		needed[l.SnapshotBuildID] = true
	}
	keptBy, err := s.db.ListKeptBuilds(ctx)
	if err != nil {
		return nil, fmt.Errorf("list kept builds: %w", err)
	}
	for _, ids := range keptBy {
		for _, id := range ids {
			needed[id] = true
		}
	}
	// Named snapshots (2.7, #83): every version's build is an orphan root,
	// plus any build a lease start is using right now (B1).
	named, err := s.db.NamedSnapshotBuilds(ctx)
	if err != nil {
		return nil, fmt.Errorf("list named snapshot builds: %w", err)
	}
	for id := range named {
		needed[id] = true
	}
	for id := range s.startingBuildSet() {
		needed[id] = true
	}
	sbs, err := s.db.ListSandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	for _, sb := range sbs {
		needed[sb.BuildID] = true
	}
	delete(needed, "")
	return needed, nil
}

// readBuildHeaders returns the concatenated bytes of dir's memfile.header
// and rootfs.ext4.header; a missing file contributes nothing.
func readBuildHeaders(dir string) []byte {
	var out []byte
	for _, name := range []string{"memfile.header", "rootfs.ext4.header"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			out = append(out, b...)
		}
	}
	return out
}

// headerReferences reports whether a build header mentions d: its 16 raw
// UUID bytes or its canonical (lower-case, dashed) textual id. That is
// what the manual cleanup matched, and it needs no header parser (the
// format is versioned and LZ4-compressed from V4 on).
func headerReferences(data []byte, d orphanDir) bool {
	if bytes.Contains(data, d.id[:]) {
		return true
	}
	return bytes.Contains(data, []byte(d.id.String()))
}

// orphanHeaderClosure expands needed with the directories reachable from
// a needed build through its headers, transitively: each newly needed
// directory's own headers are read in turn. The walk covers the storage
// directories and any quarantined ones, so a directory a needed build
// still references is known to be needed before the restore runs.
func (s *Service) orphanHeaderClosure(dirs []orphanDir, needed map[string]bool) {
	queue := make([]orphanDir, 0, len(dirs))
	for _, d := range dirs {
		if needed[d.name] {
			queue = append(queue, d)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		data := readBuildHeaders(cur.path)
		if len(data) == 0 {
			continue
		}
		for _, d := range dirs {
			if needed[d.name] {
				continue
			}
			if headerReferences(data, d) {
				needed[d.name] = true
				queue = append(queue, d)
			}
		}
	}
}

// quarantineDir is where quarantined orphans live: a sibling of the
// storage path, so a move never crosses a filesystem.
func (s *Service) quarantineDir() string {
	return filepath.Join(filepath.Dir(s.cfg.TemplateStoragePath), "quarantine")
}

// quarantineTime returns when a quarantined directory was quarantined:
// the marker file when it parses, the directory's mtime otherwise (a
// missing marker on an old directory waits out the full period from its
// last write instead of being deleted at once).
func quarantineTime(path string, fallback time.Time) time.Time {
	b, err := os.ReadFile(filepath.Join(path, orphanQuarantineMarker))
	if err != nil {
		return fallback
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err == nil {
		return t
	}
	return fallback
}

// writeQuarantineMarker records the quarantine instant in the directory
// and stamps the directory's own mtime to match, so the age is
// recoverable even if the marker is lost.
func writeQuarantineMarker(dir string, at time.Time) error {
	path := filepath.Join(dir, orphanQuarantineMarker)
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
		return err
	}
	return os.Chtimes(dir, at, at)
}

// reapOrphans applies the ORPHAN_REAP policy to every direct child build
// directory under the template storage path. It returns how many
// directories it deleted and how many bytes they held, for the pass's gc
// event. It never returns an error: a catalog or storage-path read
// failure keeps the pass green (the catalog GC's own error handling
// stands) but refuses the reap, logging why.
func (s *Service) reapOrphans(ctx context.Context) (reaped int, freed int64) {
	root := s.cfg.TemplateStoragePath
	if root == "" {
		return 0, 0
	}
	mode := orphanReapModeFromEnv()
	if mode == orphanReapOff {
		return 0, 0
	}
	dirs, err := listOrphanDirs(root)
	if err != nil {
		s.log.Printf("gc: orphan reap skipped: %v", err)
		return 0, 0
	}
	qroot := s.quarantineDir()
	quarantined, err := listOrphanDirs(qroot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Printf("gc: orphan reap skipped: %v", err)
		return 0, 0
	}
	if errors.Is(err, os.ErrNotExist) {
		quarantined = nil
	}
	if len(dirs) == 0 && len(quarantined) == 0 {
		return 0, 0
	}
	needed, err := s.orphanRoots(ctx)
	if err != nil {
		s.log.Printf("gc: orphan reap skipped: %v", err)
		return 0, 0
	}
	if len(needed) == 0 {
		// No catalog row names a build at all: the root set is
		// untrustworthy, so nothing is moved, restored or deleted this
		// pass.
		s.log.Printf("gc: orphan reap skipped: no needed builds in the catalog")
		return 0, 0
	}
	// A directory modified within the minimum age may be in use or still
	// being written; it seeds the header walk but is never touched.
	now := time.Now()
	minAge := orphanMinAge()
	for _, d := range dirs {
		if now.Sub(d.modtime) < minAge {
			needed[d.name] = true
		}
	}
	all := make([]orphanDir, 0, len(dirs)+len(quarantined))
	all = append(all, dirs...)
	all = append(all, quarantined...)
	s.orphanHeaderClosure(all, needed)

	// Restore any quarantined directory a later pass needs again, before
	// considering the rest for deletion.
	if mode == orphanReapQuarantine {
		for _, d := range quarantined {
			if !needed[d.name] {
				continue
			}
			dest := filepath.Join(root, d.name)
			if err := os.Rename(d.path, dest); err != nil {
				s.log.Printf("gc: restore quarantined %s: %v", d.name, err)
				continue
			}
			_ = os.Remove(filepath.Join(dest, orphanQuarantineMarker))
			s.log.Printf("gc: restored quarantined build %s", d.name)
		}
	}

	// Quarantine (or, in dry run, log) each unneeded storage directory.
	for _, d := range dirs {
		if needed[d.name] {
			continue
		}
		size, _ := s.diskUsage(d.path)
		if mode == orphanReapDryRun {
			s.log.Printf("gc: would reap orphan %s (%s)", d.name, formatEventBytes(size))
			continue
		}
		if err := s.quarantineOrphan(qroot, d, now); err != nil {
			s.log.Printf("gc: quarantine %s: %v", d.name, err)
			continue
		}
		s.log.Printf("gc: quarantined orphan %s (%s)", d.name, formatEventBytes(size))
	}

	// Delete or log quarantined directories that have waited out the
	// quarantine period.
	qage := orphanQuarantineAge()
	for _, d := range quarantined {
		if needed[d.name] {
			continue
		}
		at := quarantineTime(d.path, d.modtime)
		if now.Sub(at) < qage {
			continue
		}
		size, _ := s.diskUsage(d.path)
		if mode == orphanReapDryRun {
			s.log.Printf("gc: would reap quarantined orphan %s (%s)", d.name, formatEventBytes(size))
			continue
		}
		if err := os.RemoveAll(d.path); err != nil {
			s.log.Printf("gc: reap quarantined orphan %s: %v", d.name, err)
			continue
		}
		s.log.Printf("gc: reaped quarantined orphan %s (%s)", d.name, formatEventBytes(size))
		if s.metrics != nil {
			s.metrics.GCOrphansReaped.Inc()
			s.metrics.GCOrphanBytesReaped.Add(float64(size))
		}
		reaped++
		freed += size
	}
	return reaped, freed
}

// quarantineOrphan moves d under qroot and drops the marker that dates
// the quarantine. It is a rename within the storage sibling, so it is
// atomic and never copies bytes the delete would then have to find.
func (s *Service) quarantineOrphan(qroot string, d orphanDir, now time.Time) error {
	if err := os.MkdirAll(qroot, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(qroot, d.name)
	if err := os.Rename(d.path, dest); err != nil {
		return err
	}
	if err := writeQuarantineMarker(dest, now); err != nil {
		return err
	}
	return nil
}
