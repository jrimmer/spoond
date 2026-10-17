package api

import (
	"context"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/jrimmer/spoond/v3/mcp"
	"github.com/jrimmer/spoond/v3/sizes"
)

// The auth model an api.auth line reports (docs/api.md's ownership
// model, condensed to one line).
const capabilitiesAuth = "Bearer user token on the lease, images, snapshots, " +
	"users, usage and sizes routes; a lease is reachable by its owner, a share " +
	"grantee or an admin; admin users and ADMIN_TOKEN reach /api/admin/* and the " +
	"admin user routes; /healthz and /readyz need no token; /metrics takes " +
	"METRICS_TOKEN; the lease id in the path is the capability on " +
	"/llm/{lease-id}/, the proxy hostname and the guest heartbeat"

// capabilityMCP names the MCP server binary and its tool names.
type capabilityMCP struct {
	Binary string   `json:"binary"`
	Tools  []string `json:"tools"`
}

// runnerLabel is one runs-on label and the image it maps to.
type runnerLabel struct {
	Label string `json:"label"`
	Image string `json:"image"`
}

// capabilitySurfaces are the other ways into a sandbox, named only when
// this host configures them (omitempty — a caller must not assume a host
// serves a surface because some other host advertised it).
type capabilitySurfaces struct {
	// SSHGateway is the address `ssh <lease>@<addr>` reaches: GATEWAY_ADDR,
	// the knob `spoond doctor` probes. Empty when the host publishes none.
	SSHGateway string `json:"ssh_gateway,omitempty"`
	// LLMGateway is the per-lease LLM gateway's path prefix; present only
	// when the backend built one (LLM_UPSTREAM_URL set).
	LLMGateway string `json:"llm_gateway,omitempty"`
	// MCP is the stdio MCP server binary this tree ships and the tool
	// names it serves, read from the mcp package's tool table (never
	// hand-copied here).
	MCP *capabilityMCP `json:"mcp,omitempty"`
	// CIRunner lists the runs-on labels the CI runner maps to images,
	// when the backend has that table (IMAGE_MAP); omitted otherwise —
	// the backend does not know a runner that is not its process.
	CIRunner []runnerLabel `json:"ci_runner,omitempty"`
}

// capabilityImage describes one catalog image. vcpu, memory_mb, disk_mb
// and min_memory_mb are the same values GET /api/images?detail=1
// reports — both read the same ListImages rows through the same
// effectiveMinMemoryMB helper, so the two cannot disagree.
type capabilityImage struct {
	Name        string   `json:"name"`
	Capability  string   `json:"capability,omitempty"`
	Description string   `json:"description,omitempty"`
	VCPU        int      `json:"vcpu"`
	MemoryMB    int      `json:"memory_mb"`
	DiskMB      int      `json:"disk_mb"`
	MinMemoryMB int      `json:"min_memory_mb"`
	WarmSizes   []string `json:"warm_sizes"`
}

// capabilitySizes is GET /api/sizes' block from the same
// sizeLimitsOrDefault fallback, so the two routes report one truth.
type capabilitySizes struct {
	Presets     []sizes.Preset `json:"presets"`
	MaxVCPU     int            `json:"max_vcpu"`
	MaxMemoryMB int            `json:"max_memory_mb"`
}

// capabilityNetPolicy is one egress policy and its one-line meaning,
// matching docs/api.md's Network policy table.
type capabilityNetPolicy struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// netpolicyTable is that table, the same four classes as the docs.
var netpolicyTable = []capabilityNetPolicy{
	{"none", "nothing at all"},
	{"lan", "the LAN ranges (RFC 1918 minus the lease networks), host services, DNS, and every exposing peer's published ports"},
	{"internet", "everything public, plus the LAN ranges, host services, DNS, and every exposing peer's published ports"},
	{"restricted", "host services, DNS, the egress allowlist, and the published ports of the peers the allowlist names (default)"},
}

