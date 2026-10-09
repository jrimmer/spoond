package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v2/store"
)

// Named snapshots (2.7, #83): a checkpoint build with a stable name and a
// version, owned by an identity and outliving the lease it was saved
// from. Saving is POST /api/leases/{id}/snapshots; the owner manages the
// catalog through /api/named-snapshots. Start-from-snapshot (a lease
// create carrying "snapshot") is task 2 and lives in grantLease.

// DefaultMaxNamedSnapshots and DefaultSnapshotKeepVersions are the
// MAX_NAMED_SNAPSHOTS and SNAPSHOT_KEEP_VERSIONS defaults.
const (
	DefaultMaxNamedSnapshots    = 64
	DefaultSnapshotKeepVersions = 3
)

// namedSnapshotNameRe constrains a snapshot name: one or two parts
// separated by "/", each matching [a-z0-9][a-z0-9._-]{0,62}. Names are
// unique per owner; the prefix is a convention, not a project concept.
var namedSnapshotNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}(/[a-z0-9][a-z0-9._-]{0,62})?$`)

// keepBounds are the per-name retention bounds: the body's keep and PUT
// accept 1..20.
const (
	minSnapshotKeep = 1
	maxSnapshotKeep = 20
)

// validateNamedSnapshotName checks a save/show/delete name.
func validateNamedSnapshotName(name string) error {
	if !namedSnapshotNameRe.MatchString(name) {
		return fmt.Errorf("invalid snapshot name %q: must be [a-z0-9][a-z0-9._-]{0,62} optionally prefixed with <project>/", name)
	}
	return nil
}

// namedSnapshotError is one error answer on these routes: a
// machine-readable code next to the human message (A2).
type namedSnapshotError struct {
	status int
	code   string
	msg    string
	// retryAfter > 0 adds a Retry-After header (seconds).
	retryAfter int
}

func (e *namedSnapshotError) Error() string { return e.msg }

func (e *namedSnapshotError) write(w http.ResponseWriter) {
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	writeJSON(w, e.status, map[string]string{"error": e.msg, "code": e.code})
}

// The named-snapshot error constructors, one per A2 code.
func errSaveInProgress() *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusConflict, code: "save_in_progress", msg: "save in progress", retryAfter: 5}
}
func errSecretsInUse() *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusConflict, code: "secrets_in_use", msg: "a job with secrets is running"}
}
func errLeaseBusyNamed() *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusConflict, code: "lease_busy", msg: "lease is busy; retry"}
}
func errKeptBudget(msg string) *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusConflict, code: "kept_budget", msg: msg}
}
func errSnapshotLimit(msg string) *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusConflict, code: "snapshot_limit", msg: msg}
}
func errSnapshotInUse(msg string) *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusConflict, code: "snapshot_in_use", msg: msg}
}
func errNamedNotFound() *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusNotFound, code: "not_found", msg: "snapshot not found"}
}
func errLeaseNotFound() *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusNotFound, code: "not_found", msg: "lease not found"}
}
func errLeaseNotLive() *namedSnapshotError {
	return &namedSnapshotError{status: http.StatusConflict, code: "lease_not_live", msg: "lease is not running"}
}

// namedSaveState is the in-memory state of one (owner, name,
// idempotency_key) save: in flight, or failed for 24 h with its error.
// A failed save stores nothing in the catalog, so a replay with the same
// key runs a fresh save (A2). The state is lost on restart, when the key
// reads absent.
type namedSaveState struct {
	state    string // in_progress | failed
	code     string
	err      string
	failedAt time.Time
}

// failedSaveTTL is how long a failed save's in-memory record lives (A2):
// 24 h.
const failedSaveTTL = 24 * time.Hour

// namedSaveInFlight tracks in-flight saves and recent failures, keyed by
// owner\x00name\x00key.
type namedSaveInFlight struct {
	mu    sync.Mutex
	saves map[string]*namedSaveState
}

func (n *namedSaveInFlight) key(owner, name, key string) string {
	return owner + "\x00" + name + "\x00" + key
}

// begin claims the key. It returns false when a save is already in
// flight (the caller answers save_in_progress). A stale failed record is
// cleared on a new attempt, so a failed key that is retried runs a fresh
// save.
func (n *namedSaveInFlight) begin(owner, name, key string) bool {
	if key == "" {
		return true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.saves == nil {
		n.saves = map[string]*namedSaveState{}
	}
	n.pruneLocked()
	k := n.key(owner, name, key)
	st := n.saves[k]
	if st == nil {
		n.saves[k] = &namedSaveState{state: "in_progress"}
		return true
	}
	if st.state == "in_progress" {
		return false
	}
	// A failed attempt does not poison the key: allow a retry.
	n.saves[k] = &namedSaveState{state: "in_progress"}
	return true
}

// fail records a failed save for 24 h.
func (n *namedSaveInFlight) fail(owner, name, key, code, msg string) {
	if key == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneLocked()
	if n.saves == nil {
		n.saves = map[string]*namedSaveState{}
	}
	n.saves[n.key(owner, name, key)] = &namedSaveState{state: "failed", code: code, err: msg, failedAt: time.Now()}
}

// done drops an in-flight record once the save produced a version.
func (n *namedSaveInFlight) done(owner, name, key string) {
	if key == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if st := n.saves[n.key(owner, name, key)]; st != nil && st.state == "in_progress" {
		delete(n.saves, n.key(owner, name, key))
	}
}

// lookup answers the ?idempotency_key= route's state.
func (n *namedSaveInFlight) lookup(owner, name, key string) namedSaveState {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneLocked()
	if st := n.saves[n.key(owner, name, key)]; st != nil {
		return *st
	}
	return namedSaveState{state: "absent"}
}

func (n *namedSaveInFlight) pruneLocked() {
	for k, st := range n.saves {
		if st.state == "failed" && time.Since(st.failedAt) > failedSaveTTL {
			delete(n.saves, k)
		}
	}
}

// maxNamedSnapshots returns the configured per-owner name cap. 0 = no
// cap (MAX_NAMED_SNAPSHOTS); the backend default (64) comes from main.
func (s *Service) maxNamedSnapshots() int {
	return s.cfg.MaxNamedSnapshots
}

// snapshotKeepVersions returns the configured default retention, or the
// default.
func (s *Service) snapshotKeepVersions() int {
	if s.cfg.SnapshotKeepVersions > 0 {
		return s.cfg.SnapshotKeepVersions
	}
	return DefaultSnapshotKeepVersions
}

// namedSnapshotBuildRoot walks parent_build_id from the given build up to
// the template build at the root of the chain: the image build recorded
// on a named snapshot (spec: "walk parent_build_id to the template
// build").
func (s *Service) namedSnapshotBuildRoot(ctx context.Context, buildID string) (string, store.BuildRow, error) {
	b, err := s.db.GetBuild(ctx, buildID)
	if err != nil {
		return "", store.BuildRow{}, err
	}
	root := b
	seen := map[string]bool{}
	for root.ParentBuildID != "" && !seen[root.BuildID] {
		seen[root.BuildID] = true
		next, err := s.db.GetBuild(ctx, root.ParentBuildID)
		if err != nil {
			break
		}
		root = next
	}
	return root.BuildID, root, nil
}

// orchestratorVersion returns the substrate node's version, or "" when
// it cannot be read.
func (s *Service) orchestratorVersion(ctx context.Context) string {
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		return ""
	}
	return info.Version
}

// scrubSecretsForSnapshot removes every file under /run/secrets on the
// lease's sandbox through a guest exec, independent of what this
// backend process remembers (2.7, #83 B1): after a restart the in-memory
// create-time and exec-time names are gone while the guest's tmpfs still
// holds the files. It aborts with an error when the directory is not
// empty afterwards, so the caller never checkpoints a captured secret.
// It returns how many removed entries were not among known, so the
// caller can warn that this backend lost track of them (R4).
func (s *Service) scrubSecretsForSnapshot(ctx context.Context, l *Lease, known map[string]bool) (int, error) {
	return s.scrubAllSecrets(ctx, l.SandboxID, known)
}

// saveNamedSnapshot performs one save of a lease into a named snapshot:
// it runs the checkpoint and inserts the version row, applies
// retention, emits the event and writes the source-side marker. Callers
// own the busy window and the idempotency bookkeeping. Returns the
// inserted row.
func (s *Service) saveNamedSnapshot(ctx context.Context, l *Lease, name, key string, keep int) (store.NamedSnapshotRow, bool, *namedSnapshotError) {
	if keep <= 0 {
		keep = s.snapshotKeepVersions()
	}
	// The per-lease secrets gate: hold it from the secrets check through
	// the checkpoint and the create-time re-stage, so an exec or job that
	// stages secrets cannot slip its files into the checkpoint (B2). A
	// staging already in progress means there are secrets to capture:
	// refuse with secrets_in_use.
	if !s.secretsGate.beginSave(l.ID) {
		return store.NamedSnapshotRow{}, false, errSecretsInUse()
	}
	defer s.secretsGate.endSave(l.ID)
	// The create-time re-stage is registered right after the save claims
	// the gate and before any early return below (Q1a): every early return
	// — the names-cap 409, a scrub failure — must not leave the source
	// without its create-time secrets. The re-stage is idempotent and runs
	// before endSave (defers run LIFO), so it is the last thing the save
	// does under the gate.
	create := s.createSecretsFor(l.ID)
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), secretsStageTimeout)
		defer cancel()
		// A finishing job could not remove its exec-time files while this
		// save held the gate; remove them now, before restoring the
		// create-time secrets (Q1b). Any deferred name that is also a
		// create-time name is restored by the stage below.
		if pending := s.drainDeferredSecretRemovals(l.ID); len(pending) > 0 {
			s.removeSecrets(l.SandboxID, pending)
		}
		if len(create) == 0 {
			return
		}
		if err := s.stageSecrets(rctx, l.SandboxID, create); err != nil {
			s.log.Printf("snapshot: re-stage secrets on %s: %v", l.ID, err)
		}
	}()
	saveStart := s.now()
	// A running background job with exec-time secrets staged makes the
	// save 409: those files would be captured.
	if s.hasRunningJobWithSecrets(l.ID) || s.hasStagedExecSecrets(l.ID) {
		return store.NamedSnapshotRow{}, false, errSecretsInUse()
	}
	// The per-owner names cap: a save that would add a new name past it
	// answers 409. An existing name is always allowed. Checked before the
	// secret scrub so a refused save changes nothing on the guest.
	if err := s.checkNamedSnapshotLimit(ctx, l.Owner, name); err != nil {
		return store.NamedSnapshotRow{}, false, err
	}
	// Remove the create-time (and any lingering exec-time) secret files
	// before the checkpoint; re-stage the create-time ones after,
	// whatever happens. The scrub goes through the guest, so it removes
	// files a backend restart no longer remembers (B1). known is what
	// this process remembers, so it can warn about a removed file it no
	// longer knows (R4): after a backend restart the create-time secrets
	// are lost and this save drops them from the source.
	known := s.knownSecretNames(l.ID, create)
	unknown, err := s.scrubSecretsForSnapshot(ctx, l, known)
	if err != nil {
		s.log.Printf("snapshot: scrub secrets on %s: %v", l.ID, err)
		return store.NamedSnapshotRow{}, false, &namedSnapshotError{status: http.StatusInternalServerError, code: "scrub_failed", msg: "failed to remove secrets before the checkpoint"}
	}
	if unknown > 0 {
		s.log.Printf("snapshot: %s: scrub removed %d secret file(s) this backend no longer knew; their create-time values are lost for this lease (a backend restart drops them)", l.ID, unknown)
	}

	// The per-owner kept-bytes budget counts named snapshot bytes
	// alongside kept checkpoints. The size is only known after the
	// checkpoint, so the budget is enforced below.
	b, err := s.checkpointLease(ctx, l)
	if err != nil {
		return store.NamedSnapshotRow{}, false, s.mapSnapshotCheckpointError(err)
	}
	// The lease was released while the checkpoint ran: do not insert a
	// named-snapshot row that would keep pointing at the build of a lease
	// that no longer exists (spoond-775). The build itself is left for the
	// GC as an ordinary candidate.
	if s.leaseReleased(l) {
		s.log.Printf("snapshot: lease %s was released during its save; build %s left unreferenced", l.ID, b.BuildID)
		return store.NamedSnapshotRow{}, false, errLeaseNotFound()
	}
	// Walk to the template build at the root for the image_build_id and
	// the versions.
	imageBuildID, rootBuild, err := s.namedSnapshotBuildRoot(ctx, b.BuildID)
	if err != nil {
		s.log.Printf("snapshot: build chain of %s: %v", b.BuildID, err)
		imageBuildID = b.BuildID
		rootBuild = b
	}
	// Enforce the owner's byte budget against the named snapshot's size,
	// alongside kept checkpoints.
	if berr := s.enforceNamedSnapshotBudget(ctx, l.Owner, b.SizeBytes); berr != nil {
		return store.NamedSnapshotRow{}, false, berr
	}
	// The save point that simulates a backend stopping between the
	// checkpoint and the row insert (A7): the build is written but never
	// named, so a replay with the same key starts from scratch.
	if s.saveInterrupt != nil {
		if err := s.saveInterrupt(ctx, l, b.BuildID); err != nil {
			s.log.Printf("snapshot: save of %s/%s interrupted: %v", l.Owner, name, err)
			return store.NamedSnapshotRow{}, false, &namedSnapshotError{status: http.StatusInternalServerError, code: "internal", msg: "save interrupted"}
		}
	}
	// A test hook: another save may have committed the same key between
	// the checkpoint and the insert. The insert then hits the unique key
	// index and answers that row with 200 (R5).
	if s.saveBeforeInsert != nil {
		s.saveBeforeInsert(l.Owner, name, key)
	}
	row := store.NamedSnapshotRow{
		Owner:               l.Owner,
		Name:                name,
		BuildID:             b.BuildID,
		IdempotencyKey:      key,
		SourceLeaseID:       l.ID,
		Image:               l.Image,
		ImageBuildID:        imageBuildID,
		MemoryMB:            rootBuild.MemoryMB,
		SizeBytes:           b.SizeBytes,
		EnvdVersion:         b.EnvdVersion,
		FirecrackerVersion:  b.FirecrackerVersion,
		OrchestratorVersion: s.orchestratorVersion(ctx),
		CreatedAt:           s.now(),
	}
	// A user delete that ran after the leaseReleased check above must not
	// leave this row behind (spoond-q4j S1): retention keeps the newest
	// versions and the GC treats named_snapshots rows as roots, so a row
	// inserted after DeleteNamedSnapshotsOfOwner ran would live forever.
	// Hold the owner-delete lock across the check and the insert, the same
	// lock markOwnerDeleted takes: either this insert commits before the
	// mark (and the cleanup's drop, which runs after the mark, removes the
	// row) or the mark is already set here and the save refuses.
	s.ownerDeleteMu.Lock()
	if s.deletedOwners[l.Owner] {
		s.ownerDeleteMu.Unlock()
		s.log.Printf("snapshot: save of %s/%s: owner was deleted", l.Owner, name)
		return store.NamedSnapshotRow{}, false, errLeaseNotFound()
	}
	saved, err := s.db.InsertNamedSnapshot(ctx, row, keep)
	s.ownerDeleteMu.Unlock()
	if err != nil {
		// An insert that hits the (owner, name, idempotency_key) unique
		// index lost a replay race: another save of the same key
		// committed first. Answer its row with 200, never 500 (S3). The
		// losing save's checkpoint build stays unnamed, so the GC reclaims
		// it as an ordinary candidate (a catalog row) or the orphan reaper
		// does.
		if key != "" {
			if existing, gerr := s.db.GetNamedSnapshotByKey(ctx, l.Owner, name, key); gerr == nil {
				return existing, true, nil
			}
		}
		s.log.Printf("snapshot: insert %s/%s: %v", l.Owner, name, err)
		return store.NamedSnapshotRow{}, false, &namedSnapshotError{status: http.StatusInternalServerError, code: "internal", msg: "failed to save snapshot"}
	}
	// The source-side marker: written after the checkpoint and the row,
	// before the save answers (A4). Best effort, and not on the request
	// context: a disconnect mid-checkpoint must still leave the marker
	// (S1).
	mctx, mcancel := context.WithTimeout(context.WithoutCancel(ctx), secretsStageTimeout)
	s.writeLastSaveMarker(mctx, l, saved)
	mcancel()
	checkpointDur := time.Since(saveStart)
	// Retention: keep the last keep versions, never dropping one a live
	// lease started from.
	keepEff := s.effectiveKeep(ctx, l.Owner, name)
	if deleted, perr := s.db.PruneNamedSnapshotsKeeping(ctx, l.Owner, name, keepEff, s.startingBuildSet()); perr != nil {
		s.log.Printf("snapshot: prune %s/%s: %v", l.Owner, name, perr)
	} else if len(deleted) > 0 {
		s.unkeepBuilds(ctx, deleted)
	}
	s.UpdateNamedSnapshotMetrics(ctx)
	s.UpdateKeptMetrics(ctx)
	s.emitLeaseEvent(l.ID, l.Owner, LeaseSnapshotSaved,
		fmt.Sprintf("saved as %s@%d · %s · %s", name, saved.Version, formatEventBytes(saved.SizeBytes), eventDuration(checkpointDur)))
	return saved, false, nil
}

// effectiveKeep returns the retention for a name: its stored setting, or
// the default when it has none.
func (s *Service) effectiveKeep(ctx context.Context, owner, name string) int {
	keep, err := s.db.NamedSnapshotKeep(ctx, owner, name)
	if err == nil && keep > 0 {
		return keep
	}
	return s.snapshotKeepVersions()
}

// checkNamedSnapshotLimit refuses a save that would add a new name past
// the per-owner cap. A save on an existing name is always allowed.
func (s *Service) checkNamedSnapshotLimit(ctx context.Context, owner, name string) *namedSnapshotError {
	if _, err := s.db.GetNamedSnapshotLatest(ctx, owner, name); err == nil {
		return nil
	}
	cap := s.maxNamedSnapshots()
	if cap <= 0 {
		return nil
	}
	n, err := s.db.CountNamedSnapshotNames(ctx, owner)
	if err != nil {
		s.log.Printf("snapshot: count names of %s: %v", owner, err)
		return nil
	}
	if n >= cap {
		return errSnapshotLimit(fmt.Sprintf("named snapshot name limit reached (%d per owner); delete one with DELETE /api/named-snapshots/{name}", cap))
	}
	return nil
}

// enforceNamedSnapshotBudget refuses a save whose size would push the
// owner's kept+named bytes past max_kept_bytes.
func (s *Service) enforceNamedSnapshotBudget(ctx context.Context, owner string, size int64) *namedSnapshotError {
	var budget int64
	if s.identities != nil {
		if u := s.identities.UserByID(owner); u != nil {
			budget = u.MaxKeptBytes
		}
	}
	if budget <= 0 {
		return nil
	}
	kept, err := s.keptBytesOfOwner(ctx, owner)
	if err != nil {
		s.log.Printf("snapshot: kept bytes of %s: %v", owner, err)
		return nil
	}
	var namedBytes int64
	if n, err := s.db.NamedSnapshotBytesOfOwner(ctx, owner); err != nil {
		s.log.Printf("snapshot: named bytes of %s: %v", owner, err)
	} else {
		namedBytes = n
	}
	if kept+namedBytes+size > budget {
		return errKeptBudget(fmt.Sprintf("kept byte budget exceeded (%d + %d named + %d would pass %d); delete a snapshot with DELETE /api/named-snapshots/{name}[@v], or unpin builds with DELETE /api/snapshots/{build_id}", kept, namedBytes, size, budget))
	}
	return nil
}

// mapSnapshotCheckpointError maps a checkpoint failure onto a named
// snapshot error.
func (s *Service) mapSnapshotCheckpointError(err error) *namedSnapshotError {
	var capErr *keptCapError
	var budgetErr *keptBudgetError
	switch {
	case errors.Is(err, errLeaseReleased):
		// The lease was released while the checkpoint ran: to the caller
		// it is gone (spoond-775).
		return errLeaseNotFound()
	case errors.As(err, &capErr), errors.As(err, &budgetErr):
		return errKeptBudget(err.Error())
	default:
		return &namedSnapshotError{status: http.StatusInternalServerError, code: "internal", msg: "save failed"}
	}
}

// writeLastSaveMarker writes /run/spoond/last-save (0644, JSON) on the
// source lease after the checkpoint and before the save answers (A4). A
// copy's memory was captured before this write, so a copy never sees
// this save's marker. Best effort.
func (s *Service) writeLastSaveMarker(ctx context.Context, l *Lease, row store.NamedSnapshotRow) {
	payload, err := json.Marshal(map[string]any{
		"name":            row.Name,
		"version":         row.Version,
		"build_id":        row.BuildID,
		"idempotency_key": row.IdempotencyKey,
	})
	if err != nil {
		return
	}
	s.writeGuestFileAtomic(ctx, l, lastSavePath, payload, 0o644)
}

// unkeepBuilds drops any kept-builds pin on the pruned versions' builds,
// so a version dropped by retention frees its build for the GC (a lease
// still running from it would have blocked the prune).
func (s *Service) unkeepBuilds(ctx context.Context, builds []string) {
	if len(builds) == 0 {
		return
	}
	for _, id := range builds {
		if err := s.db.UnkeepBuildAny(ctx, id); err != nil {
			s.log.Printf("snapshot: unkeep %s: %v", id, err)
			continue
		}
		// Dropping a pin moves the owner's kept-bytes usage (#145 FS1).
		// Invalidate AFTER the write, so a concurrent compute cannot read
		// the pre-write state and cache it for the TTL.
		s.invalidateFairShares()
	}
}

// UpdateNamedSnapshotMetrics sets the named-snapshot gauges (2.7, #83).
func (s *Service) UpdateNamedSnapshotMetrics(ctx context.Context) {
	// A named-snapshot change moves the owner's disk usage (#145 FS1).
	s.invalidateFairShares()
	if s.metrics == nil {
		return
	}
	versions, bytes, err := s.db.NamedSnapshotMetrics(ctx)
	if err != nil {
		s.log.Printf("snapshot metrics: %v", err)
		return
	}
	s.metrics.NamedSnapshots.Set(float64(versions))
	s.metrics.NamedSnapshotBytes.Set(float64(bytes))
}

// parseSnapshotRef splits a name[@version] reference. A missing @v means
// the latest version (version 0, resolved by the caller). It validates
// the name.
func parseSnapshotRef(ref string) (name string, version int64, err error) {
	name, versionStr, hasVersion := strings.Cut(ref, "@")
	if err := validateNamedSnapshotName(name); err != nil {
		return "", 0, err
	}
	if hasVersion {
		v, perr := strconv.ParseInt(versionStr, 10, 64)
		if perr != nil || v < 1 {
			return "", 0, fmt.Errorf("invalid snapshot version %q", versionStr)
		}
		return name, v, nil
	}
	return name, 0, nil
}

// resolveNamedSnapshot returns a version row by name[@v], resolving the
// latest when version is 0. Owner-scoped: another owner's name is
// ErrNotFound.
func (s *Service) resolveNamedSnapshot(ctx context.Context, owner, name string, version int64) (store.NamedSnapshotRow, error) {
	if version > 0 {
		return s.db.GetNamedSnapshot(ctx, owner, name, version)
	}
	return s.db.GetNamedSnapshotLatest(ctx, owner, name)
}

// snapshotView renders the "snapshot" object a create response and the
// lease detail carry (A3): the name, version and build the lease started
// from, or nil when it did not start from a named snapshot. A lease
// loaded from the store has no name/version in memory, so they are
// looked up by build id.
func (s *Service) snapshotView(ctx context.Context, l *Lease) map[string]any {
	if l.SnapshotBuildID == "" {
		return nil
	}
	name, version := l.SnapshotName, l.SnapshotVersion
	if name == "" || version == 0 {
		if row, err := s.db.GetNamedSnapshotByBuild(ctx, l.Owner, l.SnapshotBuildID); err == nil {
			name, version = row.Name, row.Version
		}
	}
	return map[string]any{
		"name":     name,
		"version":  version,
		"build_id": l.SnapshotBuildID,
	}
}

// snapshotStartFromRequest resolves a create body's "snapshot" field into
// the leaseRequest override. An unknown name or version answers 404
// not_found; an explicit image that is not the snapshot's image answers
// 400 image_mismatch. The lease's image always comes from the snapshot.
// It takes a reference on the resolved build (B1) so the version and its
// build cannot be deleted or pruned while the create is in flight; the
// caller releases it with endStartingBuild once the create finishes.
func (s *Service) snapshotStartFromRequest(ctx context.Context, owner, ref, image string) (*snapshotStart, *namedSnapshotError) {
	name, version, err := parseSnapshotRef(ref)
	if err != nil {
		return nil, &namedSnapshotError{status: http.StatusBadRequest, code: "bad_request", msg: err.Error()}
	}
	row, err := s.resolveNamedSnapshot(ctx, owner, name, version)
	if errors.Is(err, store.ErrNotFound) {
		if version > 0 {
			return nil, &namedSnapshotError{status: http.StatusNotFound, code: "not_found", msg: fmt.Sprintf("snapshot %s@%d not found", name, version)}
		}
		return nil, errNamedNotFound()
	}
	if err != nil {
		s.log.Printf("create: resolve snapshot %s: %v", ref, err)
		return nil, &namedSnapshotError{status: http.StatusInternalServerError, code: "internal", msg: "failed to read snapshot"}
	}
	// The version is real: hold its build against delete, retention and
	// GC for the whole create. Re-read the row afterwards, so a start
	// that lost a race with a forced delete answers cannot_start rather
	// than starting from a version that no longer exists (B1).
	s.beginStartingBuild(row.BuildID)
	if _, rerr := s.db.GetNamedSnapshot(ctx, owner, row.Name, row.Version); errors.Is(rerr, store.ErrNotFound) {
		s.endStartingBuild(row.BuildID)
		return nil, &namedSnapshotError{status: http.StatusNotFound, code: "not_found",
			msg: fmt.Sprintf("snapshot %s@%d not found", row.Name, row.Version)}
	} else if rerr != nil {
		s.endStartingBuild(row.BuildID)
		s.log.Printf("create: re-check snapshot %s: %v", ref, rerr)
		return nil, &namedSnapshotError{status: http.StatusInternalServerError, code: "internal", msg: "failed to read snapshot"}
	}
	if image != "" && image != row.Image {
		s.endStartingBuild(row.BuildID)
		return nil, &namedSnapshotError{status: http.StatusBadRequest, code: "image_mismatch",
			msg: fmt.Sprintf("image %s does not match snapshot %s@%d (image %s)", image, row.Name, row.Version, row.Image)}
	}
	return &snapshotStart{row: row}, nil
}

// writeStartedFromMarker writes /run/spoond/started-from on a lease
// started from a named snapshot (A4): JSON {name, version, build_id}.
// It returns an error so the create can fail the lease rather than hand
// out a copy that cannot name its origin (S3).
func (s *Service) writeStartedFromMarker(l *Lease) error {
	payload, err := json.Marshal(map[string]any{
		"name":     l.SnapshotName,
		"version":  l.SnapshotVersion,
		"build_id": l.SnapshotBuildID,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	return s.writeGuestFileAtomic(ctx, l, startedFromPath, payload, 0o644)
}

// snapshotHostMismatch reports whether a saved version cannot start on
// this host because the host's envd, firecracker or orchestrator
// versions differ from the ones the version was saved with (2.7, #83
// S2). It compares only versions both sides know: an unknown host
// version ("") never refuses, and an empty saved version never matches
// anything. The returned cause names the differing component.
func (s *Service) snapshotHostMismatch(ctx context.Context, row store.NamedSnapshotRow) (string, bool) {
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		// Without the host's versions there is nothing to compare; the
		// create proceeds and a real incompatibility surfaces from the
		// substrate or the guest.
		return "", false
	}
	if row.EnvdVersion != "" && info.EnvdVersion != "" && row.EnvdVersion != info.EnvdVersion {
		return fmt.Sprintf("envd %s, host has %s", row.EnvdVersion, info.EnvdVersion), true
	}
	if row.FirecrackerVersion != "" && info.FirecrackerVersion != "" && row.FirecrackerVersion != info.FirecrackerVersion {
		return fmt.Sprintf("firecracker %s, host has %s", row.FirecrackerVersion, info.FirecrackerVersion), true
	}
	if row.OrchestratorVersion != "" && info.Version != "" && row.OrchestratorVersion != info.Version {
		return fmt.Sprintf("orchestrator %s, host has %s", row.OrchestratorVersion, info.Version), true
	}
	return "", false
}

// rerunSnapshotRetention re-applies a name's retention after a lease that
// started from one of its versions is released (S5): a version that was
// spared only because this lease ran from it is dropped once no live
// lease uses it. It reads the name from the build id; a forced delete
// leaves no row, in which case there is nothing to prune.
func (s *Service) rerunSnapshotRetention(ctx context.Context, l *Lease) {
	if l.SnapshotBuildID == "" {
		return
	}
	row, err := s.db.GetNamedSnapshotByBuild(ctx, l.Owner, l.SnapshotBuildID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		s.log.Printf("snapshot retention: build %s of %s: %v", l.SnapshotBuildID, l.ID, err)
		return
	}
	keep := s.effectiveKeep(ctx, l.Owner, row.Name)
	deleted, err := s.db.PruneNamedSnapshotsKeeping(ctx, l.Owner, row.Name, keep, s.startingBuildSet())
	if err != nil {
		s.log.Printf("snapshot retention: prune %s/%s after release of %s: %v", l.Owner, row.Name, l.ID, err)
		return
	}
	if len(deleted) > 0 {
		s.unkeepBuilds(ctx, deleted)
		s.UpdateNamedSnapshotMetrics(ctx)
	}
}

// handleLeaseSnapshotSave is POST /api/leases/{id}/snapshots: save the
// lease as a named snapshot. Owner only, live leases only.
func (s *Server) handleLeaseSnapshotSave(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Name           string `json:"name"`
		IdempotencyKey string `json:"idempotency_key"`
		Keep           *int   `json:"keep"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "code": "bad_request"})
		return
	}
	if err := validateNamedSnapshotName(req.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request"})
		return
	}
	if req.Keep != nil && (*req.Keep < minSnapshotKeep || *req.Keep > maxSnapshotKeep) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("keep must be between %d and %d", minSnapshotKeep, maxSnapshotKeep), "code": "bad_request"})
		return
	}
	lease := s.svc.lookup(owner, id)
	// Idempotent replay first (N2): a save whose key already committed
	// answers 200 with that version even when the source lease is gone,
	// and before the live check. Any lease may replay the key (A1).
	if req.IdempotencyKey != "" {
		if row, err := s.svc.db.GetNamedSnapshotByKey(r.Context(), owner, req.Name, req.IdempotencyKey); err == nil {
			s.writeSavedSnapshot(w, http.StatusOK, row)
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			s.svc.log.Printf("snapshot: lookup key %s/%s: %v", owner, req.Name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save snapshot", "code": "internal"})
			return
		}
	}
	if lease == nil {
		errLeaseNotFound().write(w)
		return
	}
	if !lease.live() {
		if lease.State == "lost" {
			writeLeaseLost(w, lease)
			return
		}
		errLeaseNotLive().write(w)
		return
	}
	if req.IdempotencyKey != "" {
		// Claim the key in memory: a concurrent save with it gets 409. A
		// failed prior attempt does not poison the key (begin resets it).
		if !s.svc.saves.begin(owner, req.Name, req.IdempotencyKey) {
			errSaveInProgress().write(w)
			return
		}
		// Re-check the catalog now that the key is claimed: another save
		// may have committed between the first lookup and begin. Answer
		// that row with 200 and release the claim, no checkpoint (S3).
		if s.svc.saveAfterClaim != nil {
			s.svc.saveAfterClaim(owner, req.Name, req.IdempotencyKey)
		}
		if row, err := s.svc.db.GetNamedSnapshotByKey(r.Context(), owner, req.Name, req.IdempotencyKey); err == nil {
			s.svc.saves.done(owner, req.Name, req.IdempotencyKey)
			s.writeSavedSnapshot(w, http.StatusOK, row)
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			s.svc.saves.done(owner, req.Name, req.IdempotencyKey)
			s.svc.log.Printf("snapshot: lookup key %s/%s: %v", owner, req.Name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save snapshot", "code": "internal"})
			return
		}
	}
	keep := 0
	if req.Keep != nil {
		keep = *req.Keep
	}
	row, replayed, serr := s.svc.saveLeaseSnapshot(r.Context(), lease, req.Name, req.IdempotencyKey, keep)
	if serr != nil {
		if req.IdempotencyKey != "" {
			if serr.code == "lease_busy" {
				// The save never ran: nothing failed, so do not leave a
				// failed record. The key is free to retry.
				s.svc.saves.done(owner, req.Name, req.IdempotencyKey)
			} else {
				s.svc.saves.fail(owner, req.Name, req.IdempotencyKey, serr.code, serr.msg)
			}
		}
		serr.write(w)
		return
	}
	if req.IdempotencyKey != "" {
		s.svc.saves.done(owner, req.Name, req.IdempotencyKey)
	}
	if replayed {
		s.writeSavedSnapshot(w, http.StatusOK, row)
		return
	}
	s.writeSavedSnapshot(w, http.StatusCreated, row)
}

// saveLeaseSnapshot takes the lease's busy flag and runs saveNamedSnapshot.
func (s *Service) saveLeaseSnapshot(ctx context.Context, l *Lease, name, key string, keep int) (store.NamedSnapshotRow, bool, *namedSnapshotError) {
	s.store.mu.Lock()
	if l.busy {
		s.store.mu.Unlock()
		return store.NamedSnapshotRow{}, false, errLeaseBusyNamed()
	}
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)
	return s.saveNamedSnapshot(ctx, l, name, key, keep)
}

