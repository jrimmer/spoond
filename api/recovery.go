package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Crash recovery (U10 R16/D4): an orchestrator crash kills every
// running sandbox. reconcileCrash resumes every lease that has a
// checkpoint — with the same sandbox id, from the checkpoint build —
// and marks the rest lost. It runs once at backend start (from
// ReconcileOrphans), every 30 s in the background, and immediately when
// NodeInfo goes from failing to succeeding.

// leaseLostMessage builds the body of a 410 code:lease_lost response:
// it says the substrate lost the sandbox, the stored reason, and that a
// DELETE frees the quota.
func leaseLostMessage(l *Lease) string {
	msg := "the substrate lost this lease's sandbox"
	if l != nil && l.LostReason != "" {
		msg += ": " + l.LostReason
	}
	return msg + "; DELETE the lease to free its quota"
}

// writeLeaseLost answers 410 lease_lost for a lost lease: the caller
// learns the substrate lost it, why, and that DELETE frees the quota.
func writeLeaseLost(w http.ResponseWriter, l *Lease) {
	writeLeaseLostMessage(w, leaseLostMessage(l))
}

// writeLeaseLostMessage writes the one 410 lease_lost body: the error
// message and its code. Both spellings of the response (a lease in hand,
// a *leaseLostError in flight) go through it, so they never drift.
func writeLeaseLostMessage(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusGone, map[string]string{
		"error": msg,
		"code":  "lease_lost",
	})
}

// leaseSuspendedMessage is the single 409 body for a lease that is
// suspended and must be resumed first. It carries code lease_suspended
// so a client can tell it apart from the other 409 (busy, code
// lease_busy) without matching the message text.
const leaseSuspendedMessage = "lease is suspended; resume it first"

// writeLeaseSuspended answers 409 lease_suspended for a suspended lease.
// When reason is non-empty the suspension was automatic and the body
// also carries "reason"
// (idle|idle_suspend|hold_lapsed|pressure|preempt|resume_failed)
// so a client learns why it was suspended without reading the event
// stream (#145 D6). A hand or drain suspend has no reason and the field
// is omitted. Callers read the reason through Service.leaseSuspendReason
// under the store lock, so a concurrent resume clearing it never races.
func writeLeaseSuspended(w http.ResponseWriter, reason string) {
	body := map[string]string{
		"error": leaseSuspendedMessage,
		"code":  "lease_suspended",
	}
	if reason != "" {
		body["reason"] = reason
	}
	writeJSON(w, http.StatusConflict, body)
}

// writeLeaseSuspendedID answers a suspended-lease 409 for a caller that
// has only the lease id (the service returned errSuspended): it reads
// the suspension reason through the service. An unknown lease still gets
// the same 409 with no reason, matching the other refusal sites.
func (s *Server) writeLeaseSuspendedID(w http.ResponseWriter, id string) {
	writeLeaseSuspended(w, s.svc.leaseSuspendReason(id))
}

// lostErr returns the 410 lease_lost error for a lease already lost, or
// nil when the call may proceed. It is shared by the service operations
// that touch a lease, so a lost lease is refused the same way everywhere
// (the handlers map *leaseLostError onto the response). Call with
// s.store.mu held.
func lostErr(l *Lease) error {
	if l.State == "lost" {
		return &leaseLostError{msg: leaseLostMessage(l)}
	}
	return nil
}

// leaseLostError is the error a lease operation returns for a lost
// lease: its message is the 410 lease_lost body (the substrate lost the
// sandbox, the stored reason, and that DELETE frees the quota).
type leaseLostError struct{ msg string }

func (e *leaseLostError) Error() string { return e.msg }

// ensureLive answers 410 lease_lost for a lost lease and reports whether
// the call may proceed. Every route that can act on a lease calls it (or
// the service reports the lost state through lostErr), so a lost lease is
// refused the same way everywhere with the reason its loss event carried.
func (s *Server) ensureLive(w http.ResponseWriter, l *Lease) bool {
	if l.State == "lost" {
		writeLeaseLost(w, l)
		return false
	}
	return true
}