// capabilityAPI describes the API family: where it lives under this URL
// and how a caller authenticates.
type capabilityAPI struct {
	// Base is the prefix every versioned route lives at. Routes are
	// named absolute in endpoints below so a caller can join either
	// way; both spellings are stable (#52).
	Base string `json:"base"`
	// Primary is the documented lease path prefix; Auth the auth
	// model, from docs/api.md's "Who can do what".
	Primary string `json:"primary"`
	// Auth is the auth model, from docs/api.md's "Who can do what".
	Auth string `json:"auth"`
}

// endpointSpec is one row of the endpoint table.
type endpointSpec struct {
	// Method and Path are the handler registration's "METHOD /path",
	// with the mux's exact wildcards ({id}, {name...}, …).
	Method string `json:"method"`
	Path   string `json:"path"`
	// Summary is the route's one-liner, from its heading in docs/api.md.
	Summary string `json:"summary"`
	// Admin marks a route whose ordinary grant is an admin user or the
	// ADMIN_TOKEN: the /api/admin/* routes and the admin-only user
	// routes. Everything else is owner-scoped for any valid token, and
	// nothing in this document ever names a secret value or another
	// user's data.
	Admin bool `json:"admin,omitempty"`
}

// endpointSpec{method, path, summary} (ep) and, when the route's
// ordinary grant is an admin user or the ADMIN_TOKEN,
// endpointSpec{method, path, summary, admin:true} (epAdmin) keep the
// table one line per route.
var _ = endpointSpec{}

func ep(method, path, summary string) endpointSpec {
	return endpointSpec{Method: method, Path: path, Summary: summary}
}
func epAdmin(method, path, summary string) endpointSpec {
	return endpointSpec{Method: method, Path: path, Summary: summary, Admin: true}
}

// capabilityJSON is the response body of GET /api/capabilities (#52):
// everything an agent or person that only knows the API URL needs to
// plan work against this host. Stable field names; lists are always
// present (an empty list where a host configures nothing), surfaces
// only when present.
type capabilityJSON struct {
	// Version is the binary's version string (`spoond version`).
	Version string        `json:"version"`
	API     capabilityAPI `json:"api"`
	// Endpoints is every route the backend registers — endpointTable
	// below, kept equal to the mux by capabilities_test.go's
	// registration scan.
	Endpoints       []endpointSpec        `json:"endpoints"`
	Images          []capabilityImage     `json:"images"`
	Sizes           capabilitySizes       `json:"sizes"`
	NetworkPolicies []capabilityNetPolicy `json:"network_policies"`
	Surfaces        capabilitySurfaces    `json:"surfaces"`
}

