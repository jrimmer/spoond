package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// shareView is the JSON shape of a share.
type shareView struct {
	LeaseID   string `json:"lease_id"`
	Grantee   string `json:"grantee"`
	Mode      string `json:"mode"`
	ExpiresAt string `json:"expires_at,omitempty"`
	CreatedAt string `json:"created_at"`
}

func toShareView(sh *Share) shareView {
	v := shareView{
		LeaseID:   sh.LeaseID,
		Grantee:   sh.Grantee,
		Mode:      string(sh.Mode),
		CreatedAt: sh.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !sh.ExpiresAt.IsZero() {
		v.ExpiresAt = sh.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return v
}

// handleShareGrant shares a lease with another user (T6/#33).
// Owner-only. Body: {"grantee":"<user-id>","mode":"ssh|http","ttl":<seconds>}
func (s *Server) handleShareGrant(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Grantee string `json:"grantee"`
		Mode    string `json:"mode"`
		TTL     int    `json:"ttl"` // seconds; 0 = never expires
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Grantee = strings.TrimSpace(req.Grantee)
	if req.Grantee == "" {
		writeError(w, http.StatusBadRequest, "grantee is required")
		return
	}
	mode := ShareMode(req.Mode)
	if mode == "" {
		mode = ShareHTTP
	}
	if err := s.svc.GrantShare(owner, id, req.Grantee, mode, time.Duration(req.TTL)*time.Second); err != nil {
		if strings.Contains(err.Error(), "sandbox not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"shared": true, "lease_id": id, "grantee": req.Grantee, "mode": string(mode)})
}

// handleShareRevoke removes a grant (T6/#33). Owner-only.
func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	grantee := r.PathValue("grantee")
	if grantee == "" {
		writeError(w, http.StatusBadRequest, "grantee is required")
		return
	}
	if err := s.svc.RevokeShare(owner, id, grantee); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleShareList lists shares granted on the caller's leases (T6/#33).
func (s *Server) handleShareList(w http.ResponseWriter, r *http.Request) {
	shares := s.svc.ListShares(ownerFrom(r.Context()))
	out := make([]shareView, 0, len(shares))
	for _, sh := range shares {
		out = append(out, toShareView(sh))
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": out})
}
