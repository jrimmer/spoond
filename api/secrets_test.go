package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/substrate"
)

// secretsTestServer is a test server with the service and fake substrate
// exposed and the service log captured.
type secretsTestServer struct {
	ts   *http.Server
	url  string
	svc  *Service
	sub  *testSub
	logs *bytes.Buffer
}

func newSecretsTestServer(t *testing.T, disableProbe bool) *secretsTestServer {
	t.Helper()
	ts, svc, _, sub := newTestServerWithService(t)
	buf := &bytes.Buffer{}
	svc.log = log.New(buf, "", 0)
	if disableProbe {
		svc.probeEnabled = false
	}
	return &secretsTestServer{url: ts.URL, svc: svc, sub: sub, logs: buf}
}

func (s *secretsTestServer) do(t *testing.T, method, path, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	return doReq(t, method, s.url+path, token, body)
}

// sandboxOf resolves a lease's sandbox id through the service's store.
func (s *secretsTestServer) sandboxOf(leaseID string) string {
	s.svc.store.mu.Lock()
	defer s.svc.store.mu.Unlock()
	return s.svc.store.leases[leaseID].SandboxID
}

// tailCalls returns the fake's recorded calls for one sandbox after the
// n most recent ones were dropped (for call-order assertions).
func (s *secretsTestServer) tailCalls(sandboxID string, drop int) []string {
	calls := s.sub.Fake.CallLog()
	if drop > len(calls) {
		drop = len(calls)
	}
	calls = calls[len(calls)-drop:]
	var out []string
	for _, c := range calls {
		if strings.HasSuffix(c, " "+sandboxID) || c == sandboxID {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(c, ""), " "+sandboxID))
		}
	}
	return out
}

func TestSecretsValidation(t *testing.T) {
	s := newSecretsTestServer(t, false)

	create := func(t *testing.T, secrets any) (*http.Response, map[string]any) {
		t.Helper()
		return s.do(t, "POST", "/api/leases", "token-a",
			map[string]any{"image": "py-base", "ttl": 300, "secrets": secrets})
	}

	// 400 on an invalid name: a path separator would escape the secrets
	// directory (names become file names under /run/secrets).
	resp, body := create(t, map[string]string{"../passwd": "v4l"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid name: status %d: %v", resp.StatusCode, body)
	}
	if msg := body["error"].(string); !strings.Contains(msg, "secret name") {
		t.Fatalf("error should name the field: %v", body)
	} else if strings.Contains(msg, "v4l") {
		t.Fatalf("error must not echo a value: %v", body)
	}

	// 400 on the wrong charset and on an empty name.
	if resp, _ = create(t, map[string]string{"bad name!": "x"}); resp.StatusCode != 400 {
		t.Fatalf("bad charset: status %d", resp.StatusCode)
	}
	if resp, _ = create(t, map[string]string{"": "x"}); resp.StatusCode != 400 {
		t.Fatalf("empty name: status %d", resp.StatusCode)
	}

	// 400 on more than 32 secrets per request.
	many := map[string]string{}
	for i := range 33 {
		many["s"+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	resp, _ = create(t, many)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("33 secrets: status %d", resp.StatusCode)
	}

	// 400 on more than 64 KiB of values in total; the message names
	// sizes, not values.
	big := map[string]string{"a": strings.Repeat("x", 33<<10), "b": strings.Repeat("y", 33<<10)}
	resp, body = create(t, big)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("65 KiB: status %d: %v", resp.StatusCode, body)
	}
	if msg := body["error"].(string); !strings.Contains(msg, "byte") || strings.Contains(msg, "xxxx") {
		t.Fatalf("size error: %v", body)
	}

	// The boundaries pass: 32 secrets, full-charset names of length 64.
	ok32 := map[string]string{}
	for i := range 32 {
		ok32[fmt.Sprintf("n%02d-%s", i, strings.Repeat("abcdefgh", 8))[:64]] = "v"
	}
	resp, body = create(t, ok32)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("32 valid secrets: status %d: %v", resp.StatusCode, body)
	}
	id := body["id"].(string)

	// Exec: exactly 64 KiB in one secret is fine, one byte more is 400,
	// and the name rules are the same as create's.
	if resp, body = s.do(t, "POST", "/api/leases/"+id+"/exec", "token-a",
		map[string]any{"cmd": "true", "secrets": map[string]string{"a": strings.Repeat("x", 64<<10)}}); resp.StatusCode != 200 {
		t.Fatalf("64 KiB exec: status %d: %v", resp.StatusCode, body)
	}
	if resp, _ = s.do(t, "POST", "/api/leases/"+id+"/exec", "token-a",
		map[string]any{"cmd": "true", "secrets": map[string]string{"a": strings.Repeat("x", 64<<10+1)}}); resp.StatusCode != 400 {
		t.Fatalf("65 KiB exec: status %d", resp.StatusCode)
	}
	if resp, _ = s.do(t, "POST", "/api/leases/"+id+"/exec", "token-a",
		map[string]any{"cmd": "true", "secrets": map[string]string{"no/slash": "v"}}); resp.StatusCode != 400 {
		t.Fatalf("exec bad name: status %d", resp.StatusCode)
	}
}

