package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v2/metrics"
)

// leaseHeartbeatPrefix is the path prefix of the guest-service lease
// heartbeat: POST /lease/<lease-id>/active. It rides the guest-service
// listener (HOST_GUEST_SERVICE_PORT, the one that also serves the
// per-lease LLM gateway at /llm/<lease-id>/...), which every network
// policy — restricted included — permits.
const leaseHeartbeatPrefix = "/lease/"

// heartbeatWriteInterval is the per-lease write floor: at most one
// LastActive write per lease per window. Calls inside the window still
// answer 204 (the guest keeps working) but skip the write, so a buggy or
// hostile guest cannot hammer SQLite.
const heartbeatWriteInterval = 60 * time.Second

// leaseHeartbeat answers the guest heartbeat route. Authorization is the
// lease id itself (capability), exactly like /llm/<lease-id>/: the id is
// unguessable and every sandbox receives SPOOND_LEASE_ID plus
// SPOOND_GATEWAY_URL in its exec environment, while an agent working
// inside a lease holds no owner token. The lookup is the same
// owner-blind, released-rejecting lookup the LLM gateway uses.
type leaseHeartbeat struct {
	svc     *Service
	metrics *metrics.BackendMetrics // nil in tests that build a bare handler
	// writeMu guards lastWrite.
	writeMu sync.Mutex
	// lastWrite maps lease id -> time of its last LastActive write.
	lastWrite map[string]time.Time
}

// newLeaseHeartbeat builds the heartbeat handler.
func newLeaseHeartbeat(svc *Service) *leaseHeartbeat {
	return &leaseHeartbeat{svc: svc, lastWrite: map[string]time.Time{}}
}

// ServeHTTP handles POST /lease/<lease-id>/active. 204 on success, 404
// for an unknown or released lease, 409 for a suspended one, 405 for any
// other method. Responses carry no body and never reveal anything about
// other leases.
func (h *leaseHeartbeat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	leaseID, rest := splitHeartbeatPath(r.URL.Path)
	if leaseID == "" || rest != "/active" {
		http.Error(w, "usage: POST /lease/<lease-id>/active", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The same capability lookup the LLM gateway performs: a released
	// lease is indistinguishable from an unknown one.
	lease := h.svc.lookupAny(leaseID)
	if lease == nil {
		h.forgetLastWrite(leaseID)
		http.Error(w, "lease not found", http.StatusNotFound)
		return
	}
	if lease.Suspended {
		http.Error(w, "lease is suspended; resume it first", http.StatusConflict)
		return
	}
	if h.claimWrite(leaseID) {
		// A heartbeat is activity, nothing more: it sets LastActive (the
		// state the idle sweep reads) and persists it. It must not extend
		// ExpiresAt, change persistence, resume a suspended lease or do
		// anything else.
		h.svc.markActive(lease.ID)
	}
	if h.metrics != nil {
		h.metrics.LeaseHeartbeats.Inc()
	}
	w.WriteHeader(http.StatusNoContent)
}

// claimWrite reports whether this heartbeat may write LastActive for the
// lease (at most one write per lease per heartbeatWriteInterval).
func (h *leaseHeartbeat) claimWrite(leaseID string) bool {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	now := time.Now()
	if t, ok := h.lastWrite[leaseID]; ok && now.Sub(t) < heartbeatWriteInterval {
		return false
	}
	h.lastWrite[leaseID] = now
	return true
}

// forgetLastWrite drops a released lease's rate-limit entry so the map
// stays bounded by the live lease count.
func (h *leaseHeartbeat) forgetLastWrite(leaseID string) {
	h.writeMu.Lock()
	delete(h.lastWrite, leaseID)
	h.writeMu.Unlock()
}

// splitHeartbeatPath splits a /lease/ path into the lease id and the
// remainder: /lease/<id>/active -> ("<id>", "/active").
func splitHeartbeatPath(p string) (leaseID, rest string) {
	rest = strings.TrimPrefix(p, leaseHeartbeatPrefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i:]
	}
	return rest, ""
}
