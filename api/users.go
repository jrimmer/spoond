package api

import (
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
// may create the first (admin) user.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u := userFrom(r.Context())
	if u != nil && u.Admin {
		return true
	}
	if s.svc.identities != nil && s.svc.identities.Count() == 0 && r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/users") {
		return true // bootstrap: first user creation is open
	}
	writeError(w, http.StatusForbidden, "admin required")
	return false
}

// handleUsersList returns all users.
func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	users := s.svc.identities.Users()
	out := make([]UserView, 0, len(users))
	for _, u := range users {
		out = append(out, toUserView(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
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
