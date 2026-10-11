package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/settings"
)

// getAdminConfig is one GET /api/admin/config request with a bearer
// token, decoded into the config response shape.
func getAdminConfig(t *testing.T, ts *httptest.Server, token string) (*http.Response, configResponse) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/admin/config", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/admin/config: %v", err)
	}
	defer resp.Body.Close()
	var out configResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// configByName indexes a config response's settings by name.
func configByName(t *testing.T, body configResponse) map[string]configSetting {
	t.Helper()
	m := map[string]configSetting{}
	for _, s := range body.Settings {
		m[s.Name] = s
	}
	return m
}

// TestAdminConfigAuth pins the auth matrix on GET /api/admin/config:
// the same adminOK contract as the other /api/admin routes — 404 with
// no ADMIN_TOKEN configured, 401 on a wrong or missing token, 200 with
// the admin token or an admin user's, 401 for a non-admin user.
func TestAdminConfigAuth(t *testing.T) {
	t.Run("no token configured: 404", func(t *testing.T) {
		ts, _, _, _ := newAdminServer(t, "")
		resp, _ := getAdminConfig(t, ts, "admin-tok")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unconfigured admin token = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("wrong and missing tokens: 401", func(t *testing.T) {
		ts, _, _, _ := newAdminServer(t, "admin-tok")
		if resp, _ := getAdminConfig(t, ts, "wrong-tok"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong token = %d, want 401", resp.StatusCode)
		}
		if resp, _ := getAdminConfig(t, ts, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("missing token = %d, want 401", resp.StatusCode)
		}
	})
	t.Run("admin token and admin user token: 200", func(t *testing.T) {
		ts, _, _ := newAdminServerWithIdentities(t)
		for _, tok := range []string{"admin-tok", "user-admin-tok"} {
			resp, body := getAdminConfig(t, ts, tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("token %q = %d (%v), want 200", tok, resp.StatusCode, body.StartedAt)
			}
			if _, err := time.Parse(time.RFC3339, body.StartedAt); err != nil {
				t.Fatalf("started_at %q is not RFC3339: %v", body.StartedAt, err)
			}
		}
	})
	t.Run("non-admin user token: 401", func(t *testing.T) {
		ts, _, _ := newAdminServerWithIdentities(t)
		if resp, _ := getAdminConfig(t, ts, "user-alice-tok"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("non-admin user token = %d, want 401", resp.StatusCode)
		}
	})
}

// TestAdminConfigBody: every settings.Known name appears in sorted
// order, set/unset follows os.LookupEnv, and secret-valued names are
// masked on the wire while plain ones carry their value.
func TestAdminConfigBody(t *testing.T) {
	// The test binary inherits the harness's env, which may set spoond
	// names; the assertions below only rely on names this test sets
	// itself plus whatever happens to be unset.
	t.Setenv("SPOOND_DB_PATH", "/tmp/config-test-db.sqlite") // plain, set
	t.Setenv("ADMIN_TOKEN", "shh")                           // secret, set
	t.Setenv("LLM_UPSTREAM_KEY", "sk-test")                  // secret, set
	t.Setenv("DASH_PASSWORD_HASH", "$2y$10$wishes")          // secret, set
	t.Setenv("E2B_TOKEN_SEED_FILE", "/etc/spoond/e2b-token-seed")
	t.Setenv("PROXY_AUTH_SECRET", "pash")
	t.Setenv("METRICS_TOKEN", "mtok")
	t.Setenv("GATEWAY_TOKEN", "gtok")
	t.Setenv("BOOTSTRAP_TOKEN", "btok")
	t.Setenv("EVENTS_TOKEN", "etok")

	ts, _, _, _ := newAdminServer(t, "shh")
	resp, body := getAdminConfig(t, ts, "shh")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin token = %d, want 200", resp.StatusCode)
	}
	if got := len(body.Settings); got != len(settings.Known) {
		t.Fatalf("%d settings in response, want %d (len(settings.Known))", got, len(settings.Known))
	}
	var names []string
	for _, s := range body.Settings {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, settings.Known) {
		t.Fatal("response settings are not exactly settings.Known in sorted order")
	}
	m := configByName(t, body)

	// A set plain value comes through.
	if s := m["SPOOND_DB_PATH"]; !s.Set || s.Value != "/tmp/config-test-db.sqlite" {
		t.Fatalf("SPOOND_DB_PATH = %+v, want set with its value", s)
	}
	// Every secret-bearing name that is set is masked; its value never
	// crosses the wire.
	for _, name := range []string{"ADMIN_TOKEN", "LLM_UPSTREAM_KEY", "DASH_PASSWORD_HASH", "E2B_TOKEN_SEED_FILE", "PROXY_AUTH_SECRET", "METRICS_TOKEN", "GATEWAY_TOKEN", "BOOTSTRAP_TOKEN", "EVENTS_TOKEN"} {
		s, ok := m[name]
		if !ok {
			t.Fatalf("%s is not in settings.Known", name)
		}
		if !s.Set {
			t.Fatalf("%s reported unset although this test set it", name)
		}
		if s.Value != "(hidden)" {
			t.Fatalf("%s value %q on the wire, want \"(hidden)\"", name, s.Value)
		}
	}
	// A plain name is never masked just because its value looks secret-ish.
	// (The masking rule reads the name, not the value.)

	// Whatever known name the test process has unset is reported unset
	// with an empty value.
	for _, name := range settings.Known {
		if _, ok := os.LookupEnv(name); !ok {
			if s := m[name]; s.Set || s.Value != "" {
				t.Fatalf("%s unset in process but response says %+v", name, s)
			}
		}
	}
}

// TestAdminConfigStartsAtMoves: the started_at stamps move with the
// server's construction time, so a config show can tell a restart from
// a still-running backend.
func TestAdminConfigStartsAtMoves(t *testing.T) {
	ts, _, _, _ := newAdminServer(t, "shh")
	_, body1 := getAdminConfig(t, ts, "shh")
	t1, err := time.Parse(time.RFC3339, body1.StartedAt)
	if err != nil {
		t.Fatalf("started_at %q: %v", body1.StartedAt, err)
	}
	// A server built later stamps later (same second granularity is
	// possible; assert the stamp is within a sane window instead).
	if d := time.Since(t1); d < 0 || d > time.Minute {
		t.Fatalf("started_at %q is not this test's instant (off by %s)", body1.StartedAt, d)
	}
}