// endpointTable lists every route the backend registers in api/server.go,
// in registration order. capabilities_test.go reads server.go's
// mux.HandleFunc registrations and fails on drift: a route registered
// without a row here, or a row the mux no longer registers (wrong path,
// method or count), is a test failure — the anti-drift guarantee. The
// /api/leases twin routes are not rows: they are the same registrations
// reached through the rewrite in server.go's Handler (one route table,
// one auth path); the document's api block names the alias instead.
var endpointTable = []endpointSpec{
	ep("POST", "/api/leases", "create a lease"),
	ep("GET", "/api/leases", "list leases"),
	ep("GET", "/api/leases/queue", "list creates waiting for admission"),
	ep("GET", "/api/leases/{id}", "lease detail"),
	ep("POST", "/api/leases/{id}/exec", "run a command"),
	ep("GET", "/api/leases/{id}/jobs", "list background jobs"),
	ep("GET", "/api/leases/{id}/jobs/{job}", "read one job"),
	ep("GET", "/api/leases/{id}/jobs/{job}/output", "stream job output"),
	ep("POST", "/api/leases/{id}/jobs/{job}/signal", "signal a job"),
	ep("DELETE", "/api/leases/{id}", "delete"),
	ep("POST", "/api/leases/{id}/keepalive", "extend a persistent lease"),
	ep("POST", "/api/leases/{id}/suspend", "snapshot + stop"),
	ep("POST", "/api/leases/{id}/resume", "start from the snapshot"),
	ep("POST", "/api/leases/{id}/restart", "pause and resume, or a fresh guest"),
	ep("POST", "/api/leases/{id}/checkpoint", "snapshot a running lease"),
	ep("POST", "/api/leases/{id}/tag", "friendly name"),
	ep("POST", "/api/leases/{id}/comment", "annotate"),
	ep("POST", "/api/leases/{id}/prompt", "message the in-sandbox Shelley agent"),
	ep("GET", "/api/leases/{id}/endpoint", "resolve lease endpoint"),
	ep("GET", "/api/leases/{id}/stat", "guest metrics"),
	ep("GET", "/api/leases/{id}/stream", "interactive process (WebSocket)"),
	ep("POST", "/api/leases/{id}/network", "change egress policy live"),
	ep("POST", "/api/leases/{id}/clone", "branch to a new lease"),
	ep("POST", "/api/leases/{id}/fork", "N copies of a running lease"),
	ep("POST", "/api/leases/{id}/share", "grant (owner only)"),
	ep("DELETE", "/api/leases/{id}/share/{grantee}", "revoke (owner only)"),
	ep("GET", "/api/shares", "list shares granted on your leases"),
	epAdmin("GET", "/api/fair-shares", "every owner's slice and usage (admin only)"),
	ep("GET", "/api/images", "list images"),
	ep("GET", "/api/capabilities", "this document: routes, images, sizes, network policies, surfaces (#52)"),
	ep("GET", "/api/names/{name}", "resolve by name"),
	ep("GET", "/api/snapshots", "list your snapshot builds"),
	ep("DELETE", "/api/snapshots/{build_id}", "delete a snapshot build"),
	ep("POST", "/api/leases/{id}/snapshots", "save a lease as a named snapshot"),
	ep("GET", "/api/named-snapshots", "list your named snapshots"),
	ep("GET", "/api/named-snapshots/{name...}", "show one"),
	ep("PUT", "/api/named-snapshots/{name...}", "set retention"),
	ep("DELETE", "/api/named-snapshots/{name...}", "delete"),
	ep("POST", "/api/named-snapshot-grants", "grant a secret-free snapshot name to another user"),
	ep("GET", "/api/named-snapshot-grants", "list grants given and received"),
	ep("DELETE", "/api/named-snapshot-grants", "revoke a grant"),
	epAdmin("POST", "/api/admin/drain", "drain the node"),
	epAdmin("POST", "/api/admin/undrain", "undrain the node"),
	epAdmin("POST", "/api/admin/reconcile", "reconcile with the substrate"),
	epAdmin("GET", "/api/admin/config", "read the unit env settings"),
	epAdmin("POST", "/api/admin/unpin-by-holder", "unpin by holder prefix"),
	ep("GET", "/api/leases/events", "stream the caller's lease events"),
	ep("GET", "/api/leases/{id}/events", "stream one lease's events"),
	ep("PUT", "/api/leases/{id}/holder", "set or clear the holder label"),
	ep("PUT", "/api/leases/{id}/pin", "pin"),
	ep("DELETE", "/api/leases/{id}/pin", "unpin"),
	ep("PUT", "/api/leases/{id}/checkpoint-policy", "set the checkpoint interval"),
	ep("PUT", "/api/leases/{id}/idle-policy", "set the idle reclamation threshold"),
	ep("POST", "/api/leases/{id}/restore", "roll back to a kept checkpoint"),
	ep("POST", "/api/leases/{id}/crash-test", "crash a lease and recover it (CRASH_TEST hosts)"),
	ep("GET", "/api/leases/{id}/files/{path...}", "download a lease file"),
	ep("PUT", "/api/leases/{id}/files/{path...}", "upload a lease file"),
	ep("POST", "/api/leases/{id}/files/{path...}", "stat/mkdir/remove a lease path"),
	ep("DELETE", "/api/leases/{id}/files/{path...}", "delete a lease file"),
	ep("GET", "/healthz", "liveness"),
	ep("GET", "/readyz", "readiness"),
	ep("GET", "/metrics", "Prometheus metrics"),
	ep("GET", "/api/leases/{id}/ports/{port}/dial", "raw TCP to a guest port (WebSocket)"),
	epAdmin("GET", "/api/users", "list users (admin only)"),
	ep("GET", "/api/users/me", "current user"),
	epAdmin("GET", "/api/users/{id}", "one owner's usage (admin only)"),
	ep("GET", "/api/users/by-name/{name}", "minimal lookup"),
	ep("GET", "/api/users/by-key", "resolve SSH key"),
	ep("GET", "/api/identity-status", "identity store availability"),
	ep("GET", "/api/users/{id}/", "names a user's tokens (GET /api/users/{id}/tokens)"),
	ep("POST", "/api/users", "create a user"),
	epAdmin("POST", "/api/users/{id}/quota", "set the TTL cap (admin only)"),
	ep("POST", "/api/users/{id}/llm-key", "set/rotate/revoke a user's LLM gateway key"),
	ep("POST", "/api/users/{id}/tokens", "mint a named API token"),
	ep("DELETE", "/api/users/{id}/tokens/{token_id}", "revoke a token"),
	epAdmin("DELETE", "/api/users/{id}", "remove a user (admin only)"),
	ep("GET", "/api/usage", "the caller's fair-share usage"),
	ep("GET", "/api/usage/history", "the caller's 7-day usage history"),
	ep("GET", "/api/sizes", "size presets and per-lease limits"),
}