// writeLeaseLostErr answers 410 lease_lost for a service operation that
// refused a lost lease (a *leaseLostError), and reports whether it did.
func writeLeaseLostErr(w http.ResponseWriter, err error) bool {
	var lost *leaseLostError
	if errors.As(err, &lost) {
		writeLeaseLostMessage(w, lost.Error())
		return true
	}
	return false
}

// markLost records that a lease was lost and why: it enters the lost
// state (stamping lost_at once, as setState does), stores the reason the
// lost event carries, and returns that reason. A released lease is left
// alone: a release and a recovery race, and losing an already-released
// lease would resurrect it with a stale row and a spurious lost event
// (spoond-775). Call with s.store.mu held. A lost lease must mean a
// stopped guest (spoond-63a): after dropping the lock, every caller that
// did mark the lease lost calls stopLostSandbox, which deletes the
// lease's sandbox (and the half-started one a failed create or resume
// left under the same id).
func (s *Service) markLost(l *Lease, reason string) string {
	if l.released {
		return reason
	}
	setLostReason(l, reason)
	l.setState("lost")
	s.saveLeaseLocked(l)
	s.journalLease(journalOpLost, l, reason)
	return reason
}

// stopLostSandbox stops a lease that markLost just lost: it deletes the
// sandbox through the substrate (bounded retries, a log line; a delete
// that still fails is left to the periodic orphan sweep) and drops the
// sandbox row. Call without s.store.mu held.
func (s *Service) stopLostSandbox(sandboxID, leaseID string) {
	s.deleteSandboxWithRetries(sandboxID, leaseID, "lost")
	s.deleteSandboxRow(sandboxID)
}

// defaultLostSandboxDeleteAttempts and defaultLostSandboxDeleteBackoff
// bound the substrate Delete when a lease becomes lost: a few attempts
// with a short pause, so a transient substrate blip does not leave a
// guest running; a delete that still fails is retried by the periodic
// orphan sweep. Fields on Service let tests shrink the pause.
const (
	defaultLostSandboxDeleteAttempts = 3
	defaultLostSandboxDeleteBackoff  = 500 * time.Millisecond
	// lostSandboxDeleteTimeout bounds one Delete attempt. It is detached
	// from the caller's context (a cancelled reconcile must still stop the
	// guest) but still finite so the retry loop cannot hang forever.
	lostSandboxDeleteTimeout = 30 * time.Second
)

// deleteSandboxWithRetries stops a sandbox through the substrate with
// bounded retries, logging each failure. A nil or empty sandbox id is a
// no-op. reason names why the guest is being stopped ("lost" or
// "released") for the log lines. A delete that still fails after every
// attempt is logged and recorded so the periodic orphan sweep retries
// it (a released lease's row is gone by then, so the sweep cannot
// rediscover it).
func (s *Service) deleteSandboxWithRetries(sandboxID, leaseID, reason string) {
	if sandboxID == "" {
		return
	}
	attempts := s.lostSandboxDeleteAttempts
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), lostSandboxDeleteTimeout)
		err = s.sub.Delete(ctx, sandboxID)
		cancel()
		if err == nil {
			s.log.Printf("%s: lease %s stopped sandbox %s", reason, leaseID, sandboxID)
			return
		}
		if attempt < attempts {
			s.log.Printf("%s: lease %s delete sandbox %s attempt %d/%d failed (retrying): %v", reason, leaseID, sandboxID, attempt, attempts, err)
			time.Sleep(s.lostSandboxDeleteBackoff)
		}
	}
	s.log.Printf("%s: lease %s delete sandbox %s failed after %d attempt(s); leaving it to the orphan sweep: %v", reason, leaseID, sandboxID, attempts, err)
	s.rememberOrphanSandbox(sandboxID)
}

// rememberOrphanSandbox records a sandbox id whose Delete failed so the
// periodic orphan sweep retries it.
func (s *Service) rememberOrphanSandbox(sandboxID string) {
	if sandboxID == "" {
		return
	}
	s.orphanMu.Lock()
	s.orphanSandboxIDs[sandboxID] = struct{}{}
	s.orphanMu.Unlock()
}

