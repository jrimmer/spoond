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

// scrubSecretsForSnapshot removes the secret files spoond staged under
// /run/secrets on the lease's sandbox. It removes the create-time secret
// names plus the exec-time names of any job whose files are still
// present (its wrapper normally cleaned them; a save must not capture a
// lingering one). Called right before the checkpoint so none are
// captured. Callers have already refused the save when a synchronous or
// background job with secrets is still running, so those names are
// mostly defensive.
func (s *Service) scrubSecretsForSnapshot(ctx context.Context, l *Lease) {
	names := map[string]bool{}
	for _, n := range sortedSecretNames(s.createSecretsFor(l.ID)) {
		names[n] = true
	}
	// The secret names of every job tracked on this lease, running or
	// not: a stale file left by a wrapper that never cleaned up would
	// otherwise be captured by the checkpoint.
	for _, n := range s.jobSecretNamesFor(l.ID) {
		names[n] = true
	}
	// A synchronous exec that staged secrets after the refuse check but
	// before this scrub (the check does not take the lease's busy flag)
	// would otherwise be captured.
	for _, n := range s.stagedExecSecretNamesFor(l.ID) {
		names[n] = true
	}
	s.removeAllSecrets(l.SandboxID, sortedSecretNamesFromSet(names))
}

// saveNamedSnapshot performs one save of a lease into a named snapshot:
// it runs the checkpoint and inserts the version row, applies
// retention, emits the event and writes the source-side marker. Callers
// own the busy window and the idempotency bookkeeping. Returns the
// inserted row.
func (s *Service) saveNamedSnapshot(ctx context.Context, l *Lease, name, key string, keep int) (store.NamedSnapshotRow, *namedSnapshotError) {
	if keep <= 0 {
		keep = s.snapshotKeepVersions()
	}
	saveStart := s.now()
	// A running background job with exec-time secrets staged makes the
	// save 409: those files would be captured.
	if s.hasRunningJobWithSecrets(l.ID) || s.hasStagedExecSecrets(l.ID) {
		return store.NamedSnapshotRow{}, errSecretsInUse()
	}
	// The per-owner names cap: a save that would add a new name past it
	// answers 409. An existing name is always allowed. Checked before the
	// secret scrub so a refused save changes nothing on the guest.
	if err := s.checkNamedSnapshotLimit(ctx, l.Owner, name); err != nil {
		return store.NamedSnapshotRow{}, err
	}
	// Remove the create-time (and any lingering exec-time) secret files
	// before the checkpoint; re-stage the create-time ones after,
	// whatever happens.
	create := s.createSecretsFor(l.ID)
	s.scrubSecretsForSnapshot(ctx, l)
	defer func() {
		if len(create) > 0 {
			if err := s.stageSecrets(ctx, l.SandboxID, create); err != nil {
				s.log.Printf("snapshot: re-stage secrets on %s: %v", l.ID, err)
			}
		}
	}()

	// The per-owner kept-bytes budget counts named snapshot bytes
	// alongside kept checkpoints. The size is only known after the
	// checkpoint, so the budget is enforced below.
	b, err := s.checkpointLease(ctx, l)
	if err != nil {
		return store.NamedSnapshotRow{}, s.mapSnapshotCheckpointError(err)
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
		return store.NamedSnapshotRow{}, berr
	}
	// The save point that simulates a backend stopping between the
	// checkpoint and the row insert (A7): the build is written but never
	// named, so a replay with the same key starts from scratch.
	if s.saveInterrupt != nil {
		if err := s.saveInterrupt(ctx, l, b.BuildID); err != nil {
			s.log.Printf("snapshot: save of %s/%s interrupted: %v", l.Owner, name, err)
			return store.NamedSnapshotRow{}, &namedSnapshotError{status: http.StatusInternalServerError, code: "internal", msg: "save interrupted"}
		}
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
	saved, err := s.db.InsertNamedSnapshot(ctx, row, keep)
	if err != nil {
		failed := &namedSnapshotError{status: http.StatusInternalServerError, code: "internal", msg: "failed to save snapshot"}
		s.log.Printf("snapshot: insert %s/%s: %v", l.Owner, name, err)
		return store.NamedSnapshotRow{}, failed
	}
	// The source-side marker: written after the checkpoint and the row,
	// before the save answers (A4). Best effort.
	s.writeLastSaveMarker(ctx, l, saved)
	checkpointDur := time.Since(saveStart)
	// Retention: keep the last keep versions, never dropping one a live
	// lease started from.
	keepEff := s.effectiveKeep(ctx, l.Owner, name)
	if deleted, perr := s.db.PruneNamedSnapshots(ctx, l.Owner, name, keepEff); perr != nil {
		s.log.Printf("snapshot: prune %s/%s: %v", l.Owner, name, perr)
	} else if len(deleted) > 0 {
		s.unkeepBuilds(ctx, deleted)
	}
	s.UpdateNamedSnapshotMetrics(ctx)
	s.UpdateKeptMetrics(ctx)
	s.emitLeaseEvent(l.ID, l.Owner, LeaseSnapshotSaved,
		fmt.Sprintf("saved as %s@%d · %s · %s", name, saved.Version, formatEventBytes(saved.SizeBytes), eventDuration(checkpointDur)))
	return saved, nil
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
	for _, id := range builds {
		if err := s.db.UnkeepBuildAny(ctx, id); err != nil {
			s.log.Printf("snapshot: unkeep %s: %v", id, err)
		}
	}
}

// UpdateNamedSnapshotMetrics sets the named-snapshot gauges (2.7, #83).
func (s *Service) UpdateNamedSnapshotMetrics(ctx context.Context) {
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
	if lease == nil {
		errNamedNotFound().write(w)
		return
	}
	if !lease.live() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "lease is not running", "code": "lease_busy"})
		return
	}
	// Idempotent replay: a version with this key already exists (any
	// lease, A1). No checkpoint.
	if req.IdempotencyKey != "" {
		if row, err := s.svc.db.GetNamedSnapshotByKey(r.Context(), owner, req.Name, req.IdempotencyKey); err == nil {
			s.writeSavedSnapshot(w, http.StatusOK, row)
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			s.svc.log.Printf("snapshot: lookup key %s/%s: %v", owner, req.Name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save snapshot", "code": "internal"})
			return
		}
		// Claim the key in memory: a concurrent save with it gets 409. A
		// failed prior attempt does not poison the key (begin resets it).
		if !s.svc.saves.begin(owner, req.Name, req.IdempotencyKey) {
			errSaveInProgress().write(w)
			return
		}
	}
	keep := 0
	if req.Keep != nil {
		keep = *req.Keep
	}
	row, serr := s.svc.saveLeaseSnapshot(r.Context(), lease, req.Name, req.IdempotencyKey, keep)
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
	s.writeSavedSnapshot(w, http.StatusCreated, row)
}

