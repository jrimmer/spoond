package hive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// catalogServer is an httptest server answering GET /api/images like the
// lease API does.
func newCatalogServer(t *testing.T, images ...string) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(map[string]any{"images": images})
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/images" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOfflineEnvListImages(t *testing.T) {
	var token string
	body, err := json.Marshal(map[string]any{"images": []string{"elixir-release", "go-base"}})
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	env := NewOfflineEnv(srv.URL, "token-a", testInstance())
	names, err := env.ListImages(context.Background())
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if !equalStrings(names, []string{"elixir-release", "go-base"}) {
		t.Errorf("images %v", names)
	}
	if token != "Bearer token-a" {
		t.Errorf("Authorization %q, want a bearer token", token)
	}
}

func TestOfflineEnvListImagesUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	env := NewOfflineEnv(srv.URL, "bad", testInstance())
	if _, err := env.ListImages(context.Background()); err == nil {
		t.Error("unauthorized lookup succeeded")
	}
}

func TestOfflineEnvFacts(t *testing.T) {
	srv := newCatalogServer(t)
	inst := testInstance()
	env := NewOfflineEnv(srv.URL, "token-a", inst)
	got, err := env.Facts(context.Background())
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if got.LeaseAPI != inst.LeaseAPI || got.Registry != inst.Registry ||
		got.ModelService != inst.ModelService || got.AgentMail != inst.AgentMail ||
		!equalStrings(got.Images, inst.Images) {
		t.Errorf("facts %+v, want %+v", got, inst)
	}
}

// TestOfflineEnvHostStepsSkip pins what the offline environment says for
// every host-only step: skip with the reason that names the check API.
func TestOfflineEnvHostStepsSkip(t *testing.T) {
	srv := newCatalogServer(t)
	env := NewOfflineEnv(srv.URL, "token-a", testInstance())
	ctx := context.Background()
	steps := map[string]error{
		"build": env.BuildImage(ctx, "go-base-worker"),
		"key":   env.PushScratch(ctx, "ssh://git@code.example.com/example/hrmny.git"),
		"lease": func() error {
			_, err := env.Reachable(ctx, []string{"code.example.com"}, []string{NeedLeases})
			return err
		}(),
		"gate":   env.RunGate(ctx, "ssh://git@code.example.com/example/hrmny.git", "mix test"),
		"budget": func() error { _, err := env.Budget(ctx, "hrmny"); return err }(),
	}
	for name, err := range steps {
		if err == nil {
			t.Errorf("%s: no error, want a skip", name)
			continue
		}
		if err.Error() != "runs on the host: POST /hive/check" {
			t.Errorf("%s: error %q, want the host-only skip", name, err)
		}
	}
}

// TestInstanceFromEnv covers how the instance facts are derived from the
// API base URL, which is what the CLI uses until the guide (step 3)
// serves the real ones.
func TestInstanceFromEnv(t *testing.T) {
	cases := []struct {
		name string
		api  string
		want Instance
	}{
		{
			name: "https with port",
			api:  "https://spoond.example.com:8890",
			want: Instance{
				LeaseAPI:     "spoond.example.com:8890",
				Registry:     "spoond.example.com:5000",
				ModelService: "llm.example.com",
				AgentMail:    "mail.example.com",
			},
		},
		{
			name: "http with default port",
			api:  "http://127.0.0.1:8890",
			want: Instance{
				LeaseAPI:     "127.0.0.1:8890",
				Registry:     "127.0.0.1:5000",
				ModelService: "llm.example.com",
				AgentMail:    "mail.example.com",
			},
		},
	}
	for _, tc := range cases {
		got, err := InstanceFromEnv(tc.api)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.LeaseAPI != tc.want.LeaseAPI || got.Registry != tc.want.Registry ||
			got.ModelService != tc.want.ModelService || got.AgentMail != tc.want.AgentMail {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if _, err := InstanceFromEnv("not a url"); err == nil {
		t.Error("host-less URL accepted")
	}
	if _, err := InstanceFromEnv("http://"); err == nil {
		t.Error("empty host accepted")
	}
}
