package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Env is a per-PR (or per-branch) ephemeral environment: a persistent,
// workspace-backed sandbox bound to a repository ref, addressed by a
// stable key, and driven by Forgejo webhooks (create on PR open/sync,
// teardown on close).
//
// A nil SandboxID marks a *provisioning sentinel*: the key is reserved
// but the backing lease has not finished granting yet. This closes the
// check-then-create race between concurrent webhook deliveries for the
// same PR without holding the store lock across the (slow) forkd grant.
type Env struct {
	Key       string    `json:"key"`        // repo + "#" + ref
	Repo      string    `json:"repo"`       // owner/repo
	Ref       string    `json:"ref"`        // PR number or branch name
	SandboxID string    `json:"sandbox_id"` // lease id; "" = provisioning
	Image     string    `json:"image"`
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var (
	errEnvProvisioning = fmt.Errorf("environment provisioning in progress")
	errEnvNotFound     = fmt.Errorf("environment not found")
)

// EnvKey returns the stable key for a repo + ref environment.
func EnvKey(repo, ref string) string { return repo + "#" + ref }

// envName derives a stable, SSH-addressable friendly name from a repo+ref
// key. It is lowercase [a-z0-9-], starts with "env-", and is <= 63 chars,
// with an 8-hex hash suffix so distinct repos cannot collide on a name.
func envName(repo, ref string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(repo + "-" + ref) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	sum := sha256.Sum256([]byte(repo + "#" + ref))
	suffix := hex.EncodeToString(sum[:])[:8]
	prefix := "env-" + slug
	if slug == "" {
		prefix = "env"
	}
	name := prefix + "-" + suffix
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// ensureEnv returns the environment for repo+ref, creating it if absent.
// The second return value is true when a new environment was provisioned
// (vs an existing one being returned). Concurrent ensures for the same
// key observe the sentinel and receive errEnvProvisioning.
func (s *Service) ensureEnv(ctx context.Context, owner, repo, ref, image string) (*Env, bool, error) {
	key := EnvKey(repo, ref)
	s.store.mu.Lock()
	if e := s.store.envs[key]; e != nil {
		if e.SandboxID != "" {
			s.store.mu.Unlock()
			// Bump activity so the idle sweeper doesn't suspend a still-
			// active PR environment while the PR is receiving pushes.
			s.touch(e.SandboxID)
			return e, false, nil
		}
		s.store.mu.Unlock()
		return nil, false, errEnvProvisioning
	}
	// Reserve the key with a sentinel before the slow grant so concurrent
	// ensures can't double-create.
	s.store.envs[key] = &Env{Key: key, Repo: repo, Ref: ref, Image: image, Owner: owner, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	s.store.mu.Unlock()

	// Persistent leases are workspace-backed so the environment can
	// suspend/resume across PR pushes. Restricted egress is the secure
	// default (no guest-to-guest reach on the shared bridge).
	lease, err := s.grant(ctx, owner, image, 0, s.defaultTTL, true, string(PolicyRestricted), nil)
	if err != nil {
		s.store.mu.Lock()
		delete(s.store.envs, key)
		s.store.mu.Unlock()
		return nil, false, err
	}
	// Give the environment a stable, SSH-addressable name.
	if _, err := s.setName(owner, lease.ID, envName(repo, ref)); err != nil {
		s.release(ctx, lease)
		s.store.mu.Lock()
		delete(s.store.envs, key)
		s.store.mu.Unlock()
		return nil, false, fmt.Errorf("name environment: %w", err)
	}
	e := &Env{
		Key:       key,
		Repo:      repo,
		Ref:       ref,
		SandboxID: lease.ID,
		Image:     image,
		Owner:     owner,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.store.mu.Lock()
	s.store.envs[key] = e
	s.store.mu.Unlock()
	if s.metrics != nil {
		s.metrics.EnvCreated.Inc()
		s.metrics.EnvsActive.Inc()
	}
	return e, true, nil
}

// lookupEnv returns the environment for key, or nil.
func (s *Service) lookupEnv(key string) *Env {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.store.envs[key]
}

// listEnvs returns owner-scoped environments, optionally filtered by repo
// and ref (empty filter = all owned by owner).
func (s *Service) listEnvs(owner, repo, ref string) []*Env {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out = []*Env{}
	for _, e := range s.store.envs {
		if e.Owner != owner {
			continue
		}
		if repo != "" && e.Repo != repo {
			continue
		}
		if ref != "" && e.Ref != ref {
			continue
		}
		out = append(out, e)
	}
	return out
}

// teardownEnv removes the environment and releases its backing sandbox.
// Tearing down an already-gone environment is a no-op (idempotent), so
// repeated close/delete webhooks are safe.
func (s *Service) teardownEnv(ctx context.Context, key string) error {
	s.store.mu.Lock()
	e := s.store.envs[key]
	if e == nil {
		s.store.mu.Unlock()
		return errEnvNotFound
	}
	delete(s.store.envs, key)
	sid, owner := e.SandboxID, e.Owner
	s.store.mu.Unlock()
	if sid == "" {
		return nil // provisioning sentinel; no lease yet
	}
	if s.metrics != nil {
		s.metrics.EnvTeardown.Inc()
		s.metrics.EnvsActive.Dec()
	}
	if l := s.lookup(owner, sid); l != nil {
		s.release(ctx, l)
	}
	return nil
}

// firstOwner returns any single configured consumer owner. Used as the
// fallback for webhook-created environments in single-user deployments
// that don't set ENV_OWNER.
func (s *Service) firstOwner() string {
	for _, owner := range s.tokens {
		return owner
	}
	return ""
}

// handleEnvCreate provisions (or returns) a per-PR ephemeral environment.
// Body: {"repo": "owner/repo", "ref": "123", "image": "dev-base"}.
// image defaults to the server's configured environment image.
func (s *Server) handleEnvCreate(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	var req struct {
		Repo  string `json:"repo"`
		Ref   string `json:"ref"`
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Repo == "" || req.Ref == "" {
		writeError(w, http.StatusBadRequest, "repo and ref are required")
		return
	}
	image := req.Image
	if image == "" {
		image = s.envImage
	}
	if image == "" {
		writeError(w, http.StatusBadRequest, "image is required (no server default configured)")
		return
	}
	ok, err := s.reg.Has(r.Context(), image)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image registry unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown image tag: "+image)
		return
	}
	env, created, err := s.svc.ensureEnv(r.Context(), owner, req.Repo, req.Ref, image)
	if err != nil {
		if err == errEnvProvisioning {
			writeError(w, http.StatusConflict, "environment provisioning in progress; retry shortly")
			return
		}
		s.svc.log.Printf("env: ensure %s: %v", EnvKey(req.Repo, req.Ref), err)
		writeError(w, http.StatusInternalServerError, "failed to provision environment")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, env)
}

// handleEnvList lists owner-scoped environments, optionally filtered by
// repo and ref query params.
func (s *Server) handleEnvList(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"environments": s.svc.listEnvs(owner, r.URL.Query().Get("repo"), r.URL.Query().Get("ref")),
	})
}

// handleEnvDelete tears down an environment addressed by repo+ref query
// params.
func (s *Server) handleEnvDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	repo := r.URL.Query().Get("repo")
	ref := r.URL.Query().Get("ref")
	if repo == "" || ref == "" {
		writeError(w, http.StatusBadRequest, "repo and ref are required")
		return
	}
	if e := s.svc.lookupEnv(EnvKey(repo, ref)); e == nil || e.Owner != owner {
		writeError(w, http.StatusNotFound, "environment not found")
		return
	}
	if err := s.svc.teardownEnv(r.Context(), EnvKey(repo, ref)); err != nil {
		s.svc.log.Printf("env: teardown %s: %v", EnvKey(repo, ref), err)
		writeError(w, http.StatusInternalServerError, "failed to tear down environment")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// forgejoWebhookPayload is the subset of Forgejo's pull_request webhook
// payload the environment receiver needs.
type forgejoWebhookPayload struct {
	Action     string `json:"action"`
	Number     int64  `json:"number"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
	PullRequest struct {
		Number  int64  `json:"number"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
			Sha string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
			Sha string `json:"sha"`
		} `json:"base"`
	} `json:"pull_request"`
}

// handleForgejoWebhook receives Forgejo webhook deliveries and drives
// per-PR ephemeral environment lifecycle. It is auth-exempt (Forgejo
// presents no consumer token) and is instead verified by an HMAC-SHA256
// signature of the raw body using the shared WEBHOOK_SECRET.
func (s *Server) handleForgejoWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookSecret == "" {
		writeError(w, http.StatusNotFound, "webhook receiver disabled")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	sig := r.Header.Get("X-Forgejo-Signature")
	if sig == "" {
		sig = r.Header.Get("X-Gitea-Signature")
	}
	if sig == "" || !validWebhookSignature(s.webhookSecret, sig, body) {
		writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	event := r.Header.Get("X-Forgejo-Event")
	if event == "" {
		event = r.Header.Get("X-Gitea-Event")
	}
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "event": "ping"})
	case "pull_request", "pull_request_target":
		s.handlePullRequestWebhook(w, r, body)
	default:
		// Only PR lifecycle is in scope; push/issues/etc. are ignored.
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "event": event})
	}
}

