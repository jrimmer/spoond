package identity

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAddTokenMintsAndResolves covers the AddToken/UserByToken round
// trip: the secret is shown once, only the hash is stored, and the
// minted token authenticates as its user.
func TestAddTokenMintsAndResolves(t *testing.T) {
	s, _ := NewStore("")
	u, err := s.AddUser("jason", KindPerson, nil, "legacy-tok")
	if err != nil {
		t.Fatal(err)
	}
	secret, rec, err := s.AddToken(u.ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" || rec.ID == "" || rec.Name != "ci" {
		t.Fatalf("bad mint: secret=%q rec=%+v", secret, rec)
	}
	if rec.Hash != s.hashSecret(secret) {
		t.Fatal("record hash must be the hash of the returned secret")
	}
	if rec.ExpiresAt != "" {
		t.Fatalf("no-ttl token must never expire, got %q", rec.ExpiresAt)
	}
	// The secret never appears in the persisted file.
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	s2, _ := NewStore(file)
	u2, _ := s2.AddUser("jason", KindPerson, nil, "legacy-tok")
	secret2, _, err := s2.AddToken(u2.ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), secret2) {
		t.Fatal("token secret persisted in the users file")
	}
	// The secret authenticates.
	if got := s2.UserByToken(secret2); got == nil || got.ID != u2.ID {
		t.Fatal("minted token must resolve to its user")
	}
	if got := s2.UserByToken("legacy-tok"); got == nil || got.ID != u2.ID {
		t.Fatal("legacy token must still work beside named tokens")
	}
	_ = s
}

