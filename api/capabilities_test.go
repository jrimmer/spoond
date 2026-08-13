package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jrimmer/spoond/capabilities"
)

// TestCapabilitiesEndpoint verifies GET /v1/capabilities is auth-exempt
// (issue #52: an agent host must be able to discover the surface cold)
// and returns a parseable self-description document with the key fields.
func TestCapabilitiesEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)

	req, _ := http.NewRequest("GET", ts.URL+"/v1/capabilities", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var doc capabilities.Document
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Name != "spoond" {
		t.Errorf("name = %q, want spoond", doc.Name)
	}
	if doc.Auth != "bearer-token" {
		t.Errorf("auth = %q, want bearer-token (no internal impersonation detail)", doc.Auth)
	}
	if doc.TokenProvisioning == "" {
		t.Error("expected a token_provisioning field so a cold agent can bootstrap")
	}
	if len(doc.Surfaces) != 10 {
		t.Errorf("expected 10 surfaces, got %d", len(doc.Surfaces))
	}
	if len(doc.Images) == 0 {
		t.Error("expected images")
	}
}
