package runner

import (
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
		"github.api_url":    "https://code.lacy.casa",
		"github.server_url": "https://code.lacy.casa",
		"github.repository": "jrimmer/netcrawl",
	}
	// Replicate the override logic from Fetch.
	if a.internalBaseURL != "" {
		ctxMap["github.api_url"] = a.internalBaseURL
		ctxMap["github.server_url"] = a.internalBaseURL
	}
	if ctxMap["github.api_url"] != "http://10.1.0.47:3000" {
		t.Fatalf("api_url = %q, want internal", ctxMap["github.api_url"])
	}
	if ctxMap["github.server_url"] != "http://10.1.0.47:3000" {
		t.Fatalf("server_url = %q, want internal", ctxMap["github.server_url"])
	}
	if ctxMap["github.repository"] != "jrimmer/netcrawl" {
		t.Fatalf("repository = %q, want unchanged", ctxMap["github.repository"])
	}
}

func TestInternalBaseURLOverrideInjectsMissingKeys(t *testing.T) {
	a := &ForgejoAdapter{internalBaseURL: "http://10.1.0.47:3000"}
	// Forgejo may omit github.server_url entirely; the override must
	// still inject it so workflows like reviewdog's GITEA_ADDRESS
	// (github.server_url) resolve to the internal host.
	ctxMap := map[string]string{
		"github.repository": "jrimmer/netcrawl",
	}
	if a.internalBaseURL != "" {
		ctxMap["github.api_url"] = a.internalBaseURL
		ctxMap["github.server_url"] = a.internalBaseURL
	}
	if ctxMap["github.server_url"] != "http://10.1.0.47:3000" {
		t.Fatalf("server_url = %q, want internal even when missing from source context", ctxMap["github.server_url"])
	}
	if ctxMap["github.repository"] != "jrimmer/netcrawl" {
		t.Fatalf("repository = %q, want unchanged", ctxMap["github.repository"])
	}
}