// TestListTokensListsDefault covers the default presentation: a user
// with only the legacy TokenHash lists exactly one token, named
// "default", without a hash.
func TestListTokensListsDefault(t *testing.T) {
	s, _ := NewStore("")
	u, _ := s.AddUser("jason", KindPerson, nil, "legacy-tok")
	toks, err := s.ListTokens(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 1 {
		t.Fatalf("want 1 token (default), got %+v", toks)
	}
	d := toks[0]
	if d.ID != legacyTokenID || d.Name != defaultTokenName {
		t.Fatalf("legacy token presented as %q/%q", d.ID, d.Name)
	}
	if d.Hash != "" {
		t.Fatal("listed tokens must not carry a hash")
	}
	if d.LastUsedAt != "" {
		t.Fatalf("default token never used yet, last_used %q", d.LastUsedAt)
	}
	// Add one named token; both are listed, default first (creation
	// order: the legacy token is the oldest).
	if _, _, err := s.AddToken(u.ID, "ci", 0); err != nil {
		t.Fatal(err)
	}
	toks, _ = s.ListTokens(u.ID)
	if len(toks) != 2 || toks[0].Name != defaultTokenName || toks[1].Name != "ci" {
		t.Fatalf("list = %+v, want [default, ci]", toks)
	}
	// Unknown user errors.
	if _, err := s.ListTokens("u-nope"); err == nil {
		t.Fatal("expected error for unknown user")
	}
}

// TestAddTokenDuplicateNameAndDefaultReserved covers the naming rules:
// names are unique per user and "default" is reserved while the legacy
// token is still set.
func TestAddTokenDuplicateNameAndDefaultReserved(t *testing.T) {
	s, _ := NewStore("")
	u, _ := s.AddUser("jason", KindPerson, nil, "legacy-tok")
	if _, _, err := s.AddToken(u.ID, "default", 0); err == nil {
		t.Fatal("default must be reserved while the legacy token is set")
	}
	if _, _, err := s.AddToken(u.ID, "ci", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddToken(u.ID, "ci", 0); err == nil {
		t.Fatal("duplicate name must be refused")
	}
	if _, _, err := s.AddToken(u.ID, "  ", 0); err == nil {
		t.Fatal("blank name must be refused")
	}
	long := strings.Repeat("x", 65)
	if _, _, err := s.AddToken(u.ID, long, 0); err == nil {
		t.Fatal("an over-long name must be refused")
	}
	if _, _, err := s.AddToken("u-nope", "ci", 0); err == nil {
		t.Fatal("unknown user must be refused")
	}
	// After the default is revoked the name is free again.
	if err := s.RevokeToken(u.ID, legacyTokenID); err != nil {
		t.Fatal(err)
	}
	secret, rec, err := s.AddToken(u.ID, "default", 0)
	if err != nil {
		t.Fatalf("default name must be mintable once the legacy token is revoked: %v", err)
	}
	if got := s.UserByToken(secret); got == nil || got.ID != u.ID {
		t.Fatal("new default token must resolve")
	}
	if rec.ID == legacyTokenID {
		t.Fatal("a minted token must not reuse the legacy id")
	}
}

// TestTokenExpiry covers that an expired token authenticates nobody and
// is refused on every later lookup.
func TestTokenExpiry(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	s, _ := NewStore(file)
	u, _ := s.AddUser("jason", KindPerson, nil, "legacy-tok")
	secret, rec, err := s.AddToken(u.ID, "short", 1*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExpiresAt == "" {
		t.Fatal("a ttl'd token must carry an expiry")
	}
	exp, err := time.Parse(time.RFC3339, rec.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Before(time.Now().UTC()) {
		t.Fatalf("expiry %v is already past", exp)
	}
	// Before the expiry it works.
	if got := s.UserByToken(secret); got == nil {
		t.Fatal("unexpired token must resolve")
	}
	// Write the file, reload, and let the expiry pass.
	s2, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if got := s2.UserByToken(secret); got != nil {
		t.Fatalf("expired token still resolves to %+v", got)
	}
	// The direct store too.
	if got := s.UserByToken(secret); got != nil {
		t.Fatal("expired token must be refused by the original store")
	}
	// The legacy token is unaffected.
	if got := s.UserByToken("legacy-tok"); got == nil {
		t.Fatal("legacy token must still resolve")
	}
}

// TestRevokeToken covers revocation of named tokens and of the default
// (legacy) token, including that a revoked secret stops authenticating.
func TestRevokeToken(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	s, _ := NewStore(file)
	u, _ := s.AddUser("jason", KindPerson, nil, "legacy-tok")
	secret, rec, err := s.AddToken(u.ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(u.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.UserByToken(secret); got != nil {
		t.Fatal("revoked token still authenticates")
	}
	toks, _ := s.ListTokens(u.ID)
	if len(toks) != 1 || toks[0].Name != defaultTokenName {
		t.Fatalf("after revoke the list = %+v, want only default", toks)
	}
	// A repeat revoke, an unknown id and another user's id all answer
	// ErrTokenNotFound alike, so none can be told apart.
	if err := s.RevokeToken(u.ID, rec.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("repeat revoke = %v, want ErrTokenNotFound", err)
	}
	if err := s.RevokeToken(u.ID, "t-nope"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("unknown token id = %v, want ErrTokenNotFound", err)
	}
	if err := s.RevokeToken("u-nope", "t-nope"); err == nil {
		t.Fatal("unknown user must error")
	}
	// An id that is another user's token must not be revocable here.
	uB, _ := s.AddUser("bob", KindPerson, nil, "b-tok")
	_, recB, err := s.AddToken(uB.ID, "bob-tokname", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(u.ID, recB.ID); err == nil {
		t.Fatal("revoking another user's token must fail")
	}
	if got := s.UserByToken("b-tok"); got == nil {
		t.Fatal("bob's untouched state must still verify")
	}
	// Revoking default clears the legacy field; the legacy token stops
	// working and the field stays empty after a reload.
	if err := s.RevokeToken(u.ID, legacyTokenID); err != nil {
		t.Fatal(err)
	}
	if got := s.UserByToken("legacy-tok"); got != nil {
		t.Fatal("legacy token must stop working after its revoke")
	}
	s2, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.UserByToken("legacy-tok"); got != nil {
		t.Fatal("legacy token still verifies after reload of a revoked default")
	}
	u2 := s2.UserByID(u.ID)
	if u2.TokenHash != "" {
		t.Fatal("revoked default must clear the legacy TokenHash field")
	}
	toks, _ = s2.ListTokens(u.ID)
	if len(toks) != 0 {
		t.Fatalf("after all revokes the list = %+v, want empty", toks)
	}
}

// TestTokenLastUsedAt covers that an authentication stamps the token's
// last_used_at (persisted) and never moves it backwards.
func TestTokenLastUsedAt(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	s, _ := NewStore(file)
	u, _ := s.AddUser("jason", KindPerson, nil, "legacy-tok")
	secret, _, err := s.AddToken(u.ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListTokens(u.ID)
	if before[1].LastUsedAt != "" {
		t.Fatalf("fresh token carries a last-used stamp %q", before[1].LastUsedAt)
	}
	if s.UserByToken(secret) == nil {
		t.Fatal("token must resolve")
	}
	stamped, _ := s.ListTokens(u.ID)
	if stamped[1].LastUsedAt == "" {
		t.Fatal("use must stamp last_used_at")
	}
	// Persisted.
	s2, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s2.ListTokens(u.ID)
	if again[1].LastUsedAt != stamped[1].LastUsedAt {
		t.Fatalf("last_used not persisted: %q vs %q", again[1].LastUsedAt, stamped[1].LastUsedAt)
	}
}

// TestStoreRoundTripOldFormatAndTokens: a users file written before
// named tokens (no "tokens" field, plain unsalted token hashes, no salt
// sidecar) loads unchanged; the legacy token becomes the "default"
// token on the list; newly minted named tokens persist alongside.
func TestStoreRoundTripOldFormatAndTokens(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	legacyHash := sha256Hex("old-plain-tok")
	old := `[{"id":"u-old","name":"old","kind":"person","admin":true,"fingerprints":["SHA256:fp-old"],` +
		`"token_hash":"` + legacyHash + `","llm_key_hash":"","created_at":"2026-01-01T00:00:00Z",` +
		`"max_leases":3,"max_ttl":600,"max_kept_bytes":1073741824,"guaranteed_mib":1024,"max_mib":4096}]`
	if err := os.WriteFile(file, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(file)
	if err != nil {
		t.Fatalf("load old users file: %v", err)
	}
	u := s.UserByID("u-old")
	if u == nil || !u.Admin || u.MaxTTL != 600 {
		t.Fatalf("loaded user = %+v", u)
	}
	// The plain hash still verifies (legacy store, no salt).
	if got := s.UserByToken("old-plain-tok"); got == nil || got.ID != "u-old" {
		t.Fatal("old plain token must verify after upgrade")
	}
	// The legacy token is presented as "default".
	toks, _ := s.ListTokens(u.ID)
	if len(toks) != 1 || toks[0].Name != defaultTokenName || toks[0].ID != legacyTokenID {
		t.Fatalf("legacy default presentation = %+v", toks)
	}
	// A new named token mints and persists beside it.
	secret, rec, err := s.AddToken(u.ID, "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The file carries both hashes: the untouched legacy one and the new
	// token record.
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), secret) {
		t.Fatal("secret persisted")
	}
	var users []User
	if err := json.Unmarshal(data, &users); err != nil {
		t.Fatal(err)
	}
	if users[0].TokenHash != legacyHash {
		t.Fatalf("legacy token_hash was rewritten: %q", users[0].TokenHash)
	}
	if len(users[0].Tokens) != 1 || users[0].Tokens[0].ID != rec.ID {
		t.Fatalf("tokens field = %+v", users[0].Tokens)
	}
	// Reload: both still work.
	s2, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.UserByToken("old-plain-tok"); got == nil {
		t.Fatal("legacy token must survive the reload")
	}
	if got := s2.UserByToken(secret); got == nil || got.ID != "u-old" {
		t.Fatal("minted token must survive the reload")
	}
	toks, _ = s2.ListTokens(u.ID)
	if len(toks) != 2 {
		t.Fatalf("list after reload = %+v", toks)
	}
}

// TestTokenHashHexLength: minted secrets are "sdt-" + 64 hex chars.
func TestTokenHashHexLength(t *testing.T) {
	s, _ := NewStore("")
	u, _ := s.AddUser("jason", KindPerson, nil, "legacy-tok")
	secret, _, err := s.AddToken(u.ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimPrefix(secret, "sdt-")
	if len(raw) != 64 {
		t.Fatalf("secret suffix len = %d, want 64 hex", len(raw))
	}
	if _, err := hex.DecodeString(raw); err != nil {
		t.Fatalf("secret suffix is not hex: %v", err)
	}
}

// TestLastUsedThrottled: an authentication refreshes a token's
// last-used stamp only once it is older than lastUsedResolution, so a
// busy token does not rewrite the users file on every request.
func TestLastUsedThrottled(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.AddUser("alice", KindPerson, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	secret, rec, err := s.AddToken(u.ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	stamp := func(at string) {
		s.mu.Lock()
		for i := range s.users[u.ID].Tokens {
			if s.users[u.ID].Tokens[i].ID == rec.ID {
				s.users[u.ID].Tokens[i].LastUsedAt = at
			}
		}
		s.mu.Unlock()
	}
	read := func() string {
		toks, _ := s.ListTokens(u.ID)
		for _, tk := range toks {
			if tk.ID == rec.ID {
				return tk.LastUsedAt
			}
		}
		return ""
	}
	recent := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	stamp(recent)
	if s.UserByToken(secret) == nil {
		t.Fatal("token did not authenticate")
	}
	if got := read(); got != recent {
		t.Fatalf("a 1 min old stamp was rewritten: %s -> %s", recent, got)
	}
	old := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	stamp(old)
	s.UserByToken(secret)
	if got := read(); got <= old {
		t.Fatalf("a 10 min old stamp was not refreshed: %s", got)
	}
}
