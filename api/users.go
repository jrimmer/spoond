package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jrimmer/spoond/identity"
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
	MaxLeases    int           `json:"max_leases"`
	MaxTTL       int           `json:"max_ttl"`
}

func toUserView(u *identity.User) UserView {
	return UserView{
		ID:           u.ID,
		Name:         u.Name,
		Kind:         u.Kind,
		Admin:        u.Admin,
		Fingerprints: u.Fingerprints,
		CreatedAt:    u.CreatedAt,
		MaxLeases:    u.MaxLeases,
		MaxTTL:       u.MaxTTL,
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
		out = append(out, toUserView(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// handleUsersMe returns the caller's own user record. Available to any
// authenticated identity-store user (self-scoped, no directory leak).
func (s *Server) handleUsersMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u == nil {
		writeError(w, http.StatusNotFound, "no identity user for this token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u)})
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
	writeJSON(w, http.StatusCreated, map[string]any{"user": toUserView(u)})
}

// handleUsersByKey resolves an SSH key fingerprint to a user. The
// gateway calls this in its PublicKeyCallback.
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
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u)})
}

// handleUsersQuota updates a user's lease quota (admin only; T4/#31).
func (s *Server) handleUsersQuota(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	var req struct {
		MaxLeases int `json:"max_leases"`
		MaxTTL    int `json:"max_ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.MaxLeases < 0 || req.MaxTTL < 0 {
		writeError(w, http.StatusBadRequest, "quota values must be >= 0")
		return
	}
	if err := s.svc.identities.SetQuota(id, req.MaxLeases, req.MaxTTL); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	u := s.svc.identities.UserByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u)})
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
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u)})
}

// handleUsersDelete removes a user (admin only).
func (s *Server) handleUsersDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	if u := userFrom(r.Context()); u != nil && u.ID == id {
		writeError(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}
	if err := s.svc.identities.RemoveUser(id); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("remove: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
