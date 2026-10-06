package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Admin crash test (Honey M3): POST /api/admin/leases/{id}/crash runs one
// lease through the crash-recovery path on demand. It deletes the lease's
// sandbox through the substrate directly — as a crash would, without
// releasing the lease or emitting released — deletes the sandbox row,
// then runs the same per-lease recovery the startup/background reconcile
// does (api/recovery.go): recover from the lease's last checkpoint
// (generation +1, event "recovered", state recovered) or mark it lost
// (event "lost"). A `crash_test` event precedes the recovery event, so an
// operator reading the stream can tell a test from a real crash.
//
// It touches only its own lease. Note that "as a crash would" is
// deliberately not perfectly faithful: it calls substrate Delete (a real
// crash leaves the sandbox gone with no Delete call), so the fake
// substrate models the files as gone and a real node frees the VM. What
// matters is the recovery path, which is identical to the startup pass.

// crashResult is the POST /api/admin/leases/{id}/crash response. Result is
// "recovered" or "lost"; Generation and State are the lease's values after
// the recovery.
type crashResult struct {
	ID         string `json:"id"`
	Result     string `json:"result"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
}

// crashTestAdmin authenticates the crash-test route and returns a label
// naming the caller for the log line. The ADMIN_TOKEN always works; so
// does an identity-store admin user's token, so an operator can drive the
// test with their own credentials. A valid non-admin user token answers
// 403 (admin required), an unknown token 401, and no configured
// ADMIN_TOKEN 404 — matching the other admin routes' disabled/refused
// shapes.
func (s *Server) crashTestAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.adminToken == "" {
		writeError(w, http.StatusNotFound, "not found")
		return "", false
	}
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" || token == auth {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) == 1 {
		return "admin", true
	}
	if s.svc.identities != nil {
		if u := s.svc.identities.UserByToken(token); u != nil {
			if u.Admin {
				return u.ID, true
			}
			writeError(w, http.StatusForbidden, "admin required")
			return "", false
		}
	}
	writeError(w, http.StatusUnauthorized, "invalid admin token")
	return "", false
}

// handleAdminCrash runs the crash test for one lease (Honey M3). Admin
// only (403 for a non-admin, 401 for an unknown token, 404 when no admin
// token is configured). Refuses what a crash test must not touch: a busy
// lease (409), a suspended lease (409: nothing is running to crash), an
// already lost lease (410) and an unknown or released lease (404).
func (s *Server) handleAdminCrash(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.crashTestAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	lease := s.svc.lookupAny(id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	out, err := s.svc.crashTest(r.Context(), lease, admin)
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
		writeError(w, http.StatusGone, lostLeaseMessage)
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
// to recoverOneLease. admin is the name logged as having crashed the
// lease; it is only for the log line.
func (s *Service) crashTest(ctx context.Context, l *Lease, admin string) (recoveryOutcome, error) {
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
	s.store.mu.Unlock()
	defer s.endBusy(l)

	s.log.Printf("crash-test: lease %s crashed by %s", l.ID, admin)

	// Delete the sandbox as a crash would: through the substrate directly,
	// without releasing the lease or emitting released. A failed delete is
	// logged and the recovery still runs — the sandbox is treated as gone,
	// exactly as reconcileCrash treats a sandbox that vanished.
	if err := s.sub.Delete(ctx, l.SandboxID); err != nil {
		s.log.Printf("crash-test: lease %s delete sandbox %s: %v", l.ID, l.SandboxID, err)
	}
	s.deleteSandboxRow(l.SandboxID)

	// The operator marker goes first: a stream reader sees crash_test,
	// then recovered/lost.
	s.emitLeaseEvent(l.ID, l.Owner, LeaseCrashTest, "crashed by an admin")

	out := s.recoverOneLease(ctx, l)
	return out, nil
}
