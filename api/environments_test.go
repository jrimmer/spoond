package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newEnvTestServer builds a lease API server with the webhook receiver
// configured (secret, env owner, env image) for environment tests.
func newEnvTestServer(t *testing.T, secret, owner, image string) (*httptest.Server, *fakeForkd) {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a", "token-b": "consumer-b"}, 0, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServer(svc, reg)
	srv.SetWebhookSecret(secret)
	srv.SetEnvDefaults(owner, image)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, ff
}

// prPayload builds a Forgejo pull_request webhook payload.
func prPayload(action, repo string, number int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"number": number,
		"repository": map[string]any{
			"full_name": repo,
			"html_url":  "https://git.example/" + repo,
		},
		"pull_request": map[string]any{
			"number":   number,
			"state":    "open",
			"merged":   false,
			"title":    "test",
			"html_url": "https://git.example/" + repo + "/pulls/" + string(rune('0'+number)),
			"head":     map[string]any{"ref": "feature", "sha": "abc"},
			"base":     map[string]any{"ref": "main", "sha": "def"},
		},
	})
	return b
}

func signPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func webhookReq(t *testing.T, ts *httptest.Server, event string, body []byte, sig string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/hooks/forgejo", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if event != "" {
		req.Header.Set("X-Forgejo-Event", event)
	}
	if sig != "" {
		req.Header.Set("X-Forgejo-Signature", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	return resp
}

func TestEnvNameSanitizes(t *testing.T) {
	names := []string{
		envName("lacy/repo", "12"),
		envName("some-owner/a_very_long_repository_name_that_must_be_truncated", "123456"),
		envName("UPPER/Case.Repo", "feature/branch"),
		envName("", ""),
	}
	seen := map[string]bool{}
	for _, n := range names {
		if len(n) == 0 || len(n) > 63 {
			t.Fatalf("envName %q: bad length %d", n, len(n))
		}
		if !strings.HasPrefix(n, "env") {
			t.Fatalf("envName %q: expected env- prefix", n)
		}
		for i, r := range n {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0)
			if !ok {
				t.Fatalf("envName %q: invalid char %q at %d", n, r, i)
			}
		}
		if seen[n] {
			t.Fatalf("envName collision on %q", n)
		}
		seen[n] = true
	}
}

func TestEnvCreateAndReuse(t *testing.T) {
	ts, _ := newEnvTestServer(t, "", "consumer-a", "py-base")
	resp, body := doReq(t, "POST", ts.URL+"/api/environments", "token-a", map[string]any{"repo": "lacy/repo", "ref": "12"})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %v", resp.StatusCode, body)
	}
	sid, _ := body["sandbox_id"].(string)
	if sid == "" {
		t.Fatalf("expected non-empty sandbox_id, got %v", body)
	}
	if body["key"] != "lacy/repo#12" {
		t.Fatalf("expected key lacy/repo#12, got %v", body["key"])
	}

	// Same repo+ref returns the existing environment (200, same sandbox).
	resp2, body2 := doReq(t, "POST", ts.URL+"/api/environments", "token-a", map[string]any{"repo": "lacy/repo", "ref": "12"})
	if resp2.StatusCode != 200 {
		t.Fatalf("reuse status %d: %v", resp2.StatusCode, body2)
	}
	if body2["sandbox_id"] != sid {
		t.Fatalf("reuse returned different sandbox: %v vs %v", body2["sandbox_id"], sid)
	}
}

func TestEnvOwnerScoping(t *testing.T) {
	ts, _ := newEnvTestServer(t, "", "consumer-a", "py-base")
	if resp, body := doReq(t, "POST", ts.URL+"/api/environments", "token-a", map[string]any{"repo": "lacy/repo", "ref": "1"}); resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	// consumer-b has no environments.
	_, list := doReq(t, "GET", ts.URL+"/api/environments", "token-b", nil)
	if envs, ok := list["environments"].([]any); !ok || len(envs) != 0 {
		t.Fatalf("consumer-b should see no environments, got %v", list)
	}
	// consumer-a sees exactly one.
	_, listA := doReq(t, "GET", ts.URL+"/api/environments", "token-a", nil)
	if envs, ok := listA["environments"].([]any); !ok || len(envs) != 1 {
		t.Fatalf("consumer-a should see one environment, got %v", listA)
	}
}

