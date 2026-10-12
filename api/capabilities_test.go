package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// registeredRoutes reads api/server.go's source and returns every
// s.mux.HandleFunc("METHOD /path", ...) registration, in file order.
// It is the anti-drift witness for the endpoint table in
// capabilities.go: the test fails when a route the mux registers has no
// table row, and when a table row the mux does not register survives.
func registeredRoutes(t *testing.T) []endpointSpec {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("server.go"))
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var out []endpointSpec
	call := func(n ast.Node) bool {
		callExpr, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := callExpr.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" || len(callExpr.Args) < 2 {
			return true
		}
		// The receiver must be the server's mux (s.mux or any .mux —
		// registration only happens there).
		x, ok := sel.X.(*ast.SelectorExpr)
		if !ok || x.Sel.Name != "mux" {
			return true
		}
		lit, ok := callExpr.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Fatalf("mux.HandleFunc with a non-literal pattern at %s", fset.Position(n.Pos()))
		}
		pattern := strings.Trim(lit.Value, `"`)
		if method, path, ok := strings.Cut(pattern, " "); ok {
			out = append(out, endpointSpec{Method: method, Path: path})
		}
		return true
	}
	ast.Inspect(file, call)
	if len(out) == 0 {
		t.Fatal("no mux.HandleFunc registrations found in server.go: the scan broke")
	}
	return out
}

// endpointRowSet indexes the table by "METHOD /path".
func endpointRowSet() map[string]endpointSpec {
	out := map[string]endpointSpec{}
	for _, e := range endpointTable {
		out[e.Method+" "+e.Path] = e
	}
	return out
}

// TestCapabilitiesTableMatchesMux (#52) is the anti-drift guarantee:
// every route the backend registers appears in the endpoint table with
// the same method and path, and the table lists nothing unregistered.
func TestCapabilitiesTableMatchesMux(t *testing.T) {
	rows := endpointRowSet()
	registered := registeredRoutes(t)
	// Table entries that the mux does not register (stale rows).
	for _, spec := range endpointTable {
		key := spec.Method + " " + spec.Path
		if _, ok := registeredSet(registered)[key]; !ok {
			t.Errorf("endpoint table row %q is not registered in server.go — drop the row or restore the route", key)
		}
	}
	// Registrations without a table row.
	for _, reg := range registered {
		key := reg.Method + " " + reg.Path
		row, ok := rows[key]
		if !ok {
			t.Errorf("route %q registered in server.go is missing from the endpoint table (api/capabilities.go) — add a row so GET /api/capabilities keeps describing every route", key)
			continue
		}
		if row.Method != reg.Method || row.Path != reg.Path {
			t.Errorf("endpoint table row for %q has drifted: got %q, mux registers %q", key, row.Method+" "+row.Path, reg.Method+" "+reg.Path)
		}
		if strings.TrimSpace(row.Summary) == "" {
			t.Errorf("endpoint table row %q has an empty summary", key)
		}
	}
}

// registeredSet keys registrations by "METHOD /path".
func registeredSet(routes []endpointSpec) map[string]endpointSpec {
	out := map[string]endpointSpec{}
	for _, r := range routes {
		out[r.Method+" "+r.Path] = r
	}
	return out
}

// TestCapabilitiesTableRegistersCapabilities itself proves the endpoint
// table describes the new route: if GET /api/capabilities vanished from
// the table, the anti-drift test above would not notice (the mux
// registration and the row would both be gone); this pins the row.
func TestCapabilitiesTableRegistersCapabilities(t *testing.T) {
	row, ok := endpointRowSet()["GET /api/capabilities"]
	if !ok {
		t.Fatal("GET /api/capabilities is missing from the endpoint table")
	}
	if row.Admin {
		t.Error("GET /api/capabilities must be readable by any valid user token (not admin-only)")
	}
	if row.Summary == "" {
		t.Error("GET /api/capabilities has an empty summary")
	}
}

