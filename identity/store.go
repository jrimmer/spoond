// Package identity implements the spoond user/identity store (epic #26 T1).
//
// A User is a person or an agent with its own SSH key fingerprints and
// API token hash. The first user registered becomes the admin (KTD-2:
// "first user is admin"). The store is the single source of truth for
// key→user and token→user resolution; the gateway and the API both ask it.
//
// Persistence: the store is written to a JSON file on every mutation
// (atomic write via temp+rename). A nil filename keeps the store
// in-memory only (tests).
package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind is the type of user: a person or an agent (KTD-1: agents get their
// own keypair, leases, quota, and audit trail).
type Kind string

const (
	KindPerson Kind = "person"
	KindAgent  Kind = "agent"
)

const (
	// defaultTokenName is the name every legacy TokenHash is presented as
	// (A2, unified auth #138): a user's existing single token keeps working
	// unchanged and shows up in ListTokens under this name, revocable like
	// any other token.
	defaultTokenName = "default"
	// legacyTokenID is the stable id of that presented legacy token.
	legacyTokenID = "t-default"
	// maxTokensPerUser caps a user's named tokens: each mint rewrites the
	// users file under the store's lock, so an unbounded list would let
	// one user slow every login.
	maxTokensPerUser = 50
	// lastUsedResolution is how stale a token's last-used stamp may get
	// before an authentication refreshes it (and writes the users file).
	lastUsedResolution = 5 * time.Minute
)

// TokenRecord is one named bearer token of a user (A2, unified auth
// #138). Only the HMAC/SHA256 hash of the secret is stored, never the
// secret itself. ExpiresAt "" means the token never expires; LastUsedAt
// "" means it has never authenticated anyone.
type TokenRecord struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hash       string `json:"hash,omitempty"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

// expired reports whether the token's expiry has passed (or the
// record carries an unparseable expiry, which is treated as expired:
// an unreadable date must not extend access).
func (t TokenRecord) expired(now time.Time) bool {
	if t.ExpiresAt == "" {
		return false
	}
	d, err := time.Parse(time.RFC3339, t.ExpiresAt)
	if err != nil {
		return true
	}
	return !d.After(now)
}

// User is a first-class identity: a person or an agent.
type User struct {
	ID           string   `json:"id"`           // stable user id (e.g. "u-<hex>")
	Name         string   `json:"name"`         // display/ctl name, unique
	Kind         Kind     `json:"kind"`         // person | agent
	Admin        bool     `json:"admin"`        // KTD-2: first user is admin
	Fingerprints []string `json:"fingerprints"` // SHA256 fingerprints of SSH keys
	TokenHash    string   `json:"token_hash"`   // SHA256 of the bearer token ("" = none); presented as the token named "default"
	// Tokens are the named per-user tokens (A2, unified auth #138). The
	// legacy token keeps TokenHash; new tokens append here. A file
	// written before this field existed loads unchanged with nil Tokens.
	Tokens []TokenRecord `json:"tokens,omitempty"`
	// LastUsedAt backs the legacy TokenHash's presented "default"
	// token record: it is refreshed by UserByToken when the legacy
	// token authenticates. Named tokens carry their own stamps in
	// their TokenRecord.
	LastUsedAt string `json:"last_used_at,omitempty"`
	// LLMKeyHash is the SHA256 of the user's LLM gateway key (U8/T8,
	// "" = none). When set, /llm/ requests on this user's leases must
	// present the matching key; the API layer never exposes it.
	LLMKeyHash string `json:"llm_key_hash"` // SHA256 of the LLM gateway key ("" = none)
	CreatedAt  string `json:"created_at"`   // RFC3339
	// Quota (T4/#31): only the TTL cap is left. The per-owner caps
	// max_leases, guaranteed_mib, max_mib and max_kept_bytes were removed
	// in 3.0 (fair shares, FS4); a users file written before still loads
	// (unknown JSON fields are ignored) and the next save drops them.
	MaxTTL int `json:"max_ttl"` // max lease TTL seconds (0 = global max applies)
}

// ErrTooManyTokens is AddToken's answer once a user holds
// maxTokensPerUser named tokens.
var ErrTooManyTokens = fmt.Errorf("a user may hold at most %d tokens; revoke one first", maxTokensPerUser)

// ErrTokenNotFound is RevokeToken's answer for a token id the user does
// not have: never minted, already revoked, or another user's.
var ErrTokenNotFound = errors.New("token not found")

// Store is a thread-safe user registry with optional JSON persistence.
type Store struct {
	mu        sync.RWMutex
	users     map[string]*User  // by id
	byFP      map[string]string // fingerprint -> user id
	byToken   map[string]string // token hash -> user id
	byName    map[string]string // name -> user id
	byTokenID map[string]string // token id -> user id (named tokens)
	file      string
	// salt (security review #37 M1) makes stored token/LLM-key hashes
	// HMAC-SHA256(salt, secret) instead of unsalted SHA256, defeating
	// offline dictionary attacks if the users file leaks. It is loaded
	// from <file>.salt (or generated for a fresh store); a pre-existing
	// store without a sidecar stays in legacy plain-SHA256 mode so old
	// hashes keep verifying. The salt never changes once chosen.
	salt []byte
}

// NewStore creates an empty store. filename may be "" for in-memory-only.
func NewStore(filename string) (*Store, error) {
	s := &Store{
		users:     map[string]*User{},
		byFP:      map[string]string{},
		byToken:   map[string]string{},
		byName:    map[string]string{},
		byTokenID: map[string]string{},
		file:      filename,
	}
	if filename != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
		if err := s.loadSalt(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// loadSalt reads (or creates) the per-store salt sidecar. A fresh store
// (no users yet) gets a salt generated now; an existing legacy store
// without a sidecar keeps nil salt so its plain SHA256 hashes still
// verify — new writes then also stay plain for consistency until the
// store is re-seeded (documented migration path: recreate users or
// rotate tokens after setting the salt).
func (s *Store) loadSalt() error {
	sidecar := s.file + ".salt"
	data, err := os.ReadFile(sidecar)
	if err == nil {
		s.salt, err = hex.DecodeString(strings.TrimSpace(string(data)))
		return err
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("read salt: %w", err)
	}
	if len(s.users) > 0 {
		return nil // legacy store, keep plain SHA256
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if err := os.WriteFile(sidecar, []byte(hex.EncodeToString(salt)), 0o600); err != nil {
		return fmt.Errorf("write salt: %w", err)
	}
	s.salt = salt
	return nil
}

// hashSecret hashes a bearer token or LLM gateway key for storage and
// comparison. With a salt configured this is HMAC-SHA256(salt, secret)
// (security review #37 M1); legacy stores without a salt keep plain
// SHA256 so pre-existing hashes verify.
func (s *Store) hashSecret(secret string) string {
	if len(s.salt) > 0 {
		mac := hmac.New(sha256.New, s.salt)
		_, _ = mac.Write([]byte(secret))
		return hex.EncodeToString(mac.Sum(nil))
	}
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read users file: %w", err)
	}
	var users []*User
	if err := json.Unmarshal(data, &users); err != nil {
		return fmt.Errorf("parse users file: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range users {
		s.index(u)
	}
	return nil
}

// index adds a user to the lookup maps. Caller holds the lock.
func (s *Store) index(u *User) {
	s.users[u.ID] = u
	s.byName[u.Name] = u.ID
	for _, fp := range u.Fingerprints {
		s.byFP[fp] = u.ID
	}
	if u.TokenHash != "" {
		s.byToken[u.TokenHash] = u.ID
	}
	// Every named token's hash resolves to its owner too (A2); every
	// token id is indexed for cross-user revoke checks.
	for i := range u.Tokens {
		if h := u.Tokens[i].Hash; h != "" {
			s.byToken[h] = u.ID
		}
		if id := u.Tokens[i].ID; id != "" {
			s.byTokenID[id] = u.ID
		}
	}
}

// deindexToken removes one token's hash from byToken. Caller holds the
// lock. A hash that is another user's active hash is never seen here:
// AddToken refuses duplicates across users.
func (s *Store) deindexToken(u *User, h string) {
	if h != "" && s.byToken[h] == u.ID {
		delete(s.byToken, h)
	}
}

// save persists the store. Caller holds the lock.
func (s *Store) save() error {
	if s.file == "" {
		return nil
	}
	users := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

// FingerprintSHA256 returns the standard ssh SHA256 fingerprint for a
// marshaled public key.
func FingerprintSHA256(marshaled []byte) string {
	h := sha256.Sum256(marshaled)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

// AddUser registers a new user. The FIRST user becomes admin (KTD-2).
// Returns the created user.
func (s *Store) AddUser(name string, kind Kind, fingerprints []string, token string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if _, exists := s.byName[name]; exists {
		return nil, fmt.Errorf("user %q already exists", name)
	}
	// First user is admin.
	admin := len(s.users) == 0
	u := &User{
		ID:        newID(),
		Name:      name,
		Kind:      kind,
		Admin:     admin,
		CreatedAt: nowRFC3339(),
	}
	for _, fp := range fingerprints {
		fp = strings.TrimSpace(fp)
		if fp == "" {
			continue
		}
		if owner, dup := s.byFP[fp]; dup {
			return nil, fmt.Errorf("key %s already belongs to user %s", fp, owner)
		}
		u.Fingerprints = append(u.Fingerprints, fp)
	}
	if token != "" {
		u.TokenHash = s.hashSecret(token)
		if owner, dup := s.byToken[u.TokenHash]; dup {
			return nil, fmt.Errorf("token already belongs to user %s", owner)
		}
	}
	s.index(u)
	if err := s.save(); err != nil {
		return nil, err
	}
	return u, nil
}

// UserByID returns the user with the given id, or nil.
func (s *Store) UserByID(id string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users[id]
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

// UserByName returns the user with the given name, or nil.
func (s *Store) UserByName(name string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byName[strings.TrimSpace(name)]
	if !ok {
		return nil
	}
	u := s.users[id]
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

// UserByFingerprint resolves an SSH public-key fingerprint to a user.
func (s *Store) UserByFingerprint(fp string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byFP[fp]
	if !ok {
		return nil
	}
	u := s.users[id]
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

// hashTokenIndex looks up the user id for a token hash and updates the
// matching token's last-used stamp when it is later than the stored one
// (an earlier stamp must never move back). Caller holds the lock.
// Returns the user id; ok is false when the hash resolves to nothing.
func (s *Store) hashTokenIndex(h string) (string, bool) {
	id, ok := s.byToken[h]
	if !ok {
		return "", false
	}
	u := s.users[id]
	if u == nil {
		return "", false
	}
	// The stamp moves only when the stored one is older than
	// lastUsedResolution: every request authenticates through here, and
	// a users-file write per second per token would serialise them.
	now := nowRFC3339()
	stale := nowUTC().Add(-lastUsedResolution).Format(time.RFC3339)
	stamped := false
	if u.TokenHash == h && u.LastUsedAt < stale {
		u.LastUsedAt = now
		stamped = true
	}
	for i := range u.Tokens {
		if u.Tokens[i].Hash == h && u.Tokens[i].LastUsedAt < stale {
			u.Tokens[i].LastUsedAt = now
			stamped = true
		}
	}
	if stamped && s.file != "" {
		// Best-effort: a failed write must not fail the auth this call
		// answered; the stamp is retried on the next use.
		_ = s.save()
	}
	return id, true
}

// UserByToken resolves a bearer token to a user by its hash. An expired
// token (ExpiresAt set and past, A2) authenticates nobody; its record is
// never updated.
func (s *Store) UserByToken(token string) *User {
	h := s.hashSecret(token)
	s.mu.RLock()
	id, ok := s.byToken[h]
	u := s.users[id]
	if !ok || u == nil || s.tokenHashExpiredLocked(u, h) {
		s.mu.RUnlock()
		return nil
	}
	cp := *u
	due := s.stampDueLocked(u, h)
	s.mu.RUnlock()
	// Most logins only read: the exclusive lock (and a users-file write)
	// is taken only when this token's last-used stamp is due.
	if due {
		s.mu.Lock()
		if _, ok := s.hashTokenIndex(h); !ok {
			s.mu.Unlock()
			return nil // revoked between the two locks
		}
		s.mu.Unlock()
	}
	return &cp
}

// stampDueLocked reports whether the token with hash h has a last-used
// stamp older than lastUsedResolution. Caller holds the lock.
func (s *Store) stampDueLocked(u *User, h string) bool {
	stale := nowUTC().Add(-lastUsedResolution).Format(time.RFC3339)
	if u.TokenHash == h {
		return u.LastUsedAt < stale
	}
	for _, t := range u.Tokens {
		if t.Hash == h {
			return t.LastUsedAt < stale
		}
	}
	return false
}

// tokenHashExpiredLocked reports whether the given hash belongs to a
// token (the legacy one or a named one of u) whose ExpiresAt has passed,
// or to none of u's tokens (a revoke raced the lookup). Caller holds the
// lock.
func (s *Store) tokenHashExpiredLocked(u *User, hash string) bool {
	now := time.Now().UTC()
	if u.TokenHash == hash {
		// The legacy token never expires.
		return false
	}
	for _, t := range u.Tokens {
		if t.Hash == hash {
			return t.expired(now)
		}
	}
	return true
}

// AddFingerprint attaches another SSH key to a user.
func (s *Store) AddFingerprint(userID, fp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[userID]
	if u == nil {
		return fmt.Errorf("no such user")
	}
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return fmt.Errorf("fingerprint is required")
	}
	if owner, dup := s.byFP[fp]; dup && owner != userID {
		return fmt.Errorf("key %s already belongs to user %s", fp, owner)
	}
	for _, existing := range u.Fingerprints {
		if existing == fp {
			return nil // idempotent
		}
	}
	u.Fingerprints = append(u.Fingerprints, fp)
	s.byFP[fp] = userID
	return s.save()
}

// RemoveFingerprint detaches an SSH key from a user.
func (s *Store) RemoveFingerprint(userID, fp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[userID]
	if u == nil {
		return fmt.Errorf("no such user")
	}
	kept := u.Fingerprints[:0]
	for _, existing := range u.Fingerprints {
		if existing != fp {
			kept = append(kept, existing)
		}
	}
	u.Fingerprints = kept
	delete(s.byFP, fp)
	return s.save()
}

// RemoveUser deletes a user (idempotent). Does not delete leases.
func (s *Store) RemoveUser(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[userID]
	if u == nil {
		return nil
	}
	for _, fp := range u.Fingerprints {
		delete(s.byFP, fp)
	}
	if u.TokenHash != "" {
		delete(s.byToken, u.TokenHash)
	}
	for _, t := range u.Tokens {
		if t.Hash != "" {
			delete(s.byToken, t.Hash)
		}
		if t.ID != "" {
			delete(s.byTokenID, t.ID)
		}
	}
	delete(s.byName, u.Name)
	delete(s.users, userID)
	return s.save()
}

// Users returns a snapshot of all users sorted by name.
func (s *Store) Users() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		cp := *u
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetQuota updates a user's quota (T4/#31): the lease TTL cap in
// seconds, 0 = the global default cap applies. It is the only per-user
// quota left since 3.0 (fair shares, FS4).
func (s *Store) SetQuota(userID string, maxTTL int) error {
	if maxTTL < 0 {
		return fmt.Errorf("quota values must be >= 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[userID]
	if u == nil {
		return fmt.Errorf("no such user")
	}
	u.MaxTTL = maxTTL
	return s.save()
}

// SetLLMKey sets or rotates a user's LLM gateway key (U8/T8). Only the
// SHA256 hash is stored, never the key itself. An empty (or
// whitespace-only) key clears the user's key, reverting their leases to
// the legacy open-gateway behavior.
func (s *Store) SetLLMKey(userID, llmKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[userID]
	if u == nil {
		return fmt.Errorf("no such user")
	}
	llmKey = strings.TrimSpace(llmKey)
	if llmKey == "" {
		u.LLMKeyHash = ""
	} else {
		u.LLMKeyHash = s.hashSecret(llmKey)
	}
	return s.save()
}

// LLMKeyOK reports whether llmKey is the user's configured LLM gateway
// key, compared in constant time over the SHA256 hashes. Returns false
// when the user does not exist or has no key set.
func (s *Store) LLMKeyOK(userID, llmKey string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users[userID]
	if u == nil || u.LLMKeyHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(u.LLMKeyHash), []byte(s.hashSecret(llmKey))) == 1
}

// AddToken mints a new named token for a user (A2, unified auth #138).
// It returns the secret, which the caller must show exactly once — only
// its hash is stored, computed with the store's hashSecret like every
// other secret. A positive ttl gives the token an expiry (ExpiresAt;
// ttl <= 0 means it never expires). The name is unique per user; the
// legacy field's name is reserved while the legacy token (TokenHash)
// is still set, so its ListTokens entry cannot be shadowed.
func (s *Store) AddToken(userID, name string, ttl time.Duration) (string, TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[userID]
	if u == nil {
		return "", TokenRecord{}, fmt.Errorf("no such user")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", TokenRecord{}, fmt.Errorf("token name is required")
	}
	if len(name) > 64 {
		return "", TokenRecord{}, fmt.Errorf("token name too long (max 64)")
	}
	if name == defaultTokenName && u.TokenHash != "" {
		return "", TokenRecord{}, fmt.Errorf("token name %q is reserved for the user's existing token", defaultTokenName)
	}
	for _, t := range u.Tokens {
		if t.Name == name {
			return "", TokenRecord{}, fmt.Errorf("token name %q already exists", name)
		}
	}
	if len(u.Tokens) >= maxTokensPerUser {
		return "", TokenRecord{}, ErrTooManyTokens
	}
	var secretB [32]byte
	if _, err := rand.Read(secretB[:]); err != nil {
		return "", TokenRecord{}, fmt.Errorf("generate token: %w", err)
	}
	secret := "sdt-" + hex.EncodeToString(secretB[:])
	rec := TokenRecord{
		ID:        newTokenID(),
		Name:      name,
		Hash:      s.hashSecret(secret),
		CreatedAt: nowRFC3339(),
	}
	if ttl > 0 {
		rec.ExpiresAt = nowUTC().Add(ttl).Format(time.RFC3339)
	}
	u.Tokens = append(u.Tokens, rec)
	s.byToken[rec.Hash] = u.ID
	s.byTokenID[rec.ID] = u.ID
	if err := s.save(); err != nil {
		// Do not leave a half-registered token behind: index and record
		// are rolled back so a retry cannot fail with "already exists".
		u.Tokens = u.Tokens[:len(u.Tokens)-1]
		delete(s.byToken, rec.Hash)
		delete(s.byTokenID, rec.ID)
		return "", TokenRecord{}, err
	}
	return secret, rec, nil
}

// ListTokens returns a user's named tokens (A2), sorted by creation
// order (the store's), oldest first. The legacy TokenHash field is
// presented as the token named "default". Hash is never populated.
// An unknown user returns an error.
func (s *Store) ListTokens(userID string) ([]TokenRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users[userID]
	if u == nil {
		return nil, fmt.Errorf("no such user")
	}
	out := make([]TokenRecord, 0, len(u.Tokens)+1)
	if u.TokenHash != "" {
		out = append(out, TokenRecord{
			ID: legacyTokenID, Name: defaultTokenName, CreatedAt: u.CreatedAt,
			LastUsedAt: u.LastUsedAt,
		})
	}
	for _, t := range u.Tokens {
		t.Hash = "" // a listing never carries a hash
		out = append(out, t)
	}
	return out, nil
}

// RevokeToken removes one of a user's tokens by id (A2). Revoking the
// legacy "default" token clears the user's legacy TokenHash field
// (their original single token stops working immediately); revoking a
// named token drops its record. An unknown token id — never minted,
// already revoked, or another user's — is ErrTokenNotFound, so the API
// answers all three alike and no caller can probe another user's ids.
func (s *Store) RevokeToken(userID, tokenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[userID]
	if u == nil {
		return fmt.Errorf("no such user")
	}
	if tokenID == legacyTokenID {
		if u.TokenHash != "" {
			h, last := u.TokenHash, u.LastUsedAt
			s.deindexToken(u, h)
			u.TokenHash = ""
			u.LastUsedAt = ""
			if err := s.save(); err != nil {
				// Not on disk: put it back, so memory and file agree and
				// the caller can retry.
				u.TokenHash, u.LastUsedAt = h, last
				s.byToken[h] = u.ID
				return err
			}
			return nil
		}
		return ErrTokenNotFound
	}
	for i := range u.Tokens {
		if u.Tokens[i].ID != tokenID {
			continue
		}
		rec := u.Tokens[i]
		before := append([]TokenRecord(nil), u.Tokens...)
		u.Tokens = append(u.Tokens[:i], u.Tokens[i+1:]...)
		s.deindexToken(u, rec.Hash)
		delete(s.byTokenID, tokenID)
		if err := s.save(); err != nil {
			// Not on disk: put it back, so memory and file agree and the
			// caller can retry.
			u.Tokens = before
			s.byToken[rec.Hash] = u.ID
			s.byTokenID[tokenID] = u.ID
			return err
		}
		return nil
	}
	return ErrTokenNotFound
}

// Count returns the number of users (used for the bootstrap check).
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "u-" + hex.EncodeToString(b)
}

// newTokenID mints a named token's public identifier (A2). It is not a
// secret; the secret is returned by AddToken and shown once.
func newTokenID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "t-" + hex.EncodeToString(b)
}

func nowUTC() time.Time {
	return time.Now().UTC()
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
