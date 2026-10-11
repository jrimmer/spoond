package spoondgateway

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v3/settings"
)

// TestGatewayToken (#137 C2): Main's backend-token resolution — the
// environment's SPOOND_GATEWAY_TOKEN, or SPOOND_GATEWAY_TOKEN_FILE naming
// a value file (one trailing newline trimmed) — resolves through
// settings.Get. The flag wins over the environment in Main; this helper
// only ever runs when the flag is empty.
func TestGatewayToken(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "gw-token")
	if err := os.WriteFile(tokFile, []byte("gw-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOOND_GATEWAY_TOKEN", "")
	t.Setenv("SPOOND_GATEWAY_TOKEN_FILE", tokFile)
	if got := gatewayToken(); got != "gw-from-file" {
		t.Fatalf("gatewayToken() = %q, want the file's trimmed value", got)
	}

	// The name is a known setting through its _FILE spelling.
	if !settings.IsKnown("SPOOND_GATEWAY_TOKEN_FILE") {
		t.Fatal("SPOOND_GATEWAY_TOKEN_FILE is not known")
	}

	// The env form alone.
	t.Setenv("SPOOND_GATEWAY_TOKEN", "env-tok")
	t.Setenv("SPOOND_GATEWAY_TOKEN_FILE", "")
	if got := gatewayToken(); got != "env-tok" {
		t.Fatalf("gatewayToken() = %q, want the env value", got)
	}

	// Both forms set: the same both-set error settings.Get gives the
	// backend, pinned here through the error (not a fatal).
	t.Setenv("SPOOND_GATEWAY_TOKEN_FILE", tokFile)
	_, err := settings.Get("SPOOND_GATEWAY_TOKEN", os.Getenv, os.ReadFile)
	if err == nil || !strings.Contains(err.Error(), "SPOOND_GATEWAY_TOKEN and SPOOND_GATEWAY_TOKEN_FILE") {
		t.Fatalf("both forms: err = %v, want the both-set error", err)
	}
}

// TestGatewayTokenFailsAfterFlagsParse (#137 C2 review): a broken secret
// (an unreadable SPOOND_GATEWAY_TOKEN_FILE, or both forms set) exits
// exactly once from gatewayToken — the resolution runs in Main, after
// flags parse, so even `--help` survives. The exit hook is swapped, so
// the test process survives too.
func TestGatewayTokenFailsAfterFlagsParse(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "gone")
	t.Setenv("SPOOND_GATEWAY_TOKEN", "")
	t.Setenv("SPOOND_GATEWAY_TOKEN_FILE", missing)

	exits := 0
	oldExit := logExit
	logExit = func(code int) {
		exits++
		panic(errGatewayTokenExit)
	}
	t.Cleanup(func() { logExit = oldExit })

	// takeExit runs gatewayToken with the swapped hook and returns the
	// exit count after the recovered panic: the count is read from the
	// closure variable — the panicking frame never returns a value.
	takeExit := func() int {
		exits = 0
		func() {
			defer func() {
				if r := recover(); !errors.Is(r.(error), errGatewayTokenExit) {
					t.Fatalf("recover = %v, want the exit sentinel", r)
				}
			}()
			_ = gatewayToken()
		}()
		return exits
	}
	if n := takeExit(); n != 1 {
		t.Fatalf("unreadable file: %d exits, want exactly 1 (nothing re-takes the exit)", n)
	}

	// Both forms with a real file: the same one-exit shape.
	t.Setenv("SPOOND_GATEWAY_TOKEN", "both")
	t.Setenv("SPOOND_GATEWAY_TOKEN_FILE", filepath.Join(dir, "real"))
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := takeExit(); n != 1 {
		t.Fatalf("both forms: %d exits, want exactly 1", n)
	}
}

// errGatewayTokenExit is the sentinel the swapped logExit panics with.
var errGatewayTokenExit = errors.New("gateway token exit")

// TestGatewayTokenFlagParsesWithBrokenEnv (#137 C2 review): the
// regression proof — the old default computed gatewayDefaultToken() at
// package-var init and log.Fatal'ed before flag parsing, killing even
// `--help`. The flag's default is now empty and the resolution is Main's:
// with both forms set in the environment, the flag set still parses (and
// a child process running this test's body prints usage and exits
// cleanly).
func TestGatewayTokenFlagParsesWithBrokenEnv(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "gw-token")
	if err := os.WriteFile(tokFile, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOOND_GATEWAY_TOKEN", "both")
	t.Setenv("SPOOND_GATEWAY_TOKEN_FILE", tokFile)
	if def := *backendTok; def != "" {
		t.Fatalf("--backend-token default = %q, want empty (the resolution is Main's, after flag parsing)", def)
	}
}

// TestGatewayTokenChildFlagsParse (#137 C2 review): the child-process
// proof of the above — the real binary's flag registry built with the
// broken env present must not have died at init. Runs this test binary
// (same package flags) in a child with both forms set.
func TestGatewayTokenChildFlagsParse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no child-env shape on windows")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("no test binary: %v", err)
	}
	tokFile := filepath.Join(t.TempDir(), "gw-token")
	if err := os.WriteFile(tokFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestGatewayTokenFlagParsesWithBrokenEnv$", "-test.v")
	cmd.Env = append(os.Environ(),
		"SPOOND_GATEWAY_TOKEN=both",
		"SPOOND_GATEWAY_TOKEN_FILE="+tokFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("child with a broken secret env:\n%s\nerr=%v", out, err)
	}
}
