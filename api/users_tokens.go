package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v3/identity"
)

// Named per-user tokens (A2, unified auth #138): several credentials
// beside the user's original single token, each with a name, revocable,
// optionally expiring. The secret is shown once at mint and stored only
// as the store's hashed form. The user itself and an admin manage a
// user's tokens; since A3 the env ADMIN_TOKEN also mints for any user
// on the POST route (the host root's break-glass). Anyone else gets the
// same 403/404 the other user routes answer.

// IsAdminBearerToken reports whether a request presents the configured
// ADMIN_TOKEN as its bearer token (constant time): the break-glass
// credential (#138 A3). False when unset, missing or wrong — the
// request then authenticates, or is refused, as whatever else it
// carries.
func (s *Server) IsAdminBearerToken(r *http.Request) bool {
	if s.adminToken == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(auth, "Bearer ")
	return tok != "" && tok != auth &&
		subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) == 1
}

// isAdminTokenMintPath reports whether p is the mint route's path shape
// — /api/users/{id}/tokens with exactly one path id — the only user
// route the ADMIN_TOKEN may authenticate (#138 A3). The GET list and
// DELETE revoke routes are addressed by the same path with other
// methods; the caller checks the method, so the break-glass stays on
// POST alone.
func isAdminTokenMintPath(p string) bool {
	const prefix = "/api/users/"
	if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, "/tokens") {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/tokens")
	return id != "" && !strings.Contains(id, "/")
}

// adminMintExemption decides, once per request, whether the
// authMiddleware exemption for the break-glass mint applies. It checks
// the token first, then binds the verdict to the mint route alone: the
// exemption was granted only when the shape matched, and a request
// whose path or method does not must never see the marker.
func (s *Server) adminMintExemption(r *http.Request) bool {
	if r.Context().Value(ctxAdminMint{}) != true {
		return false
	}
	if r.Method != http.MethodPost || !isAdminTokenMintPath(r.URL.Path) {
		// A stale marker (Handler cleared the common mismatches; keep
		// the guard for any future wiring).
		return false
	}
	if userFrom(r.Context()) != nil {
		// Admitted because a consumer token matched. That identity, not
		// the break-glass, answers this request (it can only have been
		// the mint route, which the middleware exempted it for).
		if !s.IsAdminBearerToken(r) {
			return false
		}
	}
	return true
}

// tokenView is one listing row: everything but the secret. A token
// with no expiry carries expires_at "".
type tokenView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
	LastUsedAt string `json:"last_used_at"`
}

func toTokenView(t identity.TokenRecord) tokenView {
	return tokenView{
		ID:         t.ID,
		Name:       t.Name,
		CreatedAt:  t.CreatedAt,
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
	}
}