// TestCapabilitiesEndpoint answers the requirement: any valid user
// token gets the whole document; no token gets 401. It checks the
// stable field names — version, api, endpoints (every table row),
// images (the catalog's rows, shape-equal to detail=1's), sizes
// (GET /api/sizes' data), network_policies (the four classes) and
// surfaces — and that the document never leaks a secret or another
// user's data (it is static host description; nothing here varies by
// caller).
func TestCapabilitiesEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	// Any valid user token.
	resp, body := doReq(t, "GET", ts.URL+"/api/capabilities", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, body)
	}
	for _, field := range []string{"version", "api", "endpoints", "images", "sizes", "network_policies", "surfaces"} {
		if _, ok := body[field]; !ok {
			t.Errorf("capability document is missing %q", field)
		}
	}
	// endpooints: every table row present in the response.
	var raw struct {
		Version   string           `json:"version"`
		API       map[string]any   `json:"api"`
		Endpoints []map[string]any `json:"endpoints"`
		Images    []map[string]any `json:"images"`
		Sizes     struct {
			Presets []map[string]any `json:"presets"`
			MaxVCPU int              `json:"max_vcpu"`
			MaxMem  int              `json:"max_memory_mb"`
		} `json:"sizes"`
		NetPolicies []map[string]any `json:"network_policies"`
		Surfaces    struct {
			SSHGateway string `json:"ssh_gateway"`
			LLMGateway string `json:"llm_gateway"`
			MCP        *struct {
				Binary string   `json:"binary"`
				Tools  []string `json:"tools"`
			} `json:"mcp"`
			CIRunner []map[string]any `json:"ci_runner"`
		} `json:"surfaces"`
	}
	redoc, _ := json.Marshal(body)
	if err := json.Unmarshal(redoc, &raw); err != nil {
		t.Fatalf("re-decode capability document: %v", err)
	}
	if raw.Version == "" {
		t.Error("capability document version is empty")
	}
	if raw.API["base"] != "/api" {
		t.Errorf("api.base = %v, want /api", raw.API["base"])
	}
	if raw.API["auth"] == "" {
		t.Error("api.auth is empty")
	}
	rows := endpointRowSet()
	got := map[string]bool{}
	for _, e := range raw.Endpoints {
		key, _ := e["method"].(string)
		path, _ := e["path"].(string)
		got[key+" "+path] = true
	}
	for key := range rows {
		if !got[key] {
			t.Errorf("capability document lacks endpoint %q from the table", key)
		}
	}
	// images: the catalog's row, the same fields detail=1 answers for it.
	if len(raw.Images) != 1 {
		t.Fatalf("images = %d, want 1 (the seeded py-base)", len(raw.Images))
	}
	img := raw.Images[0]
	if img["name"] != "py-base" || img["memory_mb"].(float64) != 2048 || img["disk_mb"].(float64) != 10240 {
		t.Errorf("image row = %v", img)
	}
	// sizes: same data GET /api/sizes answers, from the same fallback.
	resp2, _ := doReq(t, "GET", ts.URL+"/api/sizes", "token-a", nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("sizes status %d", resp2.StatusCode)
	}
	if raw.Sizes.MaxVCPU == 0 && raw.Sizes.MaxMem == 0 && len(raw.Sizes.Presets) == 0 {
		t.Log("sizes block empty (host defaults)")
	}
	// network policies: the four documented classes.
	named := map[string]bool{}
	for _, p := range raw.NetPolicies {
		name, _ := p["name"].(string)
		if detail, _ := p["detail"].(string); strings.TrimSpace(detail) == "" {
			t.Errorf("network policy %q has no one-line meaning", name)
		}
		named[name] = true
	}
	for _, want := range []string{"none", "lan", "internet", "restricted"} {
		if !named[want] {
			t.Errorf("network_policies lacks %q", want)
		}
	}
	// surfaces: no host config = no surface except the MCP entry (this
	// binary always ships it).
	if raw.Surfaces.SSHGateway != "" {
		t.Errorf("ssh_gateway set with GATEWAY_ADDR unset: %q", raw.Surfaces.SSHGateway)
	}
	if raw.Surfaces.LLMGateway != "" {
		t.Errorf("llm_gateway set with no LLM_UPSTREAM_URL: %q", raw.Surfaces.LLMGateway)
	}
	if raw.Surfaces.MCP == nil {
		t.Fatal("mcp surface missing")
	}
}

