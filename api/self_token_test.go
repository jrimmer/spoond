package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// selfTokenOf reads a lease's self token file from its guest.
func selfTokenOf(t *testing.T, s *secretsTestServer, sandboxID string) string {
	t.Helper()
	data, err := s.sub.ReadFile(t.Context(), sandboxID, "/run/secrets/"+selfTokenSecretName, 1<<20)
	if err != nil {
		t.Fatalf("read self token: %v", err)
	}
	return strings.TrimSpace(string(data))
}

// createSelfTokenLease creates a lease with a self token and returns its
// id, sandbox id and token.
func createSelfTokenLease(t *testing.T, s *secretsTestServer) (id, sandboxID, token string) {
	t.Helper()
	resp, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true, "self_token": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", resp.StatusCode, body)
	}
	if body["self_token"] != true {
		t.Fatalf("create did not report self_token: %v", body)
	}
	id = body["id"].(string)
	sandboxID = s.sandboxOf(id)
	return id, sandboxID, selfTokenOf(t, s, sandboxID)
}

func TestSelfTokenCreateWithout(t *testing.T) {
	s := newSecretsTestServer(t, true)
	resp, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", resp.StatusCode, body)
	}
	id := body["id"].(string)
	if _, ok := body["self_token"]; ok {
		t.Fatalf("self_token reported without the create flag: %v", body)
	}
	sb := s.sandboxOf(id)
	for _, name := range []string{selfTokenSecretName, selfTokenIDSecret, selfTokenURLSecret} {
		if _, err := s.sub.Stat(t.Context(), sb, "/run/secrets/"+name); err == nil {
			t.Fatalf("%s staged without self_token", name)
		}
	}
	// A made-up spl_ token is 401, like any unknown token.
	resp, _ = s.do(t, "DELETE", "/api/leases/"+id, "spl_unknown-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown spl_ token: status %d", resp.StatusCode)
	}
}

func TestSelfTokenCreateAndUse(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, sb, token := createSelfTokenLease(t, s)
	if !strings.HasPrefix(token, "spl_") {
		t.Fatalf("token %q lacks the spl_ prefix", token)
	}
	// The id file names the lease, and no URL file is written while
	// SELF_TOKEN_API_URL is empty.
	data, err := s.sub.ReadFile(t.Context(), sb, "/run/secrets/"+selfTokenIDSecret, 1<<20)
	if err != nil || strings.TrimSpace(string(data)) != id {
		t.Fatalf("lease-id file = %q, %v", data, err)
	}
	if _, err := s.sub.Stat(t.Context(), sb, "/run/secrets/"+selfTokenURLSecret); err == nil {
		t.Fatal("api-url file written without SELF_TOKEN_API_URL")
	}
	// The token is not the hash stored, and the hash is its sha256.
	sum := sha256.Sum256([]byte(token))
	if want := hex.EncodeToString(sum[:]); s.svc.leaseBySelfToken(token) == nil ||
		s.svc.leaseBySelfToken(token).SelfTokenHash != want {
		t.Fatal("stored hash is not sha256(token)")
	}

	// Keepalive on its own id succeeds.
	resp, body := s.do(t, "POST", "/api/leases/"+id+"/keepalive", token, map[string]any{"ttl": 300})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keepalive: status %d: %v", resp.StatusCode, body)
	}
	// DELETE on its own id succeeds and revokes the token at once.
	resp, _ = s.do(t, "DELETE", "/api/leases/"+id, token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	resp, _ = s.do(t, "POST", "/api/leases/"+id+"/keepalive", token, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token after delete: status %d, want 401", resp.StatusCode)
	}
}