// handleUserSubtreeGet dispatches the GET subtree under
// /api/users/{id}/: exactly the trailing segment "tokens" is the token
// list; every other tail is a plain 404, so the dispatcher cannot turn
// a typo into another route. It exists because Go's ServeMux refuses to
// register "GET /api/users/{id}/tokens" beside "GET
// /api/users/by-name/{name}" — neither pattern is more specific —
// while both documented routes must keep their shapes.
func (s *Server) handleUserSubtreeGet(w http.ResponseWriter, r *http.Request) {
	// r.URL.Path starts with "/api/users/" exactly when the mux matched
	// this pattern; a prefix mismatch means the route was reached
	// through some other mount, so fall through to the plain 404.
	const prefix = "/api/users/"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	tail := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		tail = strings.Trim(rest[i+1:], "/")
	}
	switch tail {
	case "tokens":
		s.handleUsersTokensList(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// tokenTargetUser resolves the path user for the token routes and
// checks the caller may manage it: the user itself or an admin. Anyone
// else is answered like the other owner-scoped user routes: an
// authenticated caller without permission gets 404 (a user they cannot
// see must look like one that does not exist); a caller with no
// identity user in context (a legacy consumer token) gets 403, as the
// admin-gated routes answer it.
func (s *Server) tokenTargetUser(w http.ResponseWriter, r *http.Request, id string) *identity.User {
	caller := userFrom(r.Context())
	owner := s.svc.identities.UserByID(id)
	if owner == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return nil
	}
	if caller != nil {
		if caller.Admin || caller.ID == owner.ID {
			return owner
		}
		writeError(w, http.StatusNotFound, "user not found")
		return nil
	}
	writeError(w, http.StatusForbidden, "admin required")
	return nil
}

// handleUsersTokensCreate is POST /api/users/{id}/tokens
// {"name","ttl_secs"?}: mints a named token, answers the secret once.
// 201 {id,name,token,created_at,expires_at}. 403 admin required when
// the caller has no identity user at all; 404 for an unknown target
// user and for a known one the caller may not see (owner-scoped shape).
// #138 A3: the env ADMIN_TOKEN (the host root's break-glass) also
// mints, for any user — during a maintenance window ttl_secs is
// recommended so the handed-out token dies with the window. GET and
// DELETE keep the A2 rules: break-glass there is the plain 401 any
// invalid token gets.
func (s *Server) handleUsersTokensCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	var req struct {
		Name    string `json:"name"`
		TTLSecs *int64 `json:"ttl_secs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	ttl := time.Duration(0)
	if req.TTLSecs != nil {
		if *req.TTLSecs < 0 || *req.TTLSecs > math.MaxInt64/int64(time.Second) {
			writeError(w, http.StatusBadRequest, "ttl_secs out of range")
			return
		}
		ttl = time.Duration(*req.TTLSecs) * time.Second
	}
	// Who may manage the target: the user itself or an admin user
	// (A2); the break-glass ADMIN_TOKEN for any user (A3); a legacy
	// consumer token nobody. 404 for a target the caller may not see,
	// like every other owner-scoped user route.
	var owner *identity.User
	adminMint := false
	switch caller := userFrom(r.Context()); {
	case s.adminMintExemption(r):
		adminMint = true
		if owner = s.svc.identities.UserByID(id); owner == nil {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		// A break-glass token is for a window: it must expire.
		if ttl <= 0 {
			writeError(w, http.StatusBadRequest, "a token minted with ADMIN_TOKEN needs ttl_secs > 0")
			return
		}
	case caller != nil:
		if !caller.Admin && caller.ID != id {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		owner = s.svc.identities.UserByID(id)
		if owner == nil {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
	default:
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	secret, rec, err := s.svc.identities.AddToken(owner.ID, req.Name, ttl)
	if err != nil {
		// A duplicate or reserved name, or the per-user cap.
		code := ""
		if errors.Is(err, identity.ErrTooManyTokens) {
			code = "too_many_tokens"
		}
		writeErrorCode(w, http.StatusConflict, code, err.Error())
		return
	}
	if adminMint {
		// The break-glass audit line: who got a token, which one —
		// never the secret.
		s.svc.log.Printf("token minted with ADMIN_TOKEN: user %s (%s) token %q id %s, expires_at %q",
			owner.Name, owner.ID, rec.Name, rec.ID, rec.ExpiresAt)
	} else {
		s.svc.log.Printf("token minted: user %s (%s) token %q id %s, expires_at %q",
			owner.Name, owner.ID, rec.Name, rec.ID, rec.ExpiresAt)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         rec.ID,
		"name":       rec.Name,
		"token":      secret,
		"created_at": rec.CreatedAt,
		"expires_at": rec.ExpiresAt,
	})
}

// handleUsersTokensList is GET /api/users/{id}/tokens: the user's
// tokens with name, id, created, expires and last used — never
// secrets. The legacy single token is listed as the token named
// "default".
func (s *Server) handleUsersTokensList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	owner := s.tokenTargetUser(w, r, id)
	if owner == nil {
		return
	}
	toks, err := s.svc.identities.ListTokens(owner.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	out := make([]tokenView, 0, len(toks))
	for _, t := range toks {
		out = append(out, toTokenView(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// handleUsersTokenRevoke is DELETE /api/users/{id}/tokens/{token_id}:
// revokes one token, 204. Revoking the "default" token takes away the
// user's original single token. Unknown token id is idempotent (204),
// like an unknown fingerprint on the key routes.
func (s *Server) handleUsersTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tokenID := strings.TrimSpace(r.PathValue("token_id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	if tokenID == "" {
		writeError(w, http.StatusBadRequest, "token id required")
		return
	}
	owner := s.tokenTargetUser(w, r, id)
	if owner == nil {
		return
	}
	if err := s.svc.identities.RevokeToken(owner.ID, tokenID); err != nil {
		// A foreign token id answers the same 404 as an unknown one,
		// so no caller can probe which ids exist on other users. Any
		// other error is the users file failing to save: the revoke
		// was rolled back, so say so (500), never "not found".
		if errors.Is(err, identity.ErrTokenNotFound) {
			writeError(w, http.StatusNotFound, "token not found")
			return
		}
		s.svc.log.Printf("token revoke: user %s token id %s: %v", owner.ID, tokenID, err)
		writeError(w, http.StatusInternalServerError, "revoke not saved; retry")
		return
	}
	s.svc.log.Printf("token revoked: user %s (%s) token id %s", owner.Name, owner.ID, tokenID)
	w.WriteHeader(http.StatusNoContent)
}