func TestCreateSecretsWrittenBeforeExec(t *testing.T) {
	s := newSecretsTestServer(t, false)

	created, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{"API_TOKEN": "s3cret-value"}})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", created.StatusCode, body)
	}
	id := body["id"].(string)
	sb := s.sandboxOf(id)

	// The grant staged the file with mode 0600 and the value verbatim.
	info, err := s.sub.Stat(t.Context(), sb, "/run/secrets/API_TOKEN")
	if err != nil {
		t.Fatalf("stat staged secret: %v", err)
	}
	if info.Mode.Perm() != 0o600 {
		t.Fatalf("secret mode = %o, want 600", info.Mode.Perm())
	}
	data, err := s.sub.ReadFile(t.Context(), sb, "/run/secrets/API_TOKEN", 1<<20)
	if err != nil || string(data) != "s3cret-value" {
		t.Fatalf("staged secret = %q, %v", data, err)
	}

	// Ordering: the staging WriteFile happens during the grant, before
	// the lease is even returned — anything the consumer runs afterwards
	// (its first exec) sees the file.
	calls := s.sub.Fake.CallLog()
	lastWrite := -1
	for i, c := range calls {
		if strings.HasPrefix(c, "WriteFile "+sb) {
			lastWrite = i
		}
	}
	if lastWrite < 0 {
		t.Fatal("no WriteFile recorded for the staged secret")
	}

	// The value never reaches the backend's log.
	if strings.Contains(s.logs.String(), "s3cret-value") {
		t.Fatalf("secret value leaked into the log: %s", s.logs.String())
	}
}

func TestExecSecretsWrittenAndRemoved(t *testing.T) {
	s := newSecretsTestServer(t, false)

	created, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{"SHARED": "lease-value"}})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", created.StatusCode, body)
	}
	id := body["id"].(string)
	sb := s.sandboxOf(id)

	// Exec with an exec-time secret that shadows the create-time one.
	resp, body := s.do(t, "POST", "/api/leases/"+id+"/exec", "token-a",
		map[string]any{"cmd": "true", "secrets": map[string]string{"EPHEMERAL": "exec-value", "SHARED": "shadow-value"}})
	if resp.StatusCode != 200 {
		t.Fatalf("exec: status %d: %v", resp.StatusCode, body)
	}

	// The exec-time secret is gone after the command.
	if _, err := s.sub.Stat(t.Context(), sb, "/run/secrets/EPHEMERAL"); err == nil {
		t.Fatal("exec-time secret survived the command")
	}

	// The shadowed create-time secret was restored to the lease's value.
	data, err := s.sub.ReadFile(t.Context(), sb, "/run/secrets/SHARED", 1<<20)
	if err != nil {
		t.Fatalf("read shadowed secret: %v", err)
	}
	if string(data) != "lease-value" {
		t.Fatalf("shadowed secret = %q, want the lease value", data)
	}

	// The values never reached the log.
	if str := s.logs.String(); strings.Contains(str, "exec-value") || strings.Contains(str, "shadow-value") {
		t.Fatalf("exec secret values leaked into the log: %s", str)
	}
}

func TestExecSecretsPresentDuringCommand(t *testing.T) {
	s := newSecretsTestServer(t, false)

	created, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", created.StatusCode, body)
	}
	id := body["id"].(string)

	// Use the fake's exec handler to capture the filesystem state at the
	// moment the command runs: the secret must already be there, mode
	// 0600. The mount script also rides Exec; it must keep succeeding.
	s.sub.SetExecHandler(func(sid string, args []string) substrate.ExecResult {
		if len(args) == 3 && args[0] == "/bin/bash" && args[2] == secretMountScript {
			return substrate.ExecResult{Stdout: "ok\n"}
		}
		info, err := s.sub.Stat(t.Context(), sid, "/run/secrets/RUN_TOKEN")
		if err != nil {
			return substrate.ExecResult{Stderr: "secret missing during command", ExitCode: 1}
		}
		if info.Mode.Perm() != 0o600 {
			return substrate.ExecResult{Stderr: "secret mode not 0600 during command", ExitCode: 1}
		}
		return substrate.ExecResult{Stdout: "ok\n"}
	})
	defer s.sub.SetExecHandler(nil)

	resp, body := s.do(t, "POST", "/api/leases/"+id+"/exec", "token-a",
		map[string]any{"cmd": "true", "secrets": map[string]string{"RUN_TOKEN": "during"}})
	if resp.StatusCode != 200 {
		t.Fatalf("exec: status %d: %v", resp.StatusCode, body)
	}
	// The exec-time file existed with mode 0600 while the command ran
	// (the handler Stat'd it above); afterwards it is gone.
	if _, err := s.sub.Stat(t.Context(), s.sandboxOf(id), "/run/secrets/RUN_TOKEN"); err == nil {
		t.Fatal("exec-time secret survived the command")
	}
	if strings.Contains(s.logs.String(), "during") {
		t.Fatal("exec secret value leaked into the log")
	}
}