// VersionFunc builds the version string the capability document
// reports. SetVersion installs it; cmd/spoond's init wires the same
// rendering `spoond version` prints, so the endpoint and the CLI agree.
var versionFunc = func() string { return "dev" }

// SetVersion replaces the version producer (called once from the
// command wiring; a nil or empty render keeps "dev").
func SetVersion(f func() string) {
	if f == nil {
		return
	}
	versionFunc = func() string {
		if v := f(); strings.TrimSpace(v) != "" {
			return v
		}
		return "dev"
	}
}

// Version reports the binary's version string. cmd/spoond sets
// versionFunc at init with the same version its `spoond version`
// prints, so the two never disagree.
func Version() string { return versionFunc() }

// handleCapabilities serves GET /api/capabilities (#52): any valid user
// token, the same auth class as GET /api/images. The answer is always a
// JSON document — a catalog read failure leaves images empty rather
// than 500ing, so a degraded host still describes the rest of itself.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.capabilities())
}

// capabilities assembles the document.
func (s *Server) capabilities() capabilityJSON {
	imgs := []capabilityImage{}
	if rows, err := s.svc.db.ListImages(context.Background()); err == nil {
		for _, row := range rows {
			if row.CurrentBuildID == "" {
				continue // no current build: not offered on this host
			}
			warmSizes := []string{}
			if warm, werr := s.svc.db.ListImageSizes(context.Background(), row.Name); werr == nil {
				for _, ws := range warm {
					warmSizes = append(warmSizes, ws.Size)
				}
				sort.Strings(warmSizes)
			}
			imgs = append(imgs, capabilityImage{
				Name:        row.Name,
				Capability:  ImageCapability(row.Name),
				Description: ImageDescription(row.Name),
				VCPU:        row.VCPU,
				MemoryMB:    row.MemoryMB,
				DiskMB:      row.DiskMB,
				MinMemoryMB: effectiveMinMemoryMB(row),
				WarmSizes:   warmSizes,
			})
		}
	}
	presets, maxVCPU, maxMem := sizeLimitsOrDefault(s.svc.cfg)
	presetsCopy := make([]sizes.Preset, len(presets))
	copy(presetsCopy, presets)
	surfaces := capabilitySurfaces{}
	if addr := sshGatewayAddr(); addr != "" {
		surfaces.SSHGateway = addr
	}
	if s.llm != nil {
		surfaces.LLMGateway = llmGatewayPrefix + "{lease-id}/"
	}
	if names := mcp.DefaultToolNames(); len(names) > 0 {
		surfaces.MCP = &capabilityMCP{Binary: "spoond-dev-mcp", Tools: names}
	}
	if labels := runnerImageTable(); len(labels) > 0 {
		surfaces.CIRunner = labels
	}
	return capabilityJSON{
		Version: versionFunc(),
		API: capabilityAPI{
			Base:    "/api",
			Primary: apiLeasePathPrefix,
			Auth:    capabilitiesAuth,
		},
		Endpoints:       append([]endpointSpec(nil), endpointTable...),
		Images:          imgs,
		Sizes:           capabilitySizes{Presets: presetsCopy, MaxVCPU: maxVCPU, MaxMemoryMB: maxMem},
		NetworkPolicies: append([]capabilityNetPolicy(nil), netpolicyTable...),
		Surfaces:        surfaces,
	}
}

