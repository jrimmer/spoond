// forkd-acp is the native Agent Client Protocol (ACP) endpoint for
// spoond (ticket #24).
//
// Hosts like Buzz's buzz-acp spawn it as a subprocess and speak
// newline-delimited JSON-RPC 2.0 over stdio: initialize, session/new,
// session/prompt, session/cancel. Sessions map to forkd microVM
// leases; the agent loop prompts the forkd LLM gateway and executes
// tool calls (shell, read_file, write_file) inside the lease. Keys
// stay off-VM.
//
// Usage:
//
//	FORKD_BACKEND_URL=https://127.0.0.1:8890 \
//	FORKD_AGENT_TOKEN=<agent token> \
//	FORKD_LLM_MODEL=gpt-oss-20b-fireworks \
//	forkd-acp
//
// FORKD_AGENT_TOKEN is the per-agent credential for this endpoint,
// provisioned from the spoond users store (kind=agent, e.g. via
// `ssh-key add <pubkey> <name>` or POST /api/users). It is sent as a
// bearer token on every lease API call, so the leases this agent's
// sessions hold are owned by it. Legacy setups may set FORKD_TOKEN
// instead; it is honored as a fallback with a deprecation warning.
//
// Buzz config (config-only integration):
//
//	agent_command: forkd-acp
//	agent_command_env: { FORKD_BACKEND_URL: ..., FORKD_AGENT_TOKEN: ... }
package spoondacp

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jrimmer/spoond/acp"
	"github.com/jrimmer/spoond/runner"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// agentToken resolves the credential for the lease client. The per-agent
// FORKD_AGENT_TOKEN wins; FORKD_TOKEN is the legacy fallback (deprecated,
// logged). Missing both is a fatal configuration error.
func agentToken() string {
	if t := os.Getenv("FORKD_AGENT_TOKEN"); t != "" {
		return t
	}
	if t := os.Getenv("FORKD_TOKEN"); t != "" {
		log.Printf("warning: FORKD_TOKEN is deprecated for this endpoint; create an agent user and set FORKD_AGENT_TOKEN instead")
		return t
	}
	log.Fatal("neither FORKD_AGENT_TOKEN nor FORKD_TOKEN is set: create an agent user (ssh-key add <pubkey> <name> or POST /api/users with kind=agent) and set FORKD_AGENT_TOKEN to its token")
	return ""
}

func Main(args []string) int {
	backendURL := envOr("FORKD_BACKEND_URL", "https://127.0.0.1:8890")
	token := agentToken()
	model := envOr("FORKD_LLM_MODEL", "gpt-oss-20b-fireworks")
	image := envOr("FORKD_IMAGE", "dev-base")

	sandbox := runner.NewHTTPLeaseClient(backendURL, token)
	llm := &acp.LLMClient{
		BaseURL: backendURL,
		Model:   model,
		Client:  &http.Client{Timeout: 120 * time.Second},
	}
	agent := acp.NewAgent(sandbox, llm, image, 1800, 12)

	srv := acp.New(acp.Config{
		Agent: agent,
		In:    os.Stdin,
		Out:   os.Stdout,
		Log:   log.New(os.Stderr, "forkd-acp: ", log.LstdFlags),
	})

	ctx := context.Background()
	if err := srv.Run(ctx); err != nil {
		log.Printf("run: %v", err)
		return 1
	}
	srv.Close(ctx)
	return 0
}