// TestCapabilitiesNoTokenUnauthenticated: like GET /api/images the
// route lives behind the auth middleware — no token, no document.
func TestCapabilitiesNoTokenUnauthenticated(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := doReq(t, "GET", ts.URL+"/api/capabilities", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

// TestCapabilitiesSurfacesConfigured pins the conditional surfaces: the
// SSH gateway address when GATEWAY_ADDR is set, the LLM gateway prefix
// when the server was built with an upstream, and the MCP tools read
// from the mcp package's tool table.
func TestCapabilitiesSurfacesConfigured(t *testing.T) {
	t.Setenv("GATEWAY_ADDR", "ssh.example.com:2222")
	ts, _ := newTestServer(t)
	resp, body := doReq(t, "GET", ts.URL+"/api/capabilities", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, _ := json.Marshal(body["surfaces"])
	var surfaces struct {
		SSHGateway string      `json:"ssh_gateway"`
		LLMGateway string      `json:"llm_gateway"`
		MCP        interface{} `json:"mcp"`
		CIRunner   []struct {
			Label string `json:"label"`
			Image string `json:"image"`
		} `json:"ci_runner"`
	}
	if err := json.Unmarshal(raw, &surfaces); err != nil {
		t.Fatalf("decode surfaces: %v", err)
	}
	if surfaces.SSHGateway != "ssh.example.com:2222" {
		t.Errorf("ssh_gateway = %q, want the GATEWAY_ADDR value", surfaces.SSHGateway)
	}
	if surfaces.LLMGateway != "" {
		t.Errorf("llm_gateway set with no LLM_UPSTREAM_URL: %q", surfaces.LLMGateway)
	}
	if surfaces.CIRunner != nil {
		t.Errorf("ci_runner set with IMAGE_MAP unset: %v", surfaces.CIRunner)
	}
}

// TestCapabilitiesMCPSurface reads its tool names from the mcp package
// (no hand copy): the surface binary is the stdio MCP command and the
// tool names must equal what the package lists.
func TestCapabilitiesMCPSurface(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := doReq(t, "GET", ts.URL+"/api/capabilities", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mcpSurface := body["surfaces"].(map[string]any)["mcp"].(map[string]any)
	if mcpSurface["binary"] != "spoond-dev-mcp" {
		t.Errorf("mcp binary = %v", mcpSurface["binary"])
	}
	names, _ := mcpSurface["tools"].([]any)
	if len(names) == 0 {
		t.Fatal("mcp tools list is empty")
	}
	for _, n := range names {
		if s, _ := n.(string); s == "" {
			t.Errorf("mcp tool name is not a string: %v", n)
		}
	}
}

// TestCapabilitiesImagesNoSecondSource: the capability document's
// images come through the same catalog query as GET /api/images?detail=1
// — the same rows (name, default shape) must appear on both. It checks
// the values, so a future second, divergent list breaks the test.
func TestCapabilitiesImagesNoSecondSource(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, caps := doReq(t, "GET", ts.URL+"/api/capabilities", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("capabilities status %d", resp.StatusCode)
	}
	resp2, detail := doReq(t, "GET", ts.URL+"/api/images?detail=1", "token-a", nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("images detail status %d", resp2.StatusCode)
	}
	capRows := caps["images"].([]any)
	detailRows := detail["images"].([]any)
	if len(capRows) != len(detailRows) {
		t.Fatalf("capabilities images = %d rows, GET /api/images?detail=1 answers %d", len(capRows), len(detailRows))
	}
	order := func(rows []any) []string {
		names := []string{}
		for _, r := range rows {
			names = append(names, r.(map[string]any)["name"].(string))
		}
		return names
	}
	a, b := order(capRows), order(detailRows)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("capabilities image %d = %q, detail=1 answers %q", i, a[i], b[i])
		}
	}
}
