// spoond acp is the native Agent Client Protocol (ACP) endpoint for
// spoond (ticket #24).
//
// Hosts like Buzz's buzz-acp spawn it as a subprocess and speak
// newline-delimited JSON-RPC 2.0 over stdio: initialize, session/new,
// session/prompt, session/cancel. Sessions map to spoond microVM
// leases; the agent loop prompts the LLM gateway and executes
// tool calls (shell, read_file, write_file) inside the lease. Keys
// stay off-VM.
//
// Usage:
//
//	SPOOND_BACKEND_URL=https://127.0.0.1:8890 \
//	SPOOND_AGENT_TOKEN=<agent token> \
//	SPOOND_LLM_MODEL=gpt-oss-20b-fireworks \
//	spoond acp
//
// SPOOND_AGENT_TOKEN is the per-agent credential for this endpoint,
// provisioned from the spoond users store (kind=agent, e.g. via
// `ssh-key add <pubkey> <name>` or POST /api/users). It is sent as a
// bearer token on every lease API call, so the leases this agent's
// sessions hold are owned by it. The pre-2.0 name FORKD_AGENT_TOKEN
// still works but logs a deprecation warning.
//
// Buzz config (config-only integration):
//
//	agent_command: spoond acp
//	agent_command_env: { SPOOND_BACKEND_URL: ..., SPOOND_AGENT_TOKEN: ... }
package spoondacp

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jrimmer/spoond/v2/acp"
	"github.com/jrimmer/spoond/v2/internal/env"
	"github.com/jrimmer/spoond/v2/runner"
)

// agentToken resolves the credential for the lease client. The per-agent
// SPOOND_AGENT_TOKEN is required. Missing it is a fatal configuration
// error.
func agentToken() string {
	if t := env.Get("SPOOND_AGENT_TOKEN", ""); t != "" {
		return t
	}
	log.Fatal("SPOOND_AGENT_TOKEN is not set: create an agent user (ssh-key add <pubkey> <name> or POST /api/users with kind=agent) and set SPOOND_AGENT_TOKEN to its token")
	return ""
}

func Main(args []string) int {
	backendURL := env.Get("SPOOND_BACKEND_URL", "https://127.0.0.1:8890")
	token := agentToken()
	model := env.Get("SPOOND_LLM_MODEL", "gpt-oss-20b-fireworks")
	image := env.Get("SPOOND_IMAGE", "dev-base")

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
		Log:   log.New(os.Stderr, "spoond-acp: ", log.LstdFlags),
	})

	ctx := context.Background()
	if err := srv.Run(ctx); err != nil {
		log.Printf("run: %v", err)
		return 1
	}
	srv.Close(ctx)
	return 0
}
