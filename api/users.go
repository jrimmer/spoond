package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jrimmer/spoond/v4/identity"
)

// UserView is the JSON shape of a user in API responses. Token hashes are
// never returned; fingerprints are.
type UserView struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Kind         identity.Kind `json:"kind"`
	Admin        bool          `json:"admin"`
	Fingerprints []string      `json:"fingerprints"`
	CreatedAt    string        `json:"created_at"`
	MaxTTL       int           `json:"max_ttl"`
	// ExecConcurrency and GuestDial are the owner's per-identity
	// concurrency overrides: the cap on concurrent
	// exec/stream operations and on open guest-port dials. 0 (unset)
	// means the host defaults apply (EXEC_CONCURRENCY_MAX,
	// GUEST_DIAL_MAX).
	ExecConcurrency int `json:"exec_concurrency"`
	GuestDial       int `json:"guest_dial"`
	// EgressCeiling is the most network access the user's leases may
	// have (#164); absent = no ceiling.
	EgressCeiling *[]string `json:"egress_ceiling,omitempty"`
	// UsedMiB is the user's current memory charge (#128): the sum of
	// memory_mb over their RUNNING leases (suspended ones hold no
	// hugepages and are not charged).
	UsedMiB int `json:"used_mib"`
}

// toUserView renders a user for the API. usedMiB is the user's current
// running-lease memory charge in MiB (#128); the Service computes it.
func toUserView(u *identity.User, usedMiB int) UserView {
	return UserView{
		ID:              u.ID,
		Name:            u.Name,
		Kind:            u.Kind,
		Admin:           u.Admin,
		Fingerprints:    u.Fingerprints,
		CreatedAt:       u.CreatedAt,
		MaxTTL:          u.MaxTTL,
		ExecConcurrency: u.ExecConcurrency,
		GuestDial:       u.GuestDial,
		EgressCeiling:   u.EgressCeiling,
		UsedMiB:         usedMiB,
	}
}

// requireAdmin returns false and writes 403 when the caller is not an
// admin user. Legacy consumer-token callers (no identity user in
// context) are NOT admin — they can read but not mutate users. The one
// exception is bootstrap: when the store is empty, anyone authenticated
// may create the first (admin) user. When BOOTSTRAP_TOKEN is configured
// (security review #37 H3/M4), bootstrap additionally requires
// X-Bootstrap-Token to match it — otherwise any token holder (tokens
// live in world-readable unit files) could claim admin on a fresh
// deployment.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u := userFrom(r.Context())
	if u != nil && u.Admin {
		return true
	}
	if s.svc.identities != nil && s.svc.identities.Count() == 0 && r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/users") {
		if s.bootstrapToken != "" {
			bt := r.Header.Get("X-Bootstrap-Token")
			if bt == "" || subtle.ConstantTimeCompare([]byte(bt), []byte(s.bootstrapToken)) != 1 {
				writeError(w, http.StatusForbidden, "bootstrap token required")
				return false
			}
		}
		return true // bootstrap: first user creation is open
	}
	writeError(w, http.StatusForbidden, "admin required")
	return false
}

// handleUsersList returns all users. Admin-only: the full directory
// (admin flags, fingerprints, quotas) must not be enumerable by any
// token holder (security review #37 C1).
func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	users := s.svc.identities.Users()
	out := make([]UserView, 0, len(users))
	for _, u := range users {
		out = append(out, toUserView(u, s.svc.usedMiB(u.ID)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// handleUsersMe returns the caller's own user record. Available to any
// authenticated identity-store user (self-scoped, no directory leak).
// It carries the caller's fair-share slice and usage (#145 FS1) beside
// the user record.
func (s *Server) handleUsersMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u == nil {
		writeError(w, http.StatusNotFound, "no identity user for this token")
		return
	}
	share, _ := s.svc.fairShareFor(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":   toUserView(u, s.svc.usedMiB(u.ID)),
		"share":  share,
		"leases": s.svc.leaseCounts(u.ID),
	})
}

// handleUsage is GET /api/usage: the caller's own fair-share slice and
// usage (#145 FS1), self-scoped. A legacy consumer-token caller (no
// identity row) still resolves to their token owner, so its usage view
// is available too.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	share, ok := s.svc.fairShareFor(r.Context(), owner)
	if !ok {
		writeError(w, http.StatusNotFound, "no usage for this owner")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"share":   share,
		"leases":  s.svc.leaseCounts(owner),
		"waiting": s.svc.queuedCreates(owner),
		"recent":  s.svc.bus.recentFor(owner, usageRecentEvent, usageRecentLimit),
	})
}

