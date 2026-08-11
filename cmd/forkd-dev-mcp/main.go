// forkd-dev-mcp is the MCP server for forkd-service (ticket #23).
//
// It exposes forkd microVM sandboxes as MCP tools (shell, file ops,
// status) so any MCP-capable agent (Goose, Codex, Claude Code,
// buzz-agent) gets sandbox-backed execution without knowing forkd
// exists. Transport: stdio (newline-delimited JSON-RPC 2.0).
//
// Usage:
//
//	FORKD_BACKEND_URL=https://127.0.0.1:8890 \
//	FORKD_TOKEN=<consumer token> \
//	forkd-dev-mcp
//
// Agents spawn it as a subprocess, e.g. Goose:
//
//	mcp {
//	  server "forkd-dev-mcp" {
//	    command = "forkd-dev-mcp"
//	    env = { FORKD_BACKEND_URL = "...", FORKD_TOKEN = "..." }
//	  }
//	}
package main

import (
	"context"
	"log"
	"os"

	"github.com/jrimmer/forkd-service/mcp"
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
	image := envOr("FORKD_IMAGE", "dev-base")

	sandbox := runner.NewHTTPLeaseClient(backendURL, token)

	srv := mcp.New(mcp.Config{
		Sandbox: sandbox,
		Image:   image,
		TTL:     600,
		Timeout: 60,
		In:      os.Stdin,
		Out:     os.Stdout,
		Log:     log.New(os.Stderr, "forkd-dev-mcp: ", log.LstdFlags),
	})
	defer srv.Close()

	if err := srv.Run(context.Background()); err != nil {
		log.Printf("run: %v", err)
		os.Exit(1)
	}
}