// orphanSandboxSnapshot copies the pending orphan sandbox ids.
func (s *Service) orphanSandboxSnapshot() []string {
	s.orphanMu.Lock()
	defer s.orphanMu.Unlock()
	out := make([]string, 0, len(s.orphanSandboxIDs))
	for id := range s.orphanSandboxIDs {
		out = append(out, id)
	}
	return out
}

// setLostReason stamps l's loss reason once. A lease that dips in and out
// of the lost state keeps the reason it first lost for, like LostAt.
func setLostReason(l *Lease, reason string) {
	if l.LostReason == "" && reason != "" {
		l.LostReason = reason
	}
}

// recoverySummary is the reconcileCrash result and the
// POST /api/admin/reconcile response.
type recoverySummary struct {
	Recovered int `json:"recovered"`
	Lost      int `json:"lost"`
}

// pruneStaleLeaseRows drops lease rows with no in-memory twin whose
// sandbox is gone. It clears a row a checkpoint or pause that finished
// after its lease was released once wrote back (spoond-775): the save
// guard stops new ones, and this reconcile sweep clears any such row
// whose sandbox is gone. A row with an in-memory twin is left for
// reconcileCrash (a live lease whose sandbox vanished is recovered or
// lost), and a row whose sandbox still exists is left for
// ReconcileOrphans. A backend start loads every stored lease into
// memory, so a row an older binary left behind is not dropped here — it
// is loaded as a live lease and goes through the ordinary lost/grace
// path instead. Runs before the recovery pass so a phantom is never
// recovered.
func (s *Service) pruneStaleLeaseRows(ctx context.Context, present map[string]bool) {
	rows, err := s.db.ListLeases(ctx)
	if err != nil {
		s.log.Printf("reconcile: list lease rows: %v", err)
		return
	}
	s.store.mu.Lock()
	var stale []string
	for _, r := range rows {
		if _, ok := s.store.leases[r.ID]; ok {
			continue // a live twin: reconcileCrash owns it
		}
		if r.SandboxID != "" && present[r.SandboxID] {
			continue // its sandbox survives; ReconcileOrphans reclaims it
		}
		stale = append(stale, r.ID)
	}
	s.store.mu.Unlock()
	for _, id := range stale {
		s.log.Printf("reconcile: dropping stale lease row %s (no in-memory lease, sandbox gone)", id)
		if err := s.db.DeleteKeptBuilds(ctx, id); err != nil {
			s.log.Printf("reconcile: delete kept builds of stale lease %s: %v", id, err)
		}
		s.store.mu.Lock()
		s.deleteLeaseLocked(id)
		s.store.mu.Unlock()
	}
}

// reconcileCrash reconciles the lease state with the sandboxes that
// survived on the node. If List fails, nothing changes: leases are
// never marked lost on a list failure.
func (s *Service) reconcileCrash(ctx context.Context) recoverySummary {
	sbs, err := s.sub.List(ctx)
	if err != nil {
		s.log.Printf("reconcileCrash: list sandboxes failed: %v", err)
		return recoverySummary{}
	}
	present := make(map[string]bool, len(sbs))
	for _, sb := range sbs {
		present[sb.ID] = true
	}
	s.pruneStaleLeaseRows(ctx, present)

	s.store.mu.Lock()
	var targets []*Lease
	for _, l := range s.store.leases {
		if l.released || !l.live() || l.busy {
			continue
		}
		// A lease with a pending recovery retry for its current sandbox is
		// re-targeted even when that sandbox is present: a previous
		// attempt's cleanup delete can have failed, and the retry would
		// otherwise skip the lease for ever. A budget from a sandbox the
		// lease has since left must not bypass the check: the lease is
		// healthy and would be rolled back to an old checkpoint (B2).
		if present[l.SandboxID] && !s.recoveryPending(l.SandboxID) {
			continue
		}
		targets = append(targets, l)
	}
	s.store.mu.Unlock()

	var summary recoverySummary
	for _, l := range targets {
		// Mark the lease busy for the recovery, as crashTest does: the
		// rootfs liveness probe must not count transport failures against
		// (and at its threshold recover) a lease that a crash reconcile
		// is already recovering (spoond-5ca).
		if !s.trySetBusy(l) {
			continue
		}
		out := s.recoverOneLease(ctx, l)
		s.endBusy(l)
		switch out.Result {
		case "recovered":
			summary.Recovered++
		case "lost":
			summary.Lost++
		}
		// "recovering" (a transient failure within its retry budget) is
		// counted in neither: the lease is still in flight and the next
		// reconcile pass picks it up again.
	}

	// Pool entries whose sandbox is not live are dead.
	s.store.mu.Lock()
	for img, ids := range s.store.pool {
		kept := ids[:0]
		for _, id := range ids {
			if present[id] {
				kept = append(kept, id)
			} else {
				s.removePoolLocked(id)
			}
		}
		s.store.pool[img] = kept
	}
	s.store.mu.Unlock()

	// Recovered leases change the peer allowances (new host IPs).
	s.refreshPeersAsync(ctx)
	return summary
}