func TestSelfTokenScope(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, _, token := createSelfTokenLease(t, s)
	resp, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second create: status %d: %v", resp.StatusCode, body)
	}
	other := body["id"].(string)

	// The same owner's other lease: 403 with the scope code.
	for _, c := range []struct{ method, path string }{
		{"DELETE", "/api/leases/" + other},
		{"POST", "/api/leases/" + other + "/keepalive"},
		// Everything else is out of scope even on its own lease.
		{"GET", "/api/leases/" + id},
		{"POST", "/api/leases/" + id + "/exec"},
		{"PUT", "/api/leases/" + id + "/files/x"},
		{"GET", "/api/leases"},
		{"GET", "/api/leases/events"},
		{"GET", "/api/leases/" + id + "/events"},
		{"POST", "/api/leases/" + id + "/clone"},
		{"GET", "/api/shares"},
	} {
		resp, body := s.do(t, c.method, c.path, token, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s: status %d: %v, want 403", c.method, c.path, resp.StatusCode, body)
		}
		if code, _ := body["code"].(string); code != "lease_token_scope" {
			t.Fatalf("%s %s: code %v, want lease_token_scope", c.method, c.path, body["code"])
		}
	}
}

// keepsToken asserts a lease's guest still holds its original token and
// that the token still works.
func keepsToken(t *testing.T, s *secretsTestServer, id, sb, old, what string) {
	t.Helper()
	if got := selfTokenOf(t, s, sb); got != old {
		t.Fatalf("%s changed the self token; a guest that read it once would hold a dead one", what)
	}
	if resp, _ := s.do(t, "POST", "/api/leases/"+id+"/keepalive", old, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("token after %s: status %d, want 200", what, resp.StatusCode)
	}
}

// A pause and resume keeps the token: the guest's memory, and any copy a
// process read, come back with it.
func TestSelfTokenKeptOnResume(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, _, old := createSelfTokenLease(t, s)
	resp, body := s.do(t, "POST", "/api/leases/"+id+"/suspend", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: status %d: %v", resp.StatusCode, body)
	}
	resp, body = s.do(t, "POST", "/api/leases/"+id+"/resume", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume: status %d: %v", resp.StatusCode, body)
	}
	keepsToken(t, s, id, s.sandboxOf(id), old, "resume")
}

func TestSelfTokenKeptOnRestart(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, _, old := createSelfTokenLease(t, s)
	resp, body := s.do(t, "POST", "/api/leases/"+id+"/restart", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart: status %d: %v", resp.StatusCode, body)
	}
	keepsToken(t, s, id, s.sandboxOf(id), old, "restart")
}

// After a backend restart this process no longer holds the token; a
// re-stage keeps the guest's copy when it matches the stored hash.
func TestSelfTokenKeptAfterBackendRestart(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, sb, old := createSelfTokenLease(t, s)
	s.svc.secretsMu.Lock()
	delete(s.svc.selfTokens, id)
	s.svc.secretsMu.Unlock()
	s.svc.restageCreateSecrets(t.Context(), s.svc.store.leases[id], "test")
	keepsToken(t, s, id, sb, old, "a re-stage after a backend restart")
}

// With neither this process's copy nor a matching guest copy, a re-stage
// mints a new token and the old one stops working.
func TestSelfTokenMintedWhenGuestCopyGone(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, sb, old := createSelfTokenLease(t, s)
	s.svc.secretsMu.Lock()
	delete(s.svc.selfTokens, id)
	s.svc.secretsMu.Unlock()
	if err := s.sub.Remove(t.Context(), sb, "/run/secrets/"+selfTokenSecretName, false); err != nil {
		t.Fatalf("remove guest copy: %v", err)
	}
	s.svc.restageCreateSecrets(t.Context(), s.svc.store.leases[id], "test")
	fresh := selfTokenOf(t, s, sb)
	if fresh == old || !strings.HasPrefix(fresh, selfTokenPrefix) {
		t.Fatalf("re-stage without any copy: token %q, want a new one", fresh)
	}
	if resp, _ := s.do(t, "POST", "/api/leases/"+id+"/keepalive", old, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token: status %d, want 401", resp.StatusCode)
	}
	if resp, _ := s.do(t, "POST", "/api/leases/"+id+"/keepalive", fresh, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("new token: status %d", resp.StatusCode)
	}
}