// sshGatewayAddr is the SSH gateway address this host advertises:
// GATEWAY_ADDR, the same setting `spoond doctor` probes and the
// settings registry lists as a Doctor TypeAddr. Empty = not published.
func sshGatewayAddr() string {
	return strings.TrimSpace(os.Getenv("GATEWAY_ADDR"))
}

// runnerImageTable parses IMAGE_MAP ("label=image,..." — the same spec
// spoond-runner reads for its Executor.Labels). Broken pairs are
// skipped; an unset table means this backend does not know the runner's
// labels and the surface is omitted.
func runnerImageTable() []runnerLabel {
	spec := os.Getenv("IMAGE_MAP")
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	var out []runnerLabel
	for _, pair := range strings.Split(spec, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out = append(out, runnerLabel{Label: parts[0], Image: parts[1]})
	}
	return out
}

// imageMeta is the capability/description an image name carries
// (#52): the manifest's (images/manifest.yaml) `capability` and
// `description` fields, kept here as data beside the API. An image the
// table does not know is an image built outside the manifest: the
// document then omits both fields rather than guessing — a host may
// also have catalog rows this source never named.
type imageMeta struct {
	Capability  string
	Description string
}

// imageMetaTable is that data, one row per manifest image, name-keyed.
var imageMetaTable = map[string]imageMeta{
	"py-base":        {"python", "Python 3.12 slim base for Python build/test/CI jobs"},
	"go-base":        {"golang", "Go 1.25 toolchain (go/gofmt) + git for Go build/test/CI jobs"},
	"dev-base":       {"interactive-dev", "Interactive dev environment: tmux + sshd + git + python3 + build tools"},
	"elixir-base":    {"elixir", "Elixir/Erlang OTP toolchain + git for Elixir build/test/CI jobs"},
	"elixir-release": {"elixir-release", "Elixir/OTP + Rust + Node/pnpm + kaniko for full Phoenix+NIF release builds"},
	"llm-review":     {"llm-review", "Python 3.12 + git + curl for LLM code-review jobs (language-agnostic)"},
	"rust-base":      {"rust", "Rust stable toolchain + git for Rust build/test/CI jobs"},
	"rust-qt-base":   {"rust-qt", "Rust (Arch) + Qt 6 for headless GUI tests"},
	"scylla":         {"cql-database", "ScyllaDB single node, developer mode — a SERVICE image leased per job and reached from another lease's sandbox"},
}

// ImageCapability returns the image name's capability (manifest field),
// "" when the table does not know the name.
func ImageCapability(name string) string { return imageMetaTable[name].Capability }

// ImageDescription returns the image name's one-line description
// (manifest field), "" when the table does not know the name.
func ImageDescription(name string) string { return imageMetaTable[name].Description }