// recoveryOutcome is what recoverOneLease did with one lease: "recovered"
// (it came back from its checkpoint), "lost" (it had none, or its retry
// budget ran out), "recovering" (a transient failure left it for the
// next reconcile pass) or "released" (the lease was released while the
// recovery ran, so it was left released and nothing was saved).
// Generation and State are the lease's values after the call. The crash
// test reports it; the reconcile loop counts it.
type recoveryOutcome struct {
	Result     string `json:"result"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
}

// recoverOneLease runs the per-lease half of crash reconciliation for one
// lease whose sandbox has vanished: from its newest checkpoint when it has
// one (generation +1, event "recovered", state recovered), lost otherwise
// (event "lost"). It is shared by the startup/background reconcile loop
// and the crash test, so both take the identical path. The caller
// has already removed the lease's sandbox (a real or simulated crash);
// this function emits the lease events and writes the log lines.
//
// A recovery that fails with a transient error does not lose the lease:
// it keeps its running/recovered state (with no sandbox) and the next
// reconcile pass retries, bounded by RECOVERY_RETRY_ATTEMPTS and
// RECOVERY_RETRY_WINDOW. A substrate capacity refusal waits for room
// under the same window without counting an attempt (admission refusals
// cannot reach a live lease's recovery, which skips admission). A missing
// image or build is permanent and loses the lease at once (spoond-dxq).
func (s *Service) recoverOneLease(ctx context.Context, l *Lease) recoveryOutcome {
	if l.LastCheckpointBuildID == "" {
		// No checkpoint to recover from: the running state is gone.
		return s.loseRecovery(ctx, l, "no checkpoint to recover from; the running state is gone", "")
	}
	if err := s.recoverFromCheckpoint(ctx, l); err != nil {
		// The lease was released while its recovery create ran: the
		// release already stopped the guest it knew about and the
		// create's fresh one was stopped too (spoond-775, spoond-63a).
		// Nothing to lose and nothing to save.
		if errors.Is(err, errLeaseReleased) {
			s.clearRecoveryRetries(l)
			s.log.Printf("recovery: lease %s was released during its recovery; recovery stops", l.ID)
			return recoveryOutcome{Result: "released", Generation: l.Generation, State: l.State}
		}
		// A missing image or build can never succeed on a retry: give up
		// now with the cause.
		if recoveryFailurePermanent(err) {
			reason := fmt.Sprintf("recovery from checkpoint %s failed: %v", l.LastCheckpointBuildID, err)
			return s.loseRecovery(ctx, l, reason, err.Error())
		}
		// Transient: a busy node's envd start, a deadline, or a capacity
		// refusal that is waiting for room. A capacity refusal does not
		// count an attempt (the node may host the lease later) but starts
		// the window; anything else counts. Either way the lease keeps its
		// state with no sandbox and the next pass retries it, unless the
		// budget is spent.
		var attempts int
		var spent bool
		if recoveryWaitForCapacity(err) {
			attempts, spent = s.noteRetryWait(s.recoveryRetries, l.SandboxID, l.ID, s.recoveryRetryWindow())
		} else {
			attempts, spent = s.noteRetryFailure(s.recoveryRetries, l.SandboxID, l.ID, s.recoveryRetryLimit(), s.recoveryRetryWindow())
		}
		if !spent {
			// A failed attempt needs no cleanup here: createSandbox
			// already removed any half-started sandbox after a failed
			// Create (spoond-52c S2) and deletes nothing when the create
			// was refused before reaching the orchestrator, so a delete
			// here would double-delete and falsely name a resume cleanup
			// during a recovery (spoond-15i).
			// The owner sees the retry rather than a silent wait: the
			// event names the attempt and the cause (S3). A capacity wait
			// is not an attempt, so its text names only the wait.
			if recoveryWaitForCapacity(err) {
				s.emitLeaseEvent(l.ID, l.Owner, LeaseRetry, fmt.Sprintf("recovering from checkpoint %s: waiting for capacity: %v",
					shortEventBuildID(l.LastCheckpointBuildID), err))
				s.log.Printf("recovery: lease %s still recovering (waiting for capacity, checkpoint %s): %v",
					l.ID, formatRFC3339(l.LastCheckpointAt), err)
				return recoveryOutcome{Result: "recovering", Generation: l.Generation, State: l.State}
			}
			s.emitLeaseEvent(l.ID, l.Owner, LeaseRetry, fmt.Sprintf("recovering from checkpoint %s: attempt %d/%d failed: %v",
				shortEventBuildID(l.LastCheckpointBuildID), attempts, s.recoveryRetryLimit(), err))
			s.log.Printf("recovery: lease %s still recovering (attempt %d/%d, checkpoint %s): %v",
				l.ID, attempts, s.recoveryRetryLimit(), formatRFC3339(l.LastCheckpointAt), err)
			return recoveryOutcome{Result: "recovering", Generation: l.Generation, State: l.State}
		}
		reason := fmt.Sprintf("recovery from checkpoint %s failed after %d attempt(s) within %s: %v",
			l.LastCheckpointBuildID, attempts, s.recoveryRetryWindow(), err)
		return s.loseRecovery(ctx, l, reason, err.Error())
	}
	s.clearRecoveryRetries(l)
	s.emitLeaseEvent(l.ID, l.Owner, LeaseRecovered, fmt.Sprintf("recovered from checkpoint %s", l.LastCheckpointBuildID))
	s.log.Printf("recovery: lease %s recovered (checkpoint %s)", l.ID, formatRFC3339(l.LastCheckpointAt))
	return recoveryOutcome{Result: "recovered", Generation: l.Generation, State: l.State}
}

// loseRecovery is the one path that ends a lease's recovery in the lost
// state: reason is stored as its loss reason and carried by the lost
// event; cause, when non-empty, is appended to the log. It clears every
// recovery budget of the lease, drops any stale sandbox row, marks its
// running jobs lost and lets snapshot retention run. A lease released
// while the loss was in flight is left alone: no save, no lost event, no
// job marking (spoond-775).
func (s *Service) loseRecovery(ctx context.Context, l *Lease, reason, cause string) recoveryOutcome {
	s.clearRecoveryRetries(l)
	s.store.mu.Lock()
	released := l.released
	if !released {
		s.markLost(l, reason)
	}
	s.store.mu.Unlock()
	if released {
		return recoveryOutcome{Result: "lost", Generation: l.Generation, State: l.State}
	}
	// Lost means stopped (spoond-63a): delete whatever sandbox still holds
	// the lease's id — the half-started guest a failed recovery create
	// left behind, also when the retry budget is spent — and its row.
	s.stopLostSandbox(l.SandboxID, l.ID)
	s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, reason)
	if cause != "" {
		s.log.Printf("recovery: lease %s lost (checkpoint %s): %s", l.ID, formatRFC3339(l.LastCheckpointAt), cause)
	} else {
		s.log.Printf("recovery: lease %s lost (checkpoint %s)", l.ID, formatRFC3339(l.LastCheckpointAt))
	}
	s.markLeaseJobsLost(ctx, l.ID, l.Owner, "lease lost in a crash; the job did not survive")
	// A version this lost lease started from is no longer in use:
	// retention may drop it now (S5).
	s.rerunSnapshotRetention(ctx, l)
	return recoveryOutcome{Result: "lost", Generation: l.Generation, State: l.State}
}

// recoverFromCheckpoint resumes a lease from its checkpoint build with
// the same sandbox id (the UpsertSandbox in createSandbox replaces the
// stale row) and marks it recovered. The guest's memory did not continue
// from where its processes left it, so the generation bumps (2.2) and
// the new value is written into the guest.
func (s *Service) recoverFromCheckpoint(ctx context.Context, l *Lease) error {
	img, err := s.db.GetImage(ctx, l.Image)
	if err != nil {
		return err
	}
	b, err := s.db.GetBuild(ctx, l.LastCheckpointBuildID)
	if err != nil {
		return err
	}
	if !l.live() {
		// A suspended lease is uncharged (no hugepages), so turning its
		// sandbox back on here passes the memory check like any resume
		// (#128) — before the sandbox is created. The charge is the
		// image's CURRENT memory_mb (stamped below): the recovered
		// sandbox runs that value, so it is both what admission reserves
		// with and what the deferred release drops — the same rule as
		// restartCold and restore. Reserving with the lease's stale stamp
		// would admit an outdated charge (and leak the difference). A
		// running lease is already charged with its own stamp and
		// re-admits nothing. A recovery adds no lease, so only the
		// memory cap applies.
		if err := s.reserveQuota(l.Owner, 1, img.MemoryMB, false); err != nil {
			return err
		}
		defer s.releaseQuotaReservation(l.Owner, 1, img.MemoryMB)
		// Class re-admission (#128 part 2), as for a resume: recovery
		// brings hugepages back, so a burst lease re-passes the reserve
		// (the reconciler's own reserve work lands in #128 part 3; until
		// then a burst lease keeps its demand-burst standing here).
		class, err := s.admitClass(ctx, l.Owner, img.MemoryMB, l.Burst, l.ID)
		if err != nil {
			return err
		}
		l.Class = class
	}
	sb, err := s.createSandbox(ctx, img, b, true, l.SandboxID, l)
	if err != nil {
		return err
	}
	// A release that landed while the recovery create ran must not be
	// undone by the saves below: stop the fresh guest and leave the
	// lease released (spoond-775, spoond-63a). Do not rely on
	// createSandbox's own released check: the release that cleaned up
	// while the create was still in flight deleted a not-yet-registered
	// guest (a no-op), so the fresh sandbox is still here.
	if s.leaseReleased(l) {
		s.log.Printf("recovery: lease %s was released during its recovery create; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return errLeaseReleased
	}
	s.store.mu.Lock()
	if l.released {
		// Released while the recovery created its sandbox: stop the fresh
		// sandbox (bounded retries, spoond-63a) and leave no lease or
		// sandbox row behind (spoond-775).
		s.store.mu.Unlock()
		s.log.Printf("recovery: lease %s was released during its recovery; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return errLeaseReleased
	}
	l.setState("recovered")
	l.RecoveredFrom = l.LastCheckpointAt
	l.BuildID = l.LastCheckpointBuildID
	l.HostIP = sb.HostIP
	l.ExposedIP = sb.HostIP
	// The recovered sandbox runs the image's current memory_mb: the
	// lease keeps the charge it was admitted with (#128) — stamped 0
	// rows (pre-quota leases over a vanished image) stay uncharged.
	l.MemoryMB = img.MemoryMB
	s.bumpGenerationLocked(l)
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.endCreatingSandbox(sb.ID)
	s.writeGeneration(l)
	// Crash recovery rebuilt the guest from a checkpoint: its memory did
	// not continue, so every running job is lost (2.6, #135).
	s.markLeaseJobsLost(ctx, l.ID, l.Owner, "lease recovered from a checkpoint; the job did not survive")
	// Crash recovery replaced the sandbox; put the lease's create-time
	// secrets back into the fresh tmpfs (#80).
	s.restageCreateSecrets(ctx, l, "recovery")
	return nil
}
