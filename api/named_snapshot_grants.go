package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jrimmer/spoond/v3/store"
)

// Named-snapshot grants (#141): an owner lets another user start leases
// from one of its names, so one warm-up (a checkout plus build caches)
// serves both. A grant covers the name, not a version; only versions
// saved from a lease that never had a secret (secret_free) can be
// granted or started through a grant. The grantee is charged the
// lease's memory; the snapshot's disk stays the owner's. Revoking stops
// new starts and never touches a running lease.

// grantView is the JSON shape of a grant.
type grantView struct {
	Owner     string `json:"owner"`
	Name      string `json:"name"`
	Grantee   string `json:"grantee"`
	CreatedAt string `json:"created_at"`
}

func toGrantView(g store.NamedSnapshotGrantRow) grantView {
	return grantView{Owner: g.Owner, Name: g.Name, Grantee: g.Grantee, CreatedAt: formatRFC3339(g.CreatedAt)}
}

// resolveUserRef maps a user name or id to the owner id leases and
// snapshots are keyed by: an identity-store user by id, then by name,
// then the owner of a legacy consumer token. ok is false when no such
// user exists.
func (s *Service) resolveUserRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	if s.identities != nil {
		if u := s.identities.UserByID(ref); u != nil {
			return u.ID, true
		}
		if u := s.identities.UserByName(ref); u != nil {
			return u.ID, true
		}
	}
	if s.legacyTokenOwner(ref) {
		return ref, true
	}
	return "", false
}

// checkNamedSnapshotGrant returns nil when owner granted name to
// grantee, store.ErrNotFound when not, or a store error.
func (s *Service) checkNamedSnapshotGrant(ctx context.Context, owner, name, grantee string) error {
	_, err := s.db.GetNamedSnapshotGrant(ctx, owner, name, grantee)
	return err
}

// handleNamedSnapshotGrantCreate is POST /api/named-snapshot-grants:
// {"name": "<name>", "grantee": "<user name or id>"}. Owner-scoped: the
// caller grants one of its own names. 201 with the grant (a repeat
// answers the existing grant); 404 not_found when the caller has no
// such name; 409 not_secret_free when the name's latest version was
// saved from a lease that had a secret.
func (s *Server) handleNamedSnapshotGrantCreate(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	var req struct {
		Name    string `json:"name"`
		Grantee string `json:"grantee"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := validateNamedSnapshotName(req.Name); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(req.Grantee) == "" {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "grantee is required")
		return
	}
	grantee, ok := s.svc.resolveUserRef(req.Grantee)
	if !ok {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "no such user: "+req.Grantee)
		return
	}
	if grantee == owner {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "grantee must be a different user")
		return
	}
	ctx := r.Context()
	latest, err := s.svc.db.GetNamedSnapshotLatest(ctx, owner, req.Name)
	if errors.Is(err, store.ErrNotFound) {
		errNamedNotFound().write(w)
		return
	}
	if err != nil {
		s.svc.log.Printf("snapshot grant: read %s/%s: %v", owner, req.Name, err)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "failed to read snapshot")
		return
	}
	if !latest.SecretFree {
		errNotSecretFree(latest.Name, latest.Version).write(w)
		return
	}
	g, err := s.svc.db.InsertNamedSnapshotGrant(ctx, store.NamedSnapshotGrantRow{
		Owner: owner, Name: req.Name, Grantee: grantee, CreatedAt: s.svc.now(),
	})
	if err != nil {
		s.svc.log.Printf("snapshot grant: insert %s/%s to %s: %v", owner, req.Name, grantee, err)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "failed to save grant")
		return
	}
	writeJSON(w, http.StatusCreated, toGrantView(g))
}

// handleNamedSnapshotGrantList is GET /api/named-snapshot-grants: the
// grants the caller gave and the grants it received.
func (s *Server) handleNamedSnapshotGrantList(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	given, err := s.svc.db.NamedSnapshotGrantsGiven(r.Context(), owner)
	if err != nil {
		s.svc.log.Printf("snapshot grants: list given by %s: %v", owner, err)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "failed to list grants")
		return
	}
	received, err := s.svc.db.NamedSnapshotGrantsReceived(r.Context(), owner)
	if err != nil {
		s.svc.log.Printf("snapshot grants: list received by %s: %v", owner, err)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "failed to list grants")
		return
	}
	out := map[string][]grantView{"given": {}, "received": {}}
	for _, g := range given {
		out["given"] = append(out["given"], toGrantView(g))
	}
	for _, g := range received {
		out["received"] = append(out["received"], toGrantView(g))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleNamedSnapshotGrantDelete is DELETE
// /api/named-snapshot-grants?name=<name>&grantee=<user>: revoke one
// grant the caller gave. 204, or 404 not_found when there is no such
// grant. New starts through it stop; running leases are untouched.
func (s *Server) handleNamedSnapshotGrantDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	name := r.URL.Query().Get("name")
	ref := strings.TrimSpace(r.URL.Query().Get("grantee"))
	if name == "" || ref == "" {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "name and grantee are required")
		return
	}
	// A grantee that no longer resolves (a removed user) is matched by
	// the raw value, so a stale row can still be revoked.
	grantee, ok := s.svc.resolveUserRef(ref)
	if !ok {
		grantee = ref
	}
	err := s.svc.db.DeleteNamedSnapshotGrant(r.Context(), owner, name, grantee)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, "not_found", "grant not found")
		return
	}
	if err != nil {
		s.svc.log.Printf("snapshot grant: revoke %s/%s from %s: %v", owner, name, grantee, err)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "failed to revoke grant")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
