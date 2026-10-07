package api

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Crash test (Honey M3): POST /api/leases/{id}/crash-test runs one lease
// through the crash-recovery path on demand. It deletes the lease's
// sandbox through the substrate directly — as a crash would, without
// releasing the lease or emitting released — deletes the sandbox row,
// then runs the same per-lease recovery the startup/background reconcile
// does (api/recovery.go): recover from the lease's last checkpoint
// (generation +1, event "recovered", state recovered) or mark it lost
// (event "lost"). A `crash_test` event precedes the recovery event, so a
// reader of the stream can tell a test from a real crash.
//
// The route exists only when the host sets CRASH_TEST (ServiceConfig
// CrashTest); otherwise it answers 404 like an unknown route. The caller
// is the lease's owner, or an admin for any lease; anyone else gets the
// same 404 as the other lease routes.
//
// It touches only its own lease. Note that "as a crash would" is
// deliberately not perfectly faithful: it calls substrate Delete (a real
// crash leaves the sandbox gone with no Delete call), so the fake
// substrate models the files as gone and a real node frees the VM. What
// matters is the recovery path, which is identical to the startup pass.

// crashTestTimeout bounds the delete and recovery of one crash test. They
// run detached from the request so a client disconnect cannot cut a
// recovery short and turn a recoverable lease into a lost one.
const crashTestTimeout = 10 * time.Minute

// crashResult is the POST /api/leases/{id}/crash-test response. Result is
// "recovered" or "lost"; Generation and State are the lease's values
// after the recovery.
type crashResult struct {
	ID         string `json:"id"`
	Result     string `json:"result"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
}

// handleCrashTest runs the crash test for one lease (Honey M3). Only when
// the host enables CRASH_TEST (404 "not found" otherwise, as for an
// unknown route). Owner or admin; 404 for anyone else and for an unknown
// or released lease. Refuses what a crash test must not touch: a busy
// lease (409), a suspended lease (409: nothing is running to crash) and
// an already lost lease (409 lease_lost).
func (s *Server) handleCrashTest(w http.ResponseWriter, r *http.Request) {
	if !s.svc.cfg.CrashTest {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	caller := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(caller, id)
	if lease == nil && isAdmin(r) {
		lease = s.svc.lookupAny(id)
	}
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	// Detached from the request: a client that hangs up mid-recovery must
	// not cancel it.
	ctx, cancel := context.WithTimeout(context.Background(), crashTestTimeout)
	defer cancel()
	out, err := s.svc.crashTest(ctx, lease, caller)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, crashResult{
			ID:         lease.ID,
			Result:     out.Result,
			Generation: out.Generation,
			State:      out.State,
		})
	case errors.Is(err, errLeaseBusy):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errSuspended):
		writeError(w, http.StatusConflict, "lease is suspended; nothing is running to crash")
	case errors.Is(err, errLost):
		writeLeaseLost(w, lease)
	default:
		s.svc.log.Printf("crash-test: lease %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "crash test failed")
	}
}

// errLost is returned by crashTest for a lease already lost.
var errLost = &leaseError{"lease is already lost"}

// crashTest simulates a crash of one lease and runs the shared per-lease
// recovery. It marks the lease busy for the operation (409 on a second
// caller), deletes the sandbox through the substrate directly (no
// release, no released event), drops the sandbox row and hands the lease
// to recoverOneLease. caller is the id of who crashed the lease: the
// owner, or an admin crashing someone else's lease. It names the caller
// in the log line and picks the crash_test event detail.
func (s *Service) crashTest(ctx context.Context, l *Lease, caller string) (recoveryOutcome, error) {
	s.store.mu.Lock()
	if l.busy {
		s.store.mu.Unlock()
		return recoveryOutcome{}, errLeaseBusy
	}
	if l.Suspended {
		s.store.mu.Unlock()
		return recoveryOutcome{}, errSuspended
	}
	if l.State == "lost" {
		s.store.mu.Unlock()
		return recoveryOutcome{}, errLost
	}
	l.busy = true
	byOwner := l.Owner == caller
	s.store.mu.Unlock()
	defer s.endBusy(l)

	s.log.Printf("crash-test: lease %s crashed by %s", l.ID, caller)

	// Delete the sandbox as a crash would: through the substrate directly,
	// without releasing the lease or emitting released. A failed delete is
	// logged and the recovery still runs — the sandbox is treated as gone,
	// exactly as reconcileCrash treats a sandbox that vanished.
	if err := s.sub.Delete(ctx, l.SandboxID); err != nil {
		s.log.Printf("crash-test: lease %s delete sandbox %s: %v", l.ID, l.SandboxID, err)
	}
	s.deleteSandboxRow(l.SandboxID)

	// The marker goes first: a stream reader sees crash_test, then
	// recovered/lost.
	detail := "crashed by an admin"
	if byOwner {
		detail = "crashed by its owner"
	}
	s.emitLeaseEvent(l.ID, l.Owner, LeaseCrashTest, detail)

	out := s.recoverOneLease(ctx, l)
	return out, nil
}
