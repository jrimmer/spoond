// Command spoond capabilities reads GET /api/capabilities from the
// backend and prints a readable summary of what this spoond offers
// (#52):
//
//	spoond capabilities           human-readable summary
//	spoond capabilities --json    the raw document
//
// The backend URL comes from SPOOND_URL (default
// https://127.0.0.1:8890, scheme http when TLS_CERT is unset — the same
// defaults `spoond doctor` probes with) and the token from TOKEN or
// CONSUMER_TOKENS (the first token= side, so an operator's usual env
// already carries a working credential; one-off overrides go through
// --token). TLS trusts the backend's own cert file from TLS_CERT, like
// `spoond doctor` — never an insecure skip.
//
// The summary prints the version, the api auth model, then per block:
// images (name — capability: description; default shape), size presets
// and limits, the network policies, the endpoint count plus every
// endpoint line, and the configured surfaces (SSH gateway address, the
// LLM gateway prefix, the MCP binary and its tools, the CI runner's
// label→image table).
package spoondcapabilities

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// doc is the capability document's shape (api/capabilities.go); field
// names are stable (#52).
type doc struct {
	Version string `json:"version"`
	API     struct {
		Base    string `json:"base"`
		Primary string `json:"primary"`
		Alias   string `json:"alias"`
		Auth    string `json:"auth"`
	} `json:"api"`
	Endpoints []struct {
		Method  string `json:"method"`
		Path    string `json:"path"`
		Summary string `json:"summary"`
		Admin   bool   `json:"admin,omitempty"`
	} `json:"endpoints"`
	Images []struct {
		Name        string   `json:"name"`
		Capability  string   `json:"capability,omitempty"`
		Description string   `json:"description,omitempty"`
		VCPU        int      `json:"vcpu"`
		MemoryMB    int      `json:"memory_mb"`
		DiskMB      int      `json:"disk_mb"`
		MinMemoryMB int      `json:"min_memory_mb"`
		WarmSizes   []string `json:"warm_sizes"`
	} `json:"images"`
	Sizes struct {
		Presets []struct {
			Name     string `json:"name"`
			VCPU     int    `json:"vcpu"`
			MemoryMB int    `json:"memory_mb"`
		} `json:"presets"`
		MaxVCPU     int `json:"max_vcpu"`
		MaxMemoryMB int `json:"max_memory_mb"`
	} `json:"sizes"`
	NetworkPolicies []struct {
		Name   string `json:"name"`
		Detail string `json:"detail"`
	} `json:"network_policies"`
	Surfaces struct {
		SSHGateway string `json:"ssh_gateway,omitempty"`
		LLMGateway string `json:"llm_gateway,omitempty"`
		MCP        *struct {
			Binary string   `json:"binary"`
			Tools  []string `json:"tools"`
		} `json:"mcp,omitempty"`
		CIRunner []struct {
			Label string `json:"label"`
			Image string `json:"image"`
		} `json:"ci_runner,omitempty"`
	} `json:"surfaces"`
}

// Main runs `spoond capabilities [flags]`.
func Main(args []string) int {
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print the raw JSON document")
	tokenFlag := fs.String("token", "", "bearer token (default TOKEN, else the first CONSUMER_TOKENS token side)")
	urlFlag := fs.String("url", "", "backend base URL (default SPOOND_URL, else https://127.0.0.1:8890, or http with no TLS_CERT)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "spoond capabilities: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	baseURL := strings.TrimRight(*urlFlag, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(os.Getenv("SPOOND_URL"), "/")
	}
	if baseURL == "" {
		baseURL = defaultBaseURL()
	}
	token := strings.TrimSpace(*tokenFlag)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("TOKEN"))
	}
	if token == "" {
		token = firstConsumerToken()
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "spoond capabilities: no token — set TOKEN or CONSUMER_TOKENS (token=consumer), or pass --token")
		return 2
	}

	client := &http.Client{Timeout: 15 * time.Second}
	if cert := firstTLSCert(); cert != "" && strings.HasPrefix(baseURL, "https://") {
		roots, err := x509PoolFromCert(cert)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spoond capabilities: TLS trust from TLS_CERT: %v\n", err)
			return 1
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	}

	req, err := http.NewRequest("GET", baseURL+"/api/capabilities", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond capabilities: %v\n", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond capabilities: %s/api/capabilities: %v\n", baseURL, err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond capabilities: read: %v\n", err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "spoond capabilities: HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	var d doc
	if err := json.Unmarshal(body, &d); err != nil {
		fmt.Fprintf(os.Stderr, "spoond capabilities: decode: %v\n", err)
		return 1
	}
	if *jsonOut {
		out := map[string]any{}
		if err := json.Unmarshal(body, &out); err != nil {
			fmt.Fprintf(os.Stderr, "spoond capabilities: decode: %v\n", err)
			return 1
		}
		enc, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(enc))
		return 0
	}
	printSummary(os.Stdout, d)
	return 0
}

// defaultBaseURL is the backend URL when SPOOND_URL is unset: HTTPS on
// the default bind when TLS_CERT names a cert, plain HTTP otherwise —
// the same reflection `spoond doctor` probes with.
func defaultBaseURL() string {
	scheme := "http"
	if firstTLSCert() != "" {
		scheme = "https"
	}
	bind := strings.TrimSpace(os.Getenv("BIND_ADDR"))
	if bind == "" {
		bind = "127.0.0.1:8890"
	}
	if _, port, err := splitHostPort(bind); err == nil && port != "" {
		return scheme + "://" + bind
	}
	return scheme + "://" + bind
}