func TestEnvTeardown(t *testing.T) {
	ts, _ := newEnvTestServer(t, "", "consumer-a", "py-base")
	if resp, body := doReq(t, "POST", ts.URL+"/api/environments", "token-a", map[string]any{"repo": "lacy/repo", "ref": "12"}); resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	resp, _ := doReq(t, "DELETE", ts.URL+"/api/environments?repo=lacy/repo&ref=12", "token-a", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("teardown status %d", resp.StatusCode)
	}
	// Teardown is idempotent: a second delete is 404 (already gone).
	resp2, _ := doReq(t, "DELETE", ts.URL+"/api/environments?repo=lacy/repo&ref=12", "token-a", nil)
	if resp2.StatusCode != 404 {
		t.Fatalf("second teardown status %d (want 404)", resp2.StatusCode)
	}
	_, list := doReq(t, "GET", ts.URL+"/api/environments", "token-a", nil)
	if envs, ok := list["environments"].([]any); !ok || len(envs) != 0 {
		t.Fatalf("expected no environments after teardown, got %v", list)
	}
}

func TestEnvRequiresRepoAndRef(t *testing.T) {
	ts, _ := newEnvTestServer(t, "", "consumer-a", "py-base")
	resp, _ := doReq(t, "POST", ts.URL+"/api/environments", "token-a", map[string]any{"repo": "lacy/repo"})
	if resp.StatusCode != 400 {
		t.Fatalf("missing ref should 400, got %d", resp.StatusCode)
	}
}

func TestWebhookDisabled(t *testing.T) {
	ts, _ := newEnvTestServer(t, "", "consumer-a", "py-base") // empty secret
	body := prPayload("opened", "lacy/repo", 1)
	resp := webhookReq(t, ts, "pull_request", body, "")
	if resp.StatusCode != 404 {
		t.Fatalf("disabled webhook should 404, got %d", resp.StatusCode)
	}
}

func TestWebhookInvalidSignature(t *testing.T) {
	ts, _ := newEnvTestServer(t, "s3cret", "consumer-a", "py-base")
	body := prPayload("opened", "lacy/repo", 1)
	resp := webhookReq(t, ts, "pull_request", body, "deadbeef")
	if resp.StatusCode != 401 {
		t.Fatalf("bad signature should 401, got %d", resp.StatusCode)
	}
	// Missing signature also 401.
	resp2 := webhookReq(t, ts, "pull_request", body, "")
	if resp2.StatusCode != 401 {
		t.Fatalf("missing signature should 401, got %d", resp2.StatusCode)
	}
}

func TestWebhookPullRequestLifecycle(t *testing.T) {
	ts, _ := newEnvTestServer(t, "s3cret", "consumer-a", "py-base")

	// opened → 201, environment created.
	b1 := prPayload("opened", "lacy/repo", 42)
	resp := webhookReq(t, ts, "pull_request", b1, signPayload("s3cret", b1))
	if resp.StatusCode != 201 {
		t.Fatalf("opened should 201, got %d", resp.StatusCode)
	}

	// synchronize → 200, existing environment (idempotent).
	b2 := prPayload("synchronize", "lacy/repo", 42)
	resp2 := webhookReq(t, ts, "pull_request", b2, signPayload("s3cret", b2))
	if resp2.StatusCode != 200 {
		t.Fatalf("synchronize should 200, got %d", resp2.StatusCode)
	}

	// closed → 200, torn down.
	b3 := prPayload("closed", "lacy/repo", 42)
	resp3 := webhookReq(t, ts, "pull_request", b3, signPayload("s3cret", b3))
	if resp3.StatusCode != 200 {
		t.Fatalf("closed should 200, got %d", resp3.StatusCode)
	}

	// The environment is gone (owner scoped list is empty).
	_, list := doReq(t, "GET", ts.URL+"/api/environments", "token-a", nil)
	if envs, ok := list["environments"].([]any); !ok || len(envs) != 0 {
		t.Fatalf("expected no environments after close, got %v", list)
	}
}

func TestWebhookPingAndUnrelatedEvents(t *testing.T) {
	ts, _ := newEnvTestServer(t, "s3cret", "consumer-a", "py-base")
	ping := []byte(`{"hook":{}}`)
	resp := webhookReq(t, ts, "ping", ping, signPayload("s3cret", ping))
	if resp.StatusCode != 200 {
		t.Fatalf("ping should 200, got %d", resp.StatusCode)
	}
	// A push event is acknowledged but ignored (no environment created).
	push := []byte(`{"ref":"refs/heads/main","repository":{"full_name":"lacy/repo"}}`)
	resp2 := webhookReq(t, ts, "push", push, signPayload("s3cret", push))
	if resp2.StatusCode != 200 {
		t.Fatalf("push should 200, got %d", resp2.StatusCode)
	}
	_, list := doReq(t, "GET", ts.URL+"/api/environments", "token-a", nil)
	if envs, ok := list["environments"].([]any); !ok || len(envs) != 0 {
		t.Fatalf("push should not create an environment, got %v", list)
	}
}