func TestSelfTokenForkChild(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, _, parentToken := createSelfTokenLease(t, s)
	resp, body := s.do(t, "POST", "/api/leases/"+id+"/fork", "token-a",
		map[string]any{"count": 1, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("fork: status %d: %v", resp.StatusCode, body)
	}
	ids, _ := body["ids"].([]any)
	if len(ids) != 1 {
		t.Fatalf("fork children = %v", body)
	}
	child := ids[0].(string)
	childToken := selfTokenOf(t, s, s.sandboxOf(child))
	if childToken == parentToken {
		t.Fatal("fork child shares the parent's self token")
	}
	// The parent's token on the child id is out of scope.
	resp, body = s.do(t, "DELETE", "/api/leases/"+child, parentToken, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("parent token on child: status %d: %v", resp.StatusCode, body)
	}
	if code, _ := body["code"].(string); code != "lease_token_scope" {
		t.Fatalf("code = %v", body["code"])
	}
	// The child's own token works on the child.
	if resp, body = s.do(t, "POST", "/api/leases/"+child+"/keepalive", childToken, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("child token on child: status %d: %v", resp.StatusCode, body)
	}
}

func TestSelfTokenCloneChild(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, _, parentToken := createSelfTokenLease(t, s)
	resp, body := s.do(t, "POST", "/api/leases/"+id+"/clone", "token-a", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("clone: status %d: %v", resp.StatusCode, body)
	}
	child := body["id"].(string)
	childToken := selfTokenOf(t, s, s.sandboxOf(child))
	if childToken == parentToken {
		t.Fatal("clone child shares the parent's self token")
	}
	if resp, body = s.do(t, "POST", "/api/leases/"+child+"/keepalive", parentToken, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("parent token on clone: status %d: %v", resp.StatusCode, body)
	}
}

func TestSelfTokenReservedSecretNames(t *testing.T) {
	s := newSecretsTestServer(t, true)
	for _, name := range []string{selfTokenSecretName, selfTokenIDSecret, selfTokenURLSecret} {
		resp, body := s.do(t, "POST", "/api/leases", "token-a",
			map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{name: "x"}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("create secret %s: status %d: %v", name, resp.StatusCode, body)
		}
	}
	resp, body := s.do(t, "POST", "/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", resp.StatusCode, body)
	}
	id := body["id"].(string)
	resp, body = s.do(t, "POST", "/api/leases/"+id+"/exec", "token-a",
		map[string]any{"cmd": "true", "secrets": map[string]string{selfTokenSecretName: "x"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("exec reserved secret: status %d: %v", resp.StatusCode, body)
	}
}

func TestSelfTokenReleaseReason(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, _, token := createSelfTokenLease(t, s)
	events := s.svc.Subscribe(EventFilter{})
	defer events.Close()
	if resp, _ := s.do(t, "DELETE", "/api/leases/"+id, token, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	got := ""
	for got == "" {
		select {
		case ev := <-events.C:
			if ev.LeaseID == id && ev.Type == LeaseReleased {
				got = ev.Detail
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no released event; want detail %q", selfTokenReleaseReason)
		}
	}
	if !strings.Contains(got, selfTokenReleaseReason) {
		t.Fatalf("released event detail = %q, want %q", got, selfTokenReleaseReason)
	}
}

// A named-snapshot save scrubs /run/secrets before it checkpoints and
// re-stages after: the source keeps its token, and the snapshot holds
// none.
func TestSelfTokenKeptOnSnapshotSave(t *testing.T) {
	s := newSecretsTestServer(t, true)
	id, sb, old := createSelfTokenLease(t, s)
	resp, body := s.do(t, "POST", "/api/leases/"+id+"/snapshots", "token-a",
		map[string]any{"name": "team/warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("save: status %d: %v", resp.StatusCode, body)
	}
	keepsToken(t, s, id, sb, old, "a snapshot save")
}

func TestSelfTokenAPIURLFile(t *testing.T) {
	s := newSecretsTestServer(t, true)
	s.svc.cfg.SelfTokenAPIURL = "https://api.example.test"
	id, sb, _ := createSelfTokenLease(t, s)
	_ = id
	data, err := s.sub.ReadFile(t.Context(), sb, "/run/secrets/"+selfTokenURLSecret, 1<<20)
	if err != nil || strings.TrimSpace(string(data)) != s.svc.cfg.SelfTokenAPIURL {
		t.Fatalf("api-url file = %q, %v", data, err)
	}
}
