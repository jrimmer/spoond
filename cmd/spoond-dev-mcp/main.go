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
//     (defaults to FORKD_AGENT_TOKEN).
//
// Usage (stdio):
//
//	FORKD_BACKEND_URL=https://127.0.0.1:8890 \
//	FORKD_AGENT_TOKEN=<agent token> \
//	spoond mcp
//
// Usage (HTTP):
//
//	MCP_TRANSPORT=http MCP_LISTEN=:9090 \
//	FORKD_BACKEND_URL=https://127.0.0.1:8890 \
//	FORKD_AGENT_TOKEN=<agent token> \
//	spoond mcp
//
// FORKD_AGENT_TOKEN is the per-agent credential for this endpoint,
// provisioned from the spoond users store (kind=agent, e.g. via
// `ssh-key add <pubkey> <name>` or POST /api/users). It is sent as a
// bearer token on every lease API call, so sandboxes this agent creates
// are owned by it.
//
// Agents spawn it as a subprocess, e.g. Goose:
//
//	mcp {
//	  server "spoond-mcp" {
//	    command = "spoond"
//	    args = ["mcp"]
//	    env = { FORKD_BACKEND_URL = "...", FORKD_AGENT_TOKEN = "..." }
//	  }
//	}
package spoondmcp

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/jrimmer/spoond/v2/mcp"
	"github.com/jrimmer/spoond/v2/runner"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// agentToken resolves the credential for the lease client. The per-agent
// FORKD_AGENT_TOKEN is required. Missing it is a fatal configuration
// error.
func agentToken() string {
	if t := os.Getenv("FORKD_AGENT_TOKEN"); t != "" {
		return t
	}
	log.Fatal("FORKD_AGENT_TOKEN is not set: create an agent user (ssh-key add <pubkey> <name> or POST /api/users with kind=agent) and set FORKD_AGENT_TOKEN to its token")
	return ""
}

func Main(args []string) int {
	backendURL := envOr("FORKD_BACKEND_URL", "https://127.0.0.1:8890")
	token := agentToken()
	image := envOr("FORKD_IMAGE", "dev-base")
	transport := envOr("MCP_TRANSPORT", "stdio")
	listenAddr := envOr("MCP_LISTEN", ":9090")
	httpToken := envOr("MCP_AUTH_TOKEN", token) // default to the agent token

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
			PathPrefix: envOr("MCP_PATH", "/mcp"),
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
