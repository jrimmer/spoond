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