// handlePullRequestWebhook applies a pull_request event to the environment
// lifecycle: open/reopen/synchronize ensure (create-or-reuse) an
// environment; close tears it down.
func (s *Server) handlePullRequestWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var p forgejoWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook payload")
		return
	}
	repo := p.Repository.FullName
	if repo == "" {
		writeError(w, http.StatusBadRequest, "repository.full_name missing")
		return
	}
	pr := p.Number
	if pr == 0 {
		pr = p.PullRequest.Number
	}
	if pr == 0 {
		writeError(w, http.StatusBadRequest, "pull request number missing")
		return
	}
	ref := strconv.FormatInt(pr, 10)
	key := EnvKey(repo, ref)

	switch p.Action {
	case "opened", "reopened", "synchronize", "edited", "ready_for_review", "labeled", "unlabeled":
		owner := s.envOwner
		if owner == "" {
			owner = s.svc.firstOwner()
		}
		if owner == "" {
			writeError(w, http.StatusInternalServerError, "no environment owner configured (set ENV_OWNER)")
			return
		}
		image := s.envImage
		if image == "" {
			writeError(w, http.StatusInternalServerError, "no environment image configured (set ENV_IMAGE)")
			return
		}
		env, created, err := s.svc.ensureEnv(r.Context(), owner, repo, ref, image)
		if err != nil {
			if err == errEnvProvisioning {
				writeJSON(w, http.StatusAccepted, map[string]any{"key": key, "status": "provisioning"})
				return
			}
			s.svc.log.Printf("webhook: ensure env %s: %v", key, err)
			writeError(w, http.StatusInternalServerError, "failed to provision environment")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, env)
	case "closed":
		_ = s.svc.teardownEnv(r.Context(), key)
		writeJSON(w, http.StatusOK, map[string]any{"key": key, "status": "torn_down"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"key": key, "status": "ignored", "action": p.Action})
	}
}

// validWebhookSignature verifies an HMAC-SHA256 hex signature against the
// raw payload body using the shared secret (constant-time compare).
// Accepts the optional "sha256=" prefix for GitHub-style compatibility.
func validWebhookSignature(secret, sig string, body []byte) bool {
	sig = strings.TrimPrefix(sig, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}
