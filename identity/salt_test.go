package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestM1SaltedHashes: fresh file-backed store gets a salt; stored
// hashes are HMAC-SHA256 (not plain SHA256 of the secret).
func TestM1SaltedHashes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")

	s, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.salt) == 0 {
		t.Fatal("fresh store should generate a salt")
	}
	u, err := s.AddUser("alice", KindPerson, nil, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	// The stored hash must NOT equal plain sha256 of the token.
	plain := sha256Hex("secret-token")
	if u.TokenHash == plain {
		t.Fatal("token hash must be salted, not plain SHA256")
	}
	// Verification round-trip (new store instance, same file).
	s2, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.UserByToken("secret-token"); got == nil || got.ID != u.ID {
		t.Fatal("salted token should verify after reload")
	}
	if s2.UserByToken("wrong") != nil {
		t.Fatal("wrong token should not verify")
	}
	// Salt persisted and stable across reloads.
	if string(s.salt) != string(s2.salt) {
		t.Fatal("salt must persist across reloads")
	}
}

// TestM1LegacyStoreCompat: a users file written without a salt sidecar
// (plain SHA256 hashes) keeps verifying after upgrade.
func TestM1LegacyStoreCompat(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")

	// Write a legacy users file directly (no salt sidecar).
	legacy := `[{"id":"u-1","name":"bob","kind":"person","admin":true,"fingerprints":null,"token_hash":"` +
		sha256Hex("bob-tok") + `","llm_key_hash":"","created_at":"2026-08-11T00:00:00Z"}]`
	if err := os.WriteFile(file, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.salt) != 0 {
		t.Fatal("legacy store (users present, no sidecar) must keep nil salt")
	}
	if got := s.UserByToken("bob-tok"); got == nil || got.Name != "bob" {
		t.Fatal("legacy plain-SHA256 token should still verify")
	}
}

// TestM1LLMKeySalted: LLM keys use the same salted scheme.
func TestM1LLMKeySalted(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	s, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.AddUser("carol", KindPerson, nil, "c-tok")
	if err := s.SetLLMKey(u.ID, "llm-key-1"); err != nil {
		t.Fatal(err)
	}
	if s.LLMKeyOK(u.ID, "llm-key-1") != true {
		t.Fatal("salted LLM key should verify")
	}
	if s.LLMKeyOK(u.ID, "wrong") != false {
		t.Fatal("wrong LLM key should not verify")
	}
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