// handleUsersByName resolves a username to a minimal identity (id +
// name only — no admin flag, no fingerprints, no quota). It exists so a
// lease owner can grant a share to another user by name (T6/#33)
// without exposing the full directory (security review #37 C1).
func (s *Server) handleUsersByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	u := s.svc.identities.UserByName(name)
	if u == nil {
		writeError(w, http.StatusNotFound, "no such user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]any{
		"id":   u.ID,
		"name": u.Name,
	}})
}

// handleIdentityStatus reports whether an identity store is configured.
// The SSH gateway probes this at startup: when a store is present, the
// backend by-key resolution is AUTHORITATIVE and the gateway's local
// allowlist is ignored for new connections (security review #37 H1) —
// the allowlist only applies in legacy single-user mode. Returns a
// boolean only; no user data.
func (s *Server) handleIdentityStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"identity_store": s.svc.identities != nil})
}

// handleUsersCreate registers a new user. Open during bootstrap (first
// user = admin, KTD-2); admin-only afterwards.
func (s *Server) handleUsersCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string        `json:"name"`
		Kind         identity.Kind `json:"kind"`
		Fingerprints []string      `json:"fingerprints"`
		Token        string        `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Kind != identity.KindPerson && req.Kind != identity.KindAgent {
		req.Kind = identity.KindPerson
	}
	if !s.requireAdmin(w, r) {
		return
	}
	u, err := s.svc.identities.AddUser(req.Name, req.Kind, req.Fingerprints, req.Token)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// A new owner changes N and every owner's slice (#145 FS1).
	s.svc.invalidateFairShares()
	writeJSON(w, http.StatusCreated, map[string]any{"user": toUserView(u, s.svc.usedMiB(u.ID))})
}

// handleUsersByKey resolves an SSH key fingerprint to a user. The
// gateway calls this in its PublicKeyCallback and only needs id + name;
// returning the full UserView (admin flag, all fingerprints, quotas) to
// any token holder would make it a directory oracle (security review
// #37 rescan F5).
func (s *Server) handleUsersByKey(w http.ResponseWriter, r *http.Request) {
	fp := strings.TrimSpace(r.URL.Query().Get("fingerprint"))
	if fp == "" {
		writeError(w, http.StatusBadRequest, "fingerprint query param is required")
		return
	}
	u := s.svc.identities.UserByFingerprint(fp)
	if u == nil {
		writeError(w, http.StatusNotFound, "no user for key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]any{
		"id":   u.ID,
		"name": u.Name,
	}})
}

// deprecatedQuotaWarning is the Warning header a quota request carries
// when it sets one of the per-owner caps removed in 3.0 (FS4).
const deprecatedQuotaWarning = `299 - "max_leases, guaranteed_mib, max_mib and max_kept_bytes are ignored since 3.0 (fair shares)"`

// deprecatedQuotaFields are the quota request fields 3.0 (fair shares,
// FS4) removed. A request may still carry them; they are ignored.
var deprecatedQuotaFields = []string{"max_leases", "guaranteed_mib", "max_mib", "max_kept_bytes"}

// handleUsersQuota updates a user's quota (admin only; T4/#31). Only
// max_ttl is left, plus the per-identity concurrency overrides
// : exec_concurrency sets that user's concurrent exec/stream
// cap and guest_dial the concurrent guest-port-dial cap, overriding the
// host defaults (EXEC_CONCURRENCY_MAX, GUEST_DIAL_MAX) for that
// identity alone. 0 or an omitted field means unset, the host default
// applies, and a negative value is 400. The per-owner caps max_leases,
// guaranteed_mib, max_mib and max_kept_bytes were removed in 3.0 (fair
// shares, FS4): a request may still send them, they are accepted and
// ignored, and when any is set (non-zero) the response carries
// Deprecation: true and a Warning naming them, so an old client learns
// without breaking.
func (s *Server) handleUsersQuota(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	var maxTTL int
	if v, ok := raw["max_ttl"]; ok {
		if err := json.Unmarshal(v, &maxTTL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid max_ttl: "+err.Error())
			return
		}
	}
	// The concurrency overrides read the same spellings as max_ttl: a
	// non-integer is 400, a negative is 400, and 0 (like an omitted
	// field) clears the override back to the host default.
	var execCap, dialCap int
	execSet, dialSet := false, false
	if v, ok := raw["exec_concurrency"]; ok {
		if err := json.Unmarshal(v, &execCap); err != nil {
			writeError(w, http.StatusBadRequest, "invalid exec_concurrency: "+err.Error())
			return
		}
		execSet = true
	}
	if v, ok := raw["guest_dial"]; ok {
		if err := json.Unmarshal(v, &dialCap); err != nil {
			writeError(w, http.StatusBadRequest, "invalid guest_dial: "+err.Error())
			return
		}
		dialSet = true
	}
	if maxTTL < 0 || execCap < 0 || dialCap < 0 {
		writeError(w, http.StatusBadRequest, "quota values must be >= 0")
		return
	}
	deprecated := false
	for _, f := range deprecatedQuotaFields {
		if v, ok := raw[f]; ok && setJSONValue(v) {
			deprecated = true
		}
	}
	if err := s.svc.identities.SetQuota(id, maxTTL); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// Unknown user is answered by the SetQuota refusal above; these two
	// only run when the request sent the field (a clearing 0 included),
	// so the user view below reflects every field the request sent and
	// 0 restores the host default for that identity.
	if execSet {
		if err := s.svc.identities.SetExecLimit(id, execCap); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
	}
	if dialSet {
		if err := s.svc.identities.SetDialLimit(id, dialCap); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
	}
	if deprecated {
		w.Header().Set("Deprecation", "true")
		w.Header().Set("Warning", deprecatedQuotaWarning)
	}
	u := s.svc.identities.UserByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u, s.svc.usedMiB(u.ID))})
}

// setJSONValue reports whether a raw JSON value sets something: not
// null, not a zero number, not false and not an empty string.
func setJSONValue(v json.RawMessage) bool {
	var x any
	if err := json.Unmarshal(v, &x); err != nil {
		return true
	}
	switch t := x.(type) {
	case nil:
		return false
	case float64:
		return t != 0
	case bool:
		return t
	case string:
		return t != ""
	default:
		return true
	}
}

// handleUsersLLMKey sets or rotates a user's LLM gateway key (admin
// only; U8/T8). The key is stored hashed and never returned in API
// responses. An empty llm_key clears the key, reverting that user's
// leases to the legacy open-gateway behavior.
func (s *Server) handleUsersLLMKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	var req struct {
		LLMKey string `json:"llm_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := s.svc.identities.SetLLMKey(id, req.LLMKey); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	u := s.svc.identities.UserByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u, s.svc.usedMiB(u.ID))})
}

