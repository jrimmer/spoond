// forkd-acp is the native Agent Client Protocol (ACP) endpoint for
// forkd-service (ticket #24).
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
//	FORKD_TOKEN=<consumer token> \
//	FORKD_LLM_MODEL=gpt-oss-20b-fireworks \
//	forkd-acp
//
// Buzz config (config-only integration):
//
//	agent_command: forkd-acp
//	agent_command_env: { FORKD_BACKEND_URL: ..., FORKD_TOKEN: ... }
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jrimmer/forkd-service/acp"
	"github.com/jrimmer/forkd-service/runner"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	backendURL := envOr("FORKD_BACKEND_URL", "https://127.0.0.1:8890")
	token := os.Getenv("FORKD_TOKEN")
	if token == "" {
		log.Fatal("FORKD_TOKEN is required")
	}
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
	}
	srv.Close(ctx)
}
