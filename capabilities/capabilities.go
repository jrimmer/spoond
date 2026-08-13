// Package capabilities is spoond's self-description document (issue #52):
// a single stable JSON surface that enumerates every way an agent host can
// reach spoond. The same document is served by GET /v1/capabilities on the
// backend and printed by `spoond doctor capabilities`, so agent hosts can
// discover the surface cold instead of relying on tribal knowledge.
//
// The MCP tool list and ACP method list are single-sourced from the mcp and
// acp packages (ToolNames()/MethodNames()) so they cannot drift. The lease
// API endpoints, ctl verbs, and per-image sizes are maintained in sync with
// docs/api.md, docs/ctl.md, and images/manifest.yaml respectively.
package capabilities

import (
	"github.com/jrimmer/spoond/acp"
	"github.com/jrimmer/spoond/mcp"
)

// Document is the top-level self-description.
type Document struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Auth    string `json:"auth"`
	// TokenProvisioning tells a cold agent how to obtain a bearer token.
	TokenProvisioning string    `json:"token_provisioning,omitempty"`
	Surfaces          []Surface `json:"surfaces"`
	Images            []Image   `json:"images"`
}

// Surface describes one reachable spoond surface (API, gateway, MCP, ...).
type Surface struct {
	Name        string   `json:"name"`
	Reach       string   `json:"reach,omitempty"`
	Auth        string   `json:"auth,omitempty"`
	Transports  []string `json:"transports,omitempty"`
	Endpoints   []string `json:"endpoints,omitempty"`
	CtlVerbs    []string `json:"ctl_verbs,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Methods     []string `json:"methods,omitempty"`
	Entrypoints []string `json:"entrypoints,omitempty"`
	Env         []string `json:"env,omitempty"`
	Note        string   `json:"note,omitempty"`
}

// Image describes a known sandbox image and its per-image defaults.
type Image struct {
	Tag       string `json:"tag"`
	MemoryMiB int    `json:"memory_mib,omitempty"`
	DiskMiB   int    `json:"disk_mib,omitempty"`
	Baked     bool   `json:"baked"`
	Note      string `json:"note,omitempty"`
}

// Build returns the static capability document. Image memory/disk values
// are the documented per-image bake sizes from images/manifest.yaml
// (runtime per-image sizing is tracked in issue #38).
func Build() Document {
	return Document{
		Name:    "spoond",
		Version: "1",
		Auth:    "bearer-token",
		TokenProvisioning: "obtain an agent token via `POST /api/users` " +
			"({name, kind:\"agent\", token, fingerprints:[<ssh key sha256 fingerprint>]}) " +
			"or `ssh ctl@<host> ssh-key add <pubkey> <name>`; then send `Authorization: Bearer <token>`",
		Surfaces: []Surface{
			{
				Name:  "lease-api",
				Reach: "HTTP(S) :8890 (HTTPS when TLS_CERT/TLS_KEY are configured)",
				Auth:  "bearer-token",
				Endpoints: []string{
					"POST /api/sandboxes", "GET /api/sandboxes",
					"POST /api/sandboxes/{id}/exec", "DELETE /api/sandboxes/{id}",
					"POST /api/sandboxes/{id}/keepalive", "POST /api/sandboxes/{id}/suspend",
					"POST /api/sandboxes/{id}/resume", "POST /api/sandboxes/{id}/restart",
					"POST /api/sandboxes/{id}/tag", "POST /api/sandboxes/{id}/comment",
					"POST /api/sandboxes/{id}/prompt", "GET /api/sandboxes/{id}/endpoint",
					"GET /api/sandboxes/{id}/stat", "GET /api/sandboxes/{id}/stream",
					"POST /api/sandboxes/{id}/clone", "POST /api/sandboxes/{id}/share",
					"DELETE /api/sandboxes/{id}/share/{grantee}", "GET /api/shares",
					"GET /api/images", "GET /api/names/{name}",
				},
				Note: "create/list/exec/suspend/resume/restart/clone/share/tag/comment/prompt/stat/stream sandboxes",
			},
			{
				Name:  "identity-api",
				Reach: "HTTP(S) :8890 (same as lease-api)",
				Auth:  "bearer-token (some routes admin-only)",
				Endpoints: []string{
					"GET /api/users", "GET /api/users/me", "GET /api/users/by-name/{name}",
					"GET /api/users/by-key", "GET /api/identity-status", "POST /api/users",
					"POST /api/users/{id}/quota", "POST /api/users/{id}/llm-key", "DELETE /api/users/{id}",
				},
				Note: "user + agent identity management; registered only when an identity store is configured",
			},
			{
				Name:     "ssh-gateway",
				Reach:    ":2222",
				Auth:     "ssh key",
				CtlVerbs: []string{"new", "ls", "stat", "rm", "keepalive", "suspend", "resume", "restart", "cp", "shelly", "tag", "comment", "prompt", "ssh-key", "share", "whoami", "help"},
				Note:     "ssh ctl@<host> '<verb>' for the control plane; ssh <id|name>@<host> to attach to a sandbox",
			},
			{
				Name:  "http-proxy",
				Reach: ":8891",
				Note:  "wildcard hostname -> sandbox HTTP (per-user proxy hostnames)",
			},
			{
				Name:  "llm-gateway",
				Reach: "/llm/<lease-id>/<provider>/... on :8890/:8891",
				Note:  "per-lease LLM access; the model key never enters the VM",
			},
			{
				Name:        "mcp",
				Transports:  []string{"stdio", "http"},
				Tools:       mcp.ToolNames(),
				Entrypoints: []string{"spoond mcp"},
				Env:         []string{"FORKD_BACKEND_URL", "FORKD_AGENT_TOKEN", "FORKD_IMAGE", "MCP_TRANSPORT (stdio|http)", "MCP_LISTEN (:9090)", "MCP_AUTH_TOKEN"},
				Note:        "Model Context Protocol server; stdio or Streamable HTTP/SSE (MCP_TRANSPORT=http, MCP_LISTEN=:9090); the HTTP transport requires a bearer token (MCP_AUTH_TOKEN, defaults to the agent token)",
			},
			{
				Name:        "acp",
				Transports:  []string{"stdio"},
				Methods:     acp.MethodNames(),
				Entrypoints: []string{"spoond acp"},
				Env:         []string{"FORKD_BACKEND_URL", "FORKD_AGENT_TOKEN", "FORKD_IMAGE", "FORKD_LLM_MODEL"},
				Note:        "Agent Client Protocol server; session-scoped (a session owns a lease); server→client notifications: session/update, session/request_permission",
			},
			{
				Name:  "ci-runner",
				Reach: "Forgejo Actions",
				Note:  "jobs land on forkd sandboxes by runs-on label",
			},
			{
				Name:        "forkd-cli",
				Entrypoints: []string{"from-image", "snapshot", "exec --child", "ping --child", "ls", "branch"},
				Note:        "low-level microVM tooling (the `forkd` binary) on the host; privileged",
			},
			{
				Name:  "guest-agent",
				Reach: "in-VM :8888",
				Note:  "forkd-agent.py (PID 1): JSON-over-TCP ping/exec/eval/stream",
			},
		},
		Images: []Image{
			{Tag: "dev-base", MemoryMiB: 2048, Baked: true, Note: "interactive dev (tmux + sshd + git + python3 + build tools)"},
			{Tag: "py-base", Baked: true, Note: "Python 3.12 slim; runner DEFAULT_IMAGE"},
			{Tag: "go-base", MemoryMiB: 2048, Baked: true, Note: "Go toolchain + git"},
			{Tag: "elixir-base", DiskMiB: 3072, Baked: true, Note: "Elixir/Erlang toolchain + git"},
			{Tag: "llm-review", Baked: true, Note: "Python 3.12 + git + curl for LLM code-review jobs"},
			{Tag: "rust-base", MemoryMiB: 4096, DiskMiB: 8192, Baked: false, Note: "Rust stable toolchain (recommended bake sizes)"},
		},
	}
}
