// spoond mcp is the MCP server for spoond (ticket #23).
//
// It exposes spoond microVM sandboxes as MCP tools (shell, file ops,
// status) so any MCP-capable agent (Goose, Codex, Claude Code, Pi,
// buzz-agent) gets sandbox-backed execution without knowing the
// substrate exists.
//
// Transports:
//
//   - stdio (default): newline-delimited JSON-RPC 2.0 over stdin/stdout.
//     Agents spawn the server as a subprocess.
//
//   - http: Streamable HTTP transport (2025-03-26 MCP spec) + SSE
//     endpoint for backward compatibility. Set MCP_TRANSPORT=http and
//     MCP_LISTEN=:9090 to enable. Bearer token auth via MCP_AUTH_TOKEN
//     (defaults to SPOOND_AGENT_TOKEN).
//
// Usage (stdio):
//
//	SPOOND_BACKEND_URL=https://127.0.0.1:8890 \
//	SPOOND_AGENT_TOKEN=<agent token> \
//	spoond mcp
//
// Usage (HTTP):
//
//	MCP_TRANSPORT=http MCP_LISTEN=:9090 \
//	SPOOND_BACKEND_URL=https://127.0.0.1:8890 \
//	SPOOND_AGENT_TOKEN=<agent token> \
//	spoond mcp
//
// SPOOND_AGENT_TOKEN is the per-agent credential for this endpoint,
// provisioned from the spoond users store (kind=agent, e.g. via
// `ssh-key add <pubkey> <name>` or POST /api/users). It is sent as a
// bearer token on every lease API call, so sandboxes this agent creates
// are owned by it. The pre-2.0 name FORKD_AGENT_TOKEN still works but
// logs a deprecation warning.
//
// Agents spawn it as a subprocess, e.g. Goose:
//
//	mcp {
//	  server "spoond-mcp" {
//	    command = "spoond"
//	    args = ["mcp"]
//	    env = { SPOOND_BACKEND_URL = "...", SPOOND_AGENT_TOKEN = "..." }
//	  }
//	}
package spoondmcp

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/jrimmer/spoond/v2/internal/env"
	"github.com/jrimmer/spoond/v2/mcp"
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
	image := env.Get("SPOOND_IMAGE", "dev-base")
	transport := env.Get("MCP_TRANSPORT", "stdio")
	listenAddr := env.Get("MCP_LISTEN", ":9090")
	httpToken := env.Get("MCP_AUTH_TOKEN", token) // default to the agent token

	sandbox := runner.NewHTTPLeaseClient(backendURL, token)

	srv := mcp.New(mcp.Config{
		Sandbox: sandbox,
		Image:   image,
		TTL:     600,
		Timeout: 60,
		In:      os.Stdin,
		Out:     os.Stdout,
		Log:     log.New(os.Stderr, "spoond-mcp: ", log.LstdFlags),
	})
	defer srv.Close()

	switch strings.ToLower(transport) {
	case "http", "sse", "streamable":
		httpCfg := mcp.HTTPConfig{
			Addr:       listenAddr,
			Token:      httpToken,
			PathPrefix: env.Get("MCP_PATH", "/mcp"),
		}
		log.Printf("spoond-mcp: HTTP transport on %s (path=%s, auth=%v)",
			httpCfg.Addr, httpCfg.PathPrefix, httpCfg.Token != "")
		if err := srv.RunHTTP(context.Background(), httpCfg); err != nil {
			log.Printf("run http: %v", err)
			return 1
		}
	case "stdio", "":
		if err := srv.Run(context.Background()); err != nil {
			log.Printf("run: %v", err)
			return 1
		}
	default:
		log.Fatalf("unknown MCP_TRANSPORT %q: use stdio or http", transport)
	}
	return 0
}