// splitHostPort is net.SplitHostPort testable (kept for symmetry with
// the doctor's probe host logic).
func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "", fmt.Errorf("missing port in %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}

// firstConsumerToken takes the token (left) side of the first
// token=consumer pair in CONSUMER_TOKENS.
func firstConsumerToken() string {
	raw := os.Getenv("CONSUMER_TOKENS")
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" {
			return strings.TrimSpace(parts[0])
		}
	}
	return ""
}

// firstTLSCert is the first cert file listed in TLS_CERT, or "" when
// TLS is off (the same reading the doctor does).
func firstTLSCert() string {
	v := strings.TrimSpace(os.Getenv("TLS_CERT"))
	if v == "" {
		return ""
	}
	cert := strings.Split(v, ",")[0]
	return strings.TrimSpace(cert)
}

// x509PoolFromCert builds a root pool from the backend's own cert file
// (PEM, possibly with the chain): trust the deployed backend, never
// skip verification.
func x509PoolFromCert(certFile string) (*x509.CertPool, error) {
	_, err := tls.LoadX509KeyPair(certFile, certFile)
	if err != nil {
		// A cert without its key in the same file is fine here: we only
		// need the public part. Load the PEM blocks directly.
		pemBytes, rerr := os.ReadFile(certFile)
		if rerr != nil {
			return nil, rerr
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("no PEM certificate in %s", certFile)
		}
		return pool, nil
	}
	cert, err2 := tls.LoadX509KeyPair(certFile, certFile)
	if err2 != nil {
		return nil, err2
	}
	leaf, err3 := x509.ParseCertificate(cert.Certificate[0])
	if err3 != nil {
		return nil, err3
	}
	// Trust exactly this leaf (self-signed deployments) and any chain
	// blocks in the file.
	pool := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(certFile)
	pool.AppendCertsFromPEM(pemBytes)
	_ = leaf
	return pool, nil
}

// printSummary renders the human-readable summary.
func printSummary(w io.Writer, d doc) {
	fmt.Fprintf(w, "spoond %s\n", d.Version)
	if d.API.Auth != "" {
		fmt.Fprintf(w, "api: %s (auth: %s)\n", d.API.Base, d.API.Auth)
	} else {
		fmt.Fprintf(w, "api: %s\n", d.API.Base)
	}
	if d.API.Primary != "" && d.API.Alias != "" {
		fmt.Fprintf(w, "lease paths: %s (alias: %s, identical behavior)\n", d.API.Primary, d.API.Alias)
	}
	if len(d.Images) > 0 {
		fmt.Fprintf(w, "\nimages:\n")
		for _, img := range d.Images {
			line := fmt.Sprintf("  %-16s vcpu=%d mem=%dMB disk=%dMB", img.Name, img.VCPU, img.MemoryMB, img.DiskMB)
			switch {
			case img.Capability != "" && img.Description != "":
				line += fmt.Sprintf("  %s: %s", img.Capability, img.Description)
			case img.Capability != "":
				line += "  " + img.Capability
			case img.Description != "":
				line += "  " + img.Description
			}
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	} else {
		fmt.Fprintf(w, "\nimages: none with a current build\n")
	}
	if len(d.Sizes.Presets) > 0 {
		fmt.Fprintf(w, "\nsizes:\n")
		for _, p := range d.Sizes.Presets {
			fmt.Fprintf(w, "  %-10s vcpu=%d mem=%dMB\n", p.Name, p.VCPU, p.MemoryMB)
		}
		fmt.Fprintf(w, "  limits: vcpu<=%d mem<=%dMB\n", d.Sizes.MaxVCPU, d.Sizes.MaxMemoryMB)
	}
	if len(d.NetworkPolicies) > 0 {
		fmt.Fprintf(w, "\nnetwork policies (egress):\n")
		for _, p := range d.NetworkPolicies {
			fmt.Fprintf(w, "  %-10s %s\n", p.Name, p.Detail)
		}
	}
	s := d.Surfaces
	{
		var lines []string
		if s.SSHGateway != "" {
			lines = append(lines, fmt.Sprintf("  ssh gateway: ssh <lease>@%s", s.SSHGateway))
		}
		if s.LLMGateway != "" {
			lines = append(lines, fmt.Sprintf("  llm gateway: %s<lease-id>/ (OpenAI-compatible)", s.LLMGateway))
		}
		if s.MCP != nil {
			lines = append(lines, fmt.Sprintf("  mcp: %s (stdio), tools: %s", s.MCP.Binary, strings.Join(s.MCP.Tools, ", ")))
		}
		if len(s.CIRunner) > 0 {
			parts := make([]string, 0, len(s.CIRunner))
			for _, l := range s.CIRunner {
				parts = append(parts, l.Label+"→"+l.Image)
			}
			lines = append(lines, fmt.Sprintf("  ci runner labels: %s", strings.Join(parts, ", ")))
		}
		if len(lines) > 0 {
			fmt.Fprintf(w, "\nother ways in:\n")
			for _, l := range lines {
				fmt.Fprintln(w, l)
			}
		}
	}
	if len(d.Endpoints) > 0 {
		admin := 0
		for _, e := range d.Endpoints {
			if e.Admin {
				admin++
			}
		}
		fmt.Fprintf(w, "\nendpoints (%d routes, %d admin-only):\n", len(d.Endpoints), admin)
		for _, e := range d.Endpoints {
			marker := ""
			if e.Admin {
				marker = " [admin]"
			}
			fmt.Fprintf(w, "  %-7s%-46s %s%s\n", e.Method, e.Path, e.Summary, marker)
		}
	}
}
