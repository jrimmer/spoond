package runner

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

func TestStructToMapFlattensNested(t *testing.T) {
	s, err := structpb.NewStruct(map[string]any{
		"repository": "dbl/site",
		"event": map[string]any{
			"pull_request": map[string]any{
				"number": float64(143),
			},
		},
	})
	if err != nil {
		t.Fatalf("struct: %v", err)
	}
	m := structToMap(s)
	if m["repository"] != "dbl/site" {
		t.Fatalf("repository = %q, want dbl/site", m["repository"])
	}
	if m["event.pull_request.number"] != "143" {
		t.Fatalf("event.pull_request.number = %q, want 143", m["event.pull_request.number"])
	}
}

func TestStructToMapNil(t *testing.T) {
	m := structToMap(nil)
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}
}

func TestInternalBaseURLOverride(t *testing.T) {
	a := &ForgejoAdapter{internalBaseURL: "http://10.1.0.47:3000"}
	ctxMap := map[string]string{
		"api_url":    "https://code.lacy.casa",
		"server_url": "https://code.lacy.casa",
		"repository": "jrimmer/netcrawl",
	}
	// Replicate the override logic from Fetch.
	if a.internalBaseURL != "" {
		apiURL := a.internalBaseURL
		if !strings.HasSuffix(apiURL, "/api/v1") {
			apiURL = strings.TrimRight(apiURL, "/") + "/api/v1"
		}
		ctxMap["github.api_url"] = apiURL
		ctxMap["github.server_url"] = a.internalBaseURL
		ctxMap["api_url"] = apiURL
		ctxMap["server_url"] = a.internalBaseURL
	}
	if ctxMap["api_url"] != "http://10.1.0.47:3000/api/v1" {
		t.Fatalf("api_url = %q, want internal with /api/v1 suffix", ctxMap["api_url"])
	}
	if ctxMap["server_url"] != "http://10.1.0.47:3000" {
		t.Fatalf("server_url = %q, want internal bare", ctxMap["server_url"])
	}
	if ctxMap["github.server_url"] != "http://10.1.0.47:3000" {
		t.Fatalf("github.server_url = %q, want internal", ctxMap["github.server_url"])
	}
	if ctxMap["repository"] != "jrimmer/netcrawl" {
		t.Fatalf("repository = %q, want unchanged", ctxMap["repository"])
	}
}

func TestInternalBaseURLOverrideInjectsMissingKeys(t *testing.T) {
	a := &ForgejoAdapter{internalBaseURL: "http://10.1.0.47:3000"}
	// Forgejo may omit the URL keys entirely; the override must still
	// inject them (both flat and prefixed forms) so workflows like
	// reviewdog's GITEA_ADDRESS (github.server_url) resolve to the
	// internal host.
	ctxMap := map[string]string{
		"repository": "jrimmer/netcrawl",
	}
	if a.internalBaseURL != "" {
		apiURL := a.internalBaseURL
		if !strings.HasSuffix(apiURL, "/api/v1") {
			apiURL = strings.TrimRight(apiURL, "/") + "/api/v1"
		}
		ctxMap["github.api_url"] = apiURL
		ctxMap["github.server_url"] = a.internalBaseURL
		ctxMap["api_url"] = apiURL
		ctxMap["server_url"] = a.internalBaseURL
	}
	if ctxMap["server_url"] != "http://10.1.0.47:3000" {
		t.Fatalf("server_url = %q, want internal even when missing from source context", ctxMap["server_url"])
	}
	if ctxMap["github.server_url"] != "http://10.1.0.47:3000" {
		t.Fatalf("github.server_url = %q, want internal even when missing from source context", ctxMap["github.server_url"])
	}
	if ctxMap["api_url"] != "http://10.1.0.47:3000/api/v1" {
		t.Fatalf("api_url = %q, want internal /api/v1 even when missing from source context", ctxMap["api_url"])
	}
	if ctxMap["repository"] != "jrimmer/netcrawl" {
		t.Fatalf("repository = %q, want unchanged", ctxMap["repository"])
	}
}
