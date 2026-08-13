package capabilities

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildReturnsAllSurfaces(t *testing.T) {
	doc := Build()
	if doc.Name == "" {
		t.Fatal("expected non-empty name")
	}
	if doc.Version == "" {
		t.Fatal("expected non-empty version")
	}
	if doc.Auth == "" || doc.TokenProvisioning == "" {
		t.Fatal("expected auth and token_provisioning")
	}
	got := map[string]bool{}
	for _, s := range doc.Surfaces {
		got[s.Name] = true
	}
	for _, want := range []string{
		"lease-api", "identity-api", "ssh-gateway", "http-proxy", "llm-gateway",
		"mcp", "acp", "ci-runner", "forkd-cli", "guest-agent",
	} {
		if !got[want] {
			t.Errorf("missing surface %q", want)
		}
	}
	if len(doc.Surfaces) != 10 {
		t.Errorf("expected 10 surfaces, got %d", len(doc.Surfaces))
	}
}

// TestMCPAndACPListsMatchRegistries guards against drift: the MCP tool
// list and ACP method list are single-sourced from the mcp/acp packages,
// so this asserts the canonical values they must produce.
func TestMCPAndACPListsMatchRegistries(t *testing.T) {
	doc := Build()
	for _, s := range doc.Surfaces {
		switch s.Name {
		case "mcp":
			if got := strings.Join(s.Tools, ","); got != "shell,read_file,write_file,edit_file,list_files,status" {
				t.Errorf("mcp tools = %q, want the canonical 6", got)
			}
		case "acp":
			if got := strings.Join(s.Methods, ","); got != "initialize,session/new,session/prompt,session/cancel" {
				t.Errorf("acp methods = %q, want the canonical 4", got)
			}
		}
	}
}

// TestImageDefaults asserts the load-bearing per-image bake sizes so a
// drifted memory/disk default is caught before it misprovisions a sandbox.
func TestImageDefaults(t *testing.T) {
	doc := Build()
	imgs := map[string]Image{}
	for _, img := range doc.Images {
		imgs[img.Tag] = img
	}
	if imgs["dev-base"].MemoryMiB != 2048 {
		t.Errorf("dev-base memory = %d, want 2048", imgs["dev-base"].MemoryMiB)
	}
	if imgs["elixir-base"].DiskMiB != 3072 {
		t.Errorf("elixir-base disk = %d, want 3072", imgs["elixir-base"].DiskMiB)
	}
	rust := imgs["rust-base"]
	if rust.MemoryMiB != 4096 || rust.DiskMiB != 8192 || rust.Baked {
		t.Errorf("rust-base = %+v, want mem=4096 disk=8192 baked=false", rust)
	}
}

func TestBuildJSONContainsKeyFields(t *testing.T) {
	b, err := json.Marshal(Build())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"name":"spoond"`, `"version":"1"`, `"memory_mib":4096`,
		`"tag":"rust-base"`, `"token_provisioning"`, `"lease-api"`,
		`"identity-api"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled JSON missing %q", want)
		}
	}
}