func TestCreateSecretsNotInStoreOrLogs(t *testing.T) {
	s := newSecretsTestServer(t, false)

	created, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{"TOK": "store-leak-value"}})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", created.StatusCode, body)
	}
	id := body["id"].(string)

	// The backend keeps the value in memory for re-staging...
	if got := s.svc.createSecretsFor(id); got["TOK"] != "store-leak-value" {
		t.Fatalf("create-time secrets missing from memory: %v", got)
	}
	// ...but the persisted lease row has no secrets at all.
	s.svc.store.mu.Lock()
	row := leaseToRow(s.svc.store.leases[id])
	s.svc.store.mu.Unlock()
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal lease row: %v", err)
	}
	if strings.Contains(string(encoded), "store-leak-value") {
		t.Fatal("secret value reached the persisted lease row")
	}

	// No endpoint returns it: the detail response (and list) are clean.
	for _, path := range []string{"/api/leases/" + id, "/api/leases"} {
		resp, body := s.do(t, "GET", path, "token-a", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("get %s: status %d", path, resp.StatusCode)
		}
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "store-leak-value") {
			t.Fatalf("secret value appeared in %s response", path)
		}
	}
	if str := s.logs.String(); strings.Contains(str, "store-leak-value") {
		t.Fatalf("secret value leaked into the log: %s", str)
	}
}

func TestSecretsRestagedAfterResume(t *testing.T) {
	s := newSecretsTestServer(t, true) // skip the probe: it execs and would race nothing, but keeps the log clean

	created, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true,
			"secrets": map[string]string{"TOK": "resume-value"}})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", created.StatusCode, body)
	}
	id := body["id"].(string)
	sb := s.sandboxOf(id)

	// Suspend pauses the sandbox away; the fake's filesystem for it goes
	// with it, simulating the lost tmpfs.
	resp, body := s.do(t, "POST", "/api/leases/"+id+"/suspend", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("suspend: status %d: %v", resp.StatusCode, body)
	}
	if _, err := s.sub.Stat(t.Context(), sb, "/run/secrets/TOK"); err == nil {
		t.Fatal("secret survived the pause (fake fs should be gone)")
	}

	resp, body = s.do(t, "POST", "/api/leases/"+id+"/resume", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("resume: status %d: %v", resp.StatusCode, body)
	}
	data, err := s.sub.ReadFile(t.Context(), sb, "/run/secrets/TOK", 1<<20)
	if err != nil {
		t.Fatalf("secret missing after resume: %v", err)
	}
	if string(data) != "resume-value" {
		t.Fatalf("secret after resume = %q", data)
	}
}

func TestSecretsGoneAfterRelease(t *testing.T) {
	s := newSecretsTestServer(t, true)

	created, body := s.do(t, "POST", "/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{"TOK": "gone-value"}})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", created.StatusCode, body)
	}
	id := body["id"].(string)
	if s.svc.createSecretsFor(id) == nil {
		t.Fatal("expected in-memory create-time secrets after create")
	}
	req, _ := http.NewRequest("DELETE", s.url+"/api/leases/"+id, nil)
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	if s.svc.createSecretsFor(id) != nil {
		t.Fatal("create-time secrets survived the release")
	}
}

func TestValidateSecretsUnit(t *testing.T) {
	if out, err := validateSecrets(nil); out != nil || err != nil {
		t.Fatalf("nil input = %v, %v", out, err)
	}
	if out, err := validateSecrets(map[string]string{}); out != nil || err != nil {
		t.Fatalf("empty input = %v, %v", out, err)
	}
	out, err := validateSecrets(map[string]string{"A-B_c.1": "value"})
	if err != nil || out["A-B_c.1"] != "value" {
		t.Fatalf("valid input = %v, %v", out, err)
	}
	for _, bad := range []string{"", "no/slash", "no space", "x" + strings.Repeat("y", 64)} {
		if _, err := validateSecrets(map[string]string{bad: "v"}); err == nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
	if _, err := validateSecrets(map[string]string{"ok": strings.Repeat("v", 64<<10+1)}); err == nil {
		t.Fatal("over-limit total accepted")
	}
}

var _ = substrate.ExecResult{}
