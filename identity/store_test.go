package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirstUserIsAdmin(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.AddUser("jason", KindPerson, []string{"SHA256:aaa"}, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Admin {
		t.Fatal("first user must be admin (KTD-2)")
	}
	u2, err := s.AddUser("alice", KindPerson, []string{"SHA256:bbb"}, "tok2")
	if err != nil {
		t.Fatal(err)
	}
	if u2.Admin {
		t.Fatal("second user must not be admin")
	}
}

func TestDuplicateKeyRejected(t *testing.T) {
	s, _ := NewStore("")
	if _, err := s.AddUser("a", KindPerson, []string{"SHA256:same"}, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("b", KindPerson, []string{"SHA256:same"}, "t2"); err == nil {
		t.Fatal("expected duplicate key error")
	}
}

func TestDuplicateTokenRejected(t *testing.T) {
	s, _ := NewStore("")
	if _, err := s.AddUser("a", KindPerson, []string{"SHA256:fp1"}, "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("b", KindPerson, []string{"SHA256:fp2"}, "tok"); err == nil {
		t.Fatal("expected duplicate token error")
	}
}

func TestResolutions(t *testing.T) {
	s, _ := NewStore("")
	u, _ := s.AddUser("agent1", KindAgent, []string{"SHA256:fp-agent"}, "agtok")
	if got := s.UserByID(u.ID); got == nil || got.Name != "agent1" {
		t.Fatalf("UserByID: %v", got)
	}
	if got := s.UserByFingerprint("SHA256:fp-agent"); got == nil || got.Name != "agent1" {
		t.Fatalf("UserByFingerprint: %v", got)
	}
	if got := s.UserByToken("agtok"); got == nil || got.Kind != KindAgent {
		t.Fatalf("UserByToken: %v", got)
	}
	if got := s.UserByName("agent1"); got == nil || got.ID != u.ID {
		t.Fatalf("UserByName: %v", got)
	}
}

func TestAddRemoveFingerprint(t *testing.T) {
	s, _ := NewStore("")
	u, _ := s.AddUser("jason", KindPerson, []string{"SHA256:one"}, "tok")
	if err := s.AddFingerprint(u.ID, "SHA256:two"); err != nil {
		t.Fatal(err)
	}
	if got := s.UserByFingerprint("SHA256:two"); got == nil {
		t.Fatal("second fingerprint not resolvable")
	}
	if err := s.RemoveFingerprint(u.ID, "SHA256:one"); err != nil {
		t.Fatal(err)
	}
	if got := s.UserByFingerprint("SHA256:one"); got != nil {
		t.Fatal("removed fingerprint still resolvable")
	}
	if got := s.UserByFingerprint("SHA256:two"); got == nil {
		t.Fatal("other fingerprint lost")
	}
}

func TestRemoveUser(t *testing.T) {
	s, _ := NewStore("")
	u, _ := s.AddUser("jason", KindPerson, []string{"SHA256:fp"}, "tok")
	if err := s.RemoveUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 {
		t.Fatal("user not removed")
	}
	if s.UserByFingerprint("SHA256:fp") != nil || s.UserByToken("tok") != nil {
		t.Fatal("indexes not cleaned")
	}
	// idempotent
	if err := s.RemoveUser(u.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	s, _ := NewStore(file)
	u, _ := s.AddUser("jason", KindPerson, []string{"SHA256:fp"}, "tok")
	s2, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Count() != 1 {
		t.Fatalf("expected 1 user after reload, got %d", s2.Count())
	}
	if got := s2.UserByID(u.ID); got == nil || got.Name != "jason" || !got.Admin {
		t.Fatalf("reloaded user wrong: %v", got)
	}
	if got := s2.UserByToken("tok"); got == nil {
		t.Fatal("token not resolvable after reload")
	}
}

func TestSetLLMKey(t *testing.T) {
	s, _ := NewStore("")
	u, _ := s.AddUser("jason", KindPerson, []string{"SHA256:fp"}, "tok")
	if s.LLMKeyOK(u.ID, "anything") {
		t.Fatal("no key set yet")
	}
	if err := s.SetLLMKey(u.ID, "slk-secret"); err != nil {
		t.Fatal(err)
	}
	if !s.LLMKeyOK(u.ID, "slk-secret") {
		t.Fatal("correct key must verify")
	}
	if s.LLMKeyOK(u.ID, "slk-wrong") {
		t.Fatal("wrong key must not verify")
	}
	if s.LLMKeyOK("u-nope", "slk-secret") {
		t.Fatal("unknown user must not verify")
	}
	// Only the hash is stored, never the plaintext key.
	got := s.UserByID(u.ID)
	if got.LLMKeyHash == "" || got.LLMKeyHash == "slk-secret" || len(got.LLMKeyHash) != 64 {
		t.Fatalf("bad LLMKeyHash %q (want 64-hex SHA256, not plaintext)", got.LLMKeyHash)
	}
	// Clearing (empty key) revokes and reverts to open mode.
	if err := s.SetLLMKey(u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if s.LLMKeyOK(u.ID, "slk-secret") {
		t.Fatal("cleared key must not verify")
	}
	// Whitespace-only clears too; rotation replaces the old key.
	if err := s.SetLLMKey(u.ID, "   "); err != nil {
		t.Fatal(err)
	}
	if s.LLMKeyOK(u.ID, "slk-secret") {
		t.Fatal("whitespace clear must revoke")
	}
	if err := s.SetLLMKey(u.ID, "slk-rotated"); err != nil {
		t.Fatal(err)
	}
	if !s.LLMKeyOK(u.ID, "slk-rotated") || s.LLMKeyOK(u.ID, "slk-secret") {
		t.Fatal("rotation must replace the old key")
	}
	// Unknown user.
	if err := s.SetLLMKey("u-nope", "x"); err == nil {
		t.Fatal("expected error for unknown user")
	}
}

func TestSetLLMKeyPersistence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	s, _ := NewStore(file)
	u, _ := s.AddUser("jason", KindPerson, []string{"SHA256:fp"}, "tok")
	if err := s.SetLLMKey(u.ID, "slk-persist"); err != nil {
		t.Fatal(err)
	}
	// Reload from disk: the hash must round-trip and still verify.
	s2, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.LLMKeyOK(u.ID, "slk-persist") {
		t.Fatal("LLM key lost on reload")
	}
	if s2.LLMKeyOK(u.ID, "wrong") {
		t.Fatal("wrong key verifies after reload")
	}
}

func TestFingerprintFormat(t *testing.T) {
	fp := FingerprintSHA256([]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI test"))
	if len(fp) != len("SHA256:")+43 {
		t.Fatalf("unexpected fingerprint %q (len %d)", fp, len(fp))
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 {
		t.Fatal("missing file should be empty store")
	}
	// and the file is not created until first write
	if _, err := os.Stat(filepath.Join(dir, "nope.json")); !os.IsNotExist(err) {
		t.Fatal("empty store should not create a file")
	}
}

func TestSetQuota(t *testing.T) {
	s, _ := NewStore("")
	u, _ := s.AddUser("jason", KindPerson, []string{"SHA256:fp"}, "tok")
	if err := s.SetQuota(u.ID, 3, 120); err != nil {
		t.Fatal(err)
	}
	got := s.UserByID(u.ID)
	if got.MaxLeases != 3 || got.MaxTTL != 120 {
		t.Fatalf("quota = %d/%d, want 3/120", got.MaxLeases, got.MaxTTL)
	}
	// zero = unlimited
	if err := s.SetQuota(u.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	got = s.UserByID(u.ID)
	if got.MaxLeases != 0 || got.MaxTTL != 0 {
		t.Fatalf("reset quota = %d/%d, want 0/0", got.MaxLeases, got.MaxTTL)
	}
	// unknown user
	if err := s.SetQuota("u-nope", 1, 1); err == nil {
		t.Fatal("expected error for unknown user")
	}
	// persistence round-trip
	dir := t.TempDir()
	file := filepath.Join(dir, "users.json")
	s2, _ := NewStore(file)
	u2, _ := s2.AddUser("agent1", KindAgent, []string{"SHA256:fp2"}, "tok2")
	_ = s2.SetQuota(u2.ID, 5, 60)
	s3, _ := NewStore(file)
	got3 := s3.UserByID(u2.ID)
	if got3.MaxLeases != 5 || got3.MaxTTL != 60 {
		t.Fatalf("persisted quota = %d/%d, want 5/60", got3.MaxLeases, got3.MaxTTL)
	}
}
