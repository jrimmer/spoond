package spoondgateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v3/settings"
)

// TestGatewayDefaultToken (#137 C2): --backend-token's default resolves
// through settings.Get — SPOOND_GATEWAY_TOKEN in the environment, or
// SPOOND_GATEWAY_TOKEN_FILE naming a value file (one trailing newline
// trimmed); both set or an unreadable file is a fatal. The fatal path
// (log.Fatal inside the flag-default computation) is pinned by
// capturing the logger and checking the process did not get here.
func TestGatewayDefaultToken(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "gw-token")
	if err := os.WriteFile(tokFile, []byte("gw-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOOND_GATEWAY_TOKEN", "")
	t.Setenv("SPOOND_GATEWAY_TOKEN_FILE", tokFile)
	if got := gatewayDefaultToken(); got != "gw-from-file" {
		t.Fatalf("gatewayDefaultToken() = %q, want the file's trimmed value", got)
	}

	// The name is a known setting through its _FILE spelling.
	if !settings.IsKnown("SPOOND_GATEWAY_TOKEN_FILE") {
		t.Fatal("SPOOND_GATEWAY_TOKEN_FILE is not known")
	}

	// Both forms set: the fatal (the captured logger ends it before the
	// helper returns; with the logger's ExitFunc untouched this process
	// would die, so assert through settings.Get directly instead).
	t.Setenv("SPOOND_GATEWAY_TOKEN", "env-tok")
	_, err := settings.Get("SPOOND_GATEWAY_TOKEN", os.Getenv, os.ReadFile)
	if err == nil || !strings.Contains(err.Error(), "SPOOND_GATEWAY_TOKEN and SPOOND_GATEWAY_TOKEN_FILE") {
		t.Fatalf("both forms: err = %v, want the both-set error", err)
	}
}