// writeSavedSnapshot renders a 201/200 save answer.
func (s *Server) writeSavedSnapshot(w http.ResponseWriter, status int, row store.NamedSnapshotRow) {
	writeJSON(w, status, map[string]any{
		"name":       row.Name,
		"version":    row.Version,
		"build_id":   row.BuildID,
		"image":      row.Image,
		"memory_mb":  row.MemoryMB,
		"size_bytes": row.SizeBytes,
		"created_at": formatRFC3339(row.CreatedAt),
	})
}

// handleNamedSnapshotsList is GET /api/named-snapshots (?prefix=).
func (s *Server) handleNamedSnapshotsList(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	prefix := r.URL.Query().Get("prefix")
	if prefix != "" && !validNamePrefix(prefix) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid prefix", "code": "bad_request"})
		return
	}
	rows, err := s.svc.db.ListNamedSnapshots(r.Context(), owner, prefix)
	if err != nil {
		s.svc.log.Printf("snapshot: list %s: %v", owner, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list snapshots", "code": "internal"})
		return
	}
	currentImageBuild := map[string]string{}
	if imgs, err := s.svc.db.ListImages(r.Context()); err == nil {
		for _, img := range imgs {
			currentImageBuild[img.Name] = img.CurrentBuildID
		}
	}
	// Group by name, newest version first. One query counts the live
	// leases per build, rather than one query per version (N5).
	buildIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		buildIDs = append(buildIDs, row.BuildID)
	}
	inUseByBuild, _ := s.svc.db.LiveLeaseCountsByBuild(r.Context(), buildIDs)
	type nameGroup struct {
		name     string
		latest   int64
		versions []map[string]any
	}
	var order []string
	groups := map[string]*nameGroup{}
	for _, row := range rows {
		g := groups[row.Name]
		if g == nil {
			g = &nameGroup{name: row.Name}
			groups[row.Name] = g
			order = append(order, row.Name)
		}
		if row.Version > g.latest {
			g.latest = row.Version
		}
		stale := currentImageBuild[row.Image] != "" && currentImageBuild[row.Image] != row.ImageBuildID
		g.versions = append(g.versions, map[string]any{
			"version":    row.Version,
			"build_id":   row.BuildID,
			"image":      row.Image,
			"memory_mb":  row.MemoryMB,
			"size_bytes": row.SizeBytes,
			"created_at": formatRFC3339(row.CreatedAt),
			"in_use":     inUseByBuild[row.BuildID],
			"stale":      stale,
		})
	}
	out := []map[string]any{}
	for _, name := range order {
		g := groups[name]
		out = append(out, map[string]any{
			"name":     g.name,
			"latest":   g.latest,
			"versions": g.versions,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

// validNamePrefix accepts a partial name prefix: empty, or made of
// name-part characters and at most one slash.
func validNamePrefix(p string) bool {
	if p == "" {
		return true
	}
	if strings.Count(p, "/") > 1 {
		return false
	}
	for _, c := range p {
		if c == '/' || c == '.' || c == '_' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// handleNamedSnapshotShow is GET /api/named-snapshots/{name}[@v]. It
// also serves the ?idempotency_key= lookup (A2): state in_progress,
// ready, failed or absent, and never starts a checkpoint.
func (s *Server) handleNamedSnapshotShow(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	ref := r.PathValue("name")
	name, version, err := parseSnapshotRef(ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request"})
		return
	}
	if key := r.URL.Query().Get("idempotency_key"); key != "" {
		s.handleNamedSnapshotKeyLookup(w, r, owner, name, key)
		return
	}
	row, err := s.svc.resolveNamedSnapshot(r.Context(), owner, name, version)
	if errors.Is(err, store.ErrNotFound) {
		errNamedNotFound().write(w)
		return
	}
	if err != nil {
		s.svc.log.Printf("snapshot: get %s/%s: %v", owner, name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read snapshot", "code": "internal"})
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotDetail(r.Context(), row))
}

// handleNamedSnapshotKeyLookup answers ?idempotency_key= (A2).
func (s *Server) handleNamedSnapshotKeyLookup(w http.ResponseWriter, r *http.Request, owner, name, key string) {
	if row, err := s.svc.db.GetNamedSnapshotByKey(r.Context(), owner, name, key); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"state":    "ready",
			"version":  row.Version,
			"build_id": row.BuildID,
		})
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		// A catalog failure is not "absent": a key whose row may exist
		// must not read as unsaved.
		s.svc.log.Printf("snapshot: lookup key %s/%s: %v", owner, name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read snapshot", "code": "internal"})
		return
	}
	switch st := s.svc.saves.lookup(owner, name, key); st.state {
	case "in_progress":
		writeJSON(w, http.StatusOK, map[string]any{"state": "in_progress"})
	case "failed":
		writeJSON(w, http.StatusOK, map[string]any{"state": "failed", "error": st.err, "code": st.code})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"state": "absent"})
	}
}

// snapshotDetail renders one version row with in_use and stale.
func (s *Server) snapshotDetail(ctx context.Context, row store.NamedSnapshotRow) map[string]any {
	inUse, _ := s.svc.db.LiveLeasesUsingBuild(ctx, row.BuildID)
	stale := false
	if img, err := s.svc.db.GetImage(ctx, row.Image); err == nil {
		stale = img.CurrentBuildID != "" && img.CurrentBuildID != row.ImageBuildID
	}
	return map[string]any{
		"name":       row.Name,
		"version":    row.Version,
		"build_id":   row.BuildID,
		"image":      row.Image,
		"memory_mb":  row.MemoryMB,
		"size_bytes": row.SizeBytes,
		"created_at": formatRFC3339(row.CreatedAt),
		"in_use":     inUse,
		"stale":      stale,
	}
}

// handleNamedSnapshotDelete is DELETE /api/named-snapshots/{name}[@v]
// (?force=1). 409 when a live lease started from it, unless forced.
func (s *Server) handleNamedSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	ref := r.PathValue("name")
	name, version, err := parseSnapshotRef(ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request"})
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if version > 0 {
		row, err := s.svc.db.GetNamedSnapshot(r.Context(), owner, name, version)
		if errors.Is(err, store.ErrNotFound) {
			errNamedNotFound().write(w)
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete snapshot", "code": "internal"})
			return
		}
		if !force {
			n, _ := s.svc.db.LiveLeasesUsingBuild(r.Context(), row.BuildID)
			if n > 0 || s.svc.startingBuild(row.BuildID) {
				errSnapshotInUse(fmt.Sprintf("snapshot is in use by %d live lease(s); retry with ?force=1 to drop the row", n)).write(w)
				return
			}
		}
		if err := s.svc.db.DeleteNamedSnapshot(r.Context(), owner, name, version); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				errNamedNotFound().write(w)
				return
			}
			s.svc.log.Printf("snapshot: delete %s@%d: %v", name, version, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete snapshot", "code": "internal"})
			return
		}
		s.svc.UpdateNamedSnapshotMetrics(r.Context())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// No version: delete every version of the name. List the exact name
	// (not the prefix form): deleting "warm" must not see "warmup".
	rows, err := s.svc.db.ListNamedSnapshotsExact(r.Context(), owner, name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete snapshot", "code": "internal"})
		return
	}
	if len(rows) == 0 {
		errNamedNotFound().write(w)
		return
	}
	if !force {
		for _, row := range rows {
			n, _ := s.svc.db.LiveLeasesUsingBuild(r.Context(), row.BuildID)
			if n > 0 || s.svc.startingBuild(row.BuildID) {
				errSnapshotInUse(fmt.Sprintf("snapshot %s@%d is in use by %d live lease(s); retry with ?force=1 to drop the row", name, row.Version, n)).write(w)
				return
			}
		}
	}
	if _, err := s.svc.db.DeleteNamedSnapshotName(r.Context(), owner, name); err != nil {
		s.svc.log.Printf("snapshot: delete name %s: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete snapshot", "code": "internal"})
		return
	}
	s.svc.UpdateNamedSnapshotMetrics(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// handleNamedSnapshotKeep is PUT /api/named-snapshots/{name}: set the
// name's retention and apply it at once (the live-lease rule holds).
func (s *Server) handleNamedSnapshotKeep(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	ref := r.PathValue("name")
	name, version, err := parseSnapshotRef(ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request"})
		return
	}
	// keep is a per-name setting (A6): a @version is a bad request (N3).
	if version > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "keep is set per name; drop the @version", "code": "bad_request"})
		return
	}
	var req struct {
		Keep *int `json:"keep"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Keep == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "keep is required", "code": "bad_request"})
		return
	}
	if *req.Keep < minSnapshotKeep || *req.Keep > maxSnapshotKeep {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("keep must be between %d and %d", minSnapshotKeep, maxSnapshotKeep), "code": "bad_request"})
		return
	}
	if _, err := s.svc.db.GetNamedSnapshotLatest(r.Context(), owner, name); errors.Is(err, store.ErrNotFound) {
		errNamedNotFound().write(w)
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update snapshot", "code": "internal"})
		return
	}
	if err := s.svc.db.SetNamedSnapshotKeep(r.Context(), owner, name, *req.Keep); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update snapshot", "code": "internal"})
		return
	}
	if deleted, err := s.svc.db.PruneNamedSnapshotsKeeping(r.Context(), owner, name, *req.Keep, s.svc.startingBuildSet()); err != nil {
		s.svc.log.Printf("snapshot: prune %s: %v", name, err)
	} else {
		s.svc.unkeepBuilds(r.Context(), deleted)
	}
	s.svc.UpdateNamedSnapshotMetrics(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "keep": *req.Keep, "ok": true})
}