// saveLeaseSnapshot takes the lease's busy flag and runs saveNamedSnapshot.
func (s *Service) saveLeaseSnapshot(ctx context.Context, l *Lease, name, key string, keep int) (store.NamedSnapshotRow, *namedSnapshotError) {
	s.store.mu.Lock()
	if l.busy {
		s.store.mu.Unlock()
		return store.NamedSnapshotRow{}, errLeaseBusyNamed()
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
	// Group by name, newest version first.
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
		inUse, _ := s.svc.db.LiveLeasesUsingBuild(r.Context(), row.BuildID)
		stale := currentImageBuild[row.Image] != "" && currentImageBuild[row.Image] != row.ImageBuildID
		g.versions = append(g.versions, map[string]any{
			"version":    row.Version,
			"build_id":   row.BuildID,
			"image":      row.Image,
			"memory_mb":  row.MemoryMB,
			"size_bytes": row.SizeBytes,
			"created_at": formatRFC3339(row.CreatedAt),
			"in_use":     inUse,
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
			if n > 0 {
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
			if n > 0 {
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
	name, _, err := parseSnapshotRef(r.PathValue("name"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request"})
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
	if deleted, err := s.svc.db.PruneNamedSnapshots(r.Context(), owner, name, *req.Keep); err != nil {
		s.svc.log.Printf("snapshot: prune %s: %v", name, err)
	} else {
		s.svc.unkeepBuilds(r.Context(), deleted)
	}
	s.svc.UpdateNamedSnapshotMetrics(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "keep": *req.Keep, "ok": true})
}
