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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

// User is a first-class identity: a person or an agent.
type User struct {
	ID           string   `json:"id"`           // stable user id (e.g. "u-<hex>")
	Name         string   `json:"name"`         // display/ctl name, unique
	Kind         Kind     `json:"kind"`         // person | agent
	Admin        bool     `json:"admin"`        // KTD-2: first user is admin
	Fingerprints []string `json:"fingerprints"` // SHA256 fingerprints of SSH keys
	TokenHash    string   `json:"token_hash"`   // SHA256 of the bearer token ("" = none)
	CreatedAt    string   `json:"created_at"`   // RFC3339
	// Quota (T4/#31). 0 = no per-user cap (global defaults apply).
	MaxLeases int `json:"max_leases"` // max concurrent leases (0 = unlimited)
	MaxTTL    int `json:"max_ttl"`    // max lease TTL seconds (0 = global max applies)
}

// Store is a thread-safe user registry with optional JSON persistence.
type Store struct {
	mu      sync.RWMutex
	users   map[string]*User  // by id
	byFP    map[string]string // fingerprint -> user id
	byToken map[string]string // token hash -> user id
	byName  map[string]string // name -> user id
	file    string
}

// NewStore creates an empty store. filename may be "" for in-memory-only.
func NewStore(filename string) (*Store, error) {
	s := &Store{
		users:   map[string]*User{},
		byFP:    map[string]string{},
		byToken: map[string]string{},
		byName:  map[string]string{},
		file:    filename,
	}
	if filename != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
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

// hashToken returns the SHA256 hex of a bearer token.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
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
		u.TokenHash = hashToken(token)
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

// UserByToken resolves a bearer token to a user by its hash.
func (s *Store) UserByToken(token string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byToken[hashToken(token)]
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

// SetQuota updates a user's lease quota (T4/#31). maxLeases 0 =
// unlimited; maxTTL 0 = global default cap applies.
func (s *Store) SetQuota(userID string, maxLeases, maxTTL int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[userID]
	if u == nil {
		return fmt.Errorf("no such user")
	}
	u.MaxLeases = maxLeases
	u.MaxTTL = maxTTL
	return s.save()
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

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
