package env

import "testing"

func TestBackendURLRenamed(t *testing.T) {
	t.Setenv("SPOOND_BACKEND_URL", "https://new:8890")
	t.Setenv("FORKD_BACKEND_URL", "https://old:8890")
	if got := Get("SPOOND_BACKEND_URL", "d"); got != "https://new:8890" {
		t.Fatalf("SPOOND_BACKEND_URL must win, got %q", got)
	}
}

func TestBackendURLFallback(t *testing.T) {
	t.Setenv("FORKD_BACKEND_URL", "https://old:8890")
	if got := Get("SPOOND_BACKEND_URL", "d"); got != "https://old:8890" {
		t.Fatalf("FORKD_BACKEND_URL fallback must still work, got %q", got)
	}
}

func TestAgentTokenRenamed(t *testing.T) {
	t.Setenv("SPOOND_AGENT_TOKEN", "new-token")
	t.Setenv("FORKD_AGENT_TOKEN", "old-token")
	if got := Get("SPOOND_AGENT_TOKEN", ""); got != "new-token" {
		t.Fatalf("SPOOND_AGENT_TOKEN must win, got %q", got)
	}
}

func TestAgentTokenFallback(t *testing.T) {
	t.Setenv("FORKD_AGENT_TOKEN", "old-token")
	if got := Get("SPOOND_AGENT_TOKEN", ""); got != "old-token" {
		t.Fatalf("FORKD_AGENT_TOKEN fallback must still work, got %q", got)
	}
}

func TestImageRenamed(t *testing.T) {
	t.Setenv("SPOOND_IMAGE", "go-base")
	t.Setenv("FORKD_IMAGE", "dev-base")
	if got := Get("SPOOND_IMAGE", "d"); got != "go-base" {
		t.Fatalf("SPOOND_IMAGE must win, got %q", got)
	}
}

func TestImageFallback(t *testing.T) {
	t.Setenv("FORKD_IMAGE", "dev-base")
	if got := Get("SPOOND_IMAGE", "d"); got != "dev-base" {
		t.Fatalf("FORKD_IMAGE fallback must still work, got %q", got)
	}
}

func TestLLMModelRenamed(t *testing.T) {
	t.Setenv("SPOOND_LLM_MODEL", "m2")
	t.Setenv("FORKD_LLM_MODEL", "m1")
	if got := Get("SPOOND_LLM_MODEL", "d"); got != "m2" {
		t.Fatalf("SPOOND_LLM_MODEL must win, got %q", got)
	}
}

func TestLLMModelFallback(t *testing.T) {
	t.Setenv("FORKD_LLM_MODEL", "m1")
	if got := Get("SPOOND_LLM_MODEL", "d"); got != "m1" {
		t.Fatalf("FORKD_LLM_MODEL fallback must still work, got %q", got)
	}
}

func TestCtlHostRenamed(t *testing.T) {
	t.Setenv("SPOOND_CTL_HOST", "new.example.com")
	t.Setenv("FORKD_CTL_HOST", "old.example.com")
	if got := Get("SPOOND_CTL_HOST", "d"); got != "new.example.com" {
		t.Fatalf("SPOOND_CTL_HOST must win, got %q", got)
	}
}

func TestCtlHostFallback(t *testing.T) {
	t.Setenv("FORKD_CTL_HOST", "old.example.com")
	if got := Get("SPOOND_CTL_HOST", "d"); got != "old.example.com" {
		t.Fatalf("FORKD_CTL_HOST fallback must still work, got %q", got)
	}
}

func TestCtlPortRenamed(t *testing.T) {
	t.Setenv("SPOOND_CTL_PORT", "2200")
	t.Setenv("FORKD_CTL_PORT", "2201")
	if got := Get("SPOOND_CTL_PORT", "d"); got != "2200" {
		t.Fatalf("SPOOND_CTL_PORT must win, got %q", got)
	}
}

func TestCtlPortFallback(t *testing.T) {
	t.Setenv("FORKD_CTL_PORT", "2201")
	if got := Get("SPOOND_CTL_PORT", "d"); got != "2201" {
		t.Fatalf("FORKD_CTL_PORT fallback must still work, got %q", got)
	}
}

func TestCtlKeyRenamed(t *testing.T) {
	t.Setenv("SPOOND_CTL_KEY", "/new/key")
	t.Setenv("FORKD_CTL_KEY", "/old/key")
	if got := Get("SPOOND_CTL_KEY", "d"); got != "/new/key" {
		t.Fatalf("SPOOND_CTL_KEY must win, got %q", got)
	}
}

func TestCtlKeyFallback(t *testing.T) {
	t.Setenv("FORKD_CTL_KEY", "/old/key")
	if got := Get("SPOOND_CTL_KEY", "d"); got != "/old/key" {
		t.Fatalf("FORKD_CTL_KEY fallback must still work, got %q", got)
	}
}

func TestGatewayHostRenamed(t *testing.T) {
	t.Setenv("SPOOND_GATEWAY_HOST", "new.example.com")
	t.Setenv("FORKD_GATEWAY_HOST", "old.example.com")
	if got := Get("SPOOND_GATEWAY_HOST", "d"); got != "new.example.com" {
		t.Fatalf("SPOOND_GATEWAY_HOST must win, got %q", got)
	}
}

func TestGatewayHostFallback(t *testing.T) {
	t.Setenv("FORKD_GATEWAY_HOST", "old.example.com")
	if got := Get("SPOOND_GATEWAY_HOST", "d"); got != "old.example.com" {
		t.Fatalf("FORKD_GATEWAY_HOST fallback must still work, got %q", got)
	}
}

func TestUnsetReturnsDefault(t *testing.T) {
	if got := Get("SPOOND_CTL_PORT", "2222"); got != "2222" {
		t.Fatalf("unset variable must return the default, got %q", got)
	}
}