// handleUsersEgressCeiling sets or clears a user's egress ceiling (#164,
// admin only): {"egress_ceiling": [...]} sets it, {"egress_ceiling":
// null} clears it. With "dry_run": true nothing is saved and the answer
// lists the user's existing leases the ceiling would narrow
// ("affected"); a real set returns the same list (a preview taken just
// before the save) and re-applies every live lease's egress at once.
func (s *Server) handleUsersEgressCeiling(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	v, ok := raw["egress_ceiling"]
	if !ok {
		writeError(w, http.StatusBadRequest, "egress_ceiling is required (a list, or null to clear)")
		return
	}
	var list []string
	clear := string(v) == "null"
	if !clear {
		if err := json.Unmarshal(v, &list); err != nil {
			writeError(w, http.StatusBadRequest, "egress_ceiling must be a list of strings or null")
			return
		}
	}
	var dryRun bool
	if d, ok := raw["dry_run"]; ok {
		if err := json.Unmarshal(d, &dryRun); err != nil {
			writeError(w, http.StatusBadRequest, "dry_run must be true or false")
			return
		}
	}
	if s.svc.identities.UserByID(id) == nil {
		writeError(w, http.StatusNotFound, "no such user")
		return
	}
	// The impact is computed on the normalized ceiling, as it would be
	// stored. Clearing narrows nothing.
	affected := []ceilingImpact{}
	if !clear {
		norm := make([]string, 0, len(list))
		for _, e := range list {
			n, err := identity.NormalizeCeilingEntry(e)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			norm = append(norm, n)
		}
		affected = s.svc.ceilingImpactOn(id, norm)
	}
	if dryRun {
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "affected": affected})
		return
	}
	if err := s.svc.identities.SetEgressCeiling(id, list, clear); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Apply it now: re-render every live lease's egress and update the
	// ones that changed, so a ceiling (or its removal) takes effect at
	// once rather than at the lease's next refresh.
	s.svc.refreshPeersAsync(context.WithoutCancel(r.Context()))
	u := s.svc.identities.UserByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u, s.svc.usedMiB(u.ID)), "affected": affected})
}

// handleUsersDelete lives in api/userdelete.go: it removes the identity
// and then cleans up the leases, jobs, snapshots and kept builds that
// would otherwise outlive it (spoond-q4j).
