// Command spoond-doctor is a dependency/connectivity checker for a spoond
// deployment. It exercises every external surface the backend depends on —
// forkd-controller, the LLM gateway upstream, the lease API listener, the
// SSH gateway port, the warm pool, TLS material, and disk — and reports a
// PASS/FAIL/WARN table with a non-zero exit when anything is failing.
//
// It reads the same environment the backend uses (FORKD_URL, CONSUMER_TOKENS,
// BIND_ADDR, LLM_UPSTREAM_URL, LLM_UPSTREAM_KEY, TLS_CERT, TLS_KEY,
// KNOWN_IMAGES), so running `spoond doctor` on a host is a true reflection of
// the deployed backend config.
package spoonddoctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jrimmer/spoond/forkd"
)

type checkResult struct {
	name   string
	status string // PASS | FAIL | WARN
	detail string
}

func Main(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	_ = fs.Parse(args)

	results := runChecks()

	if *jsonOut {
		type out struct {
			Checks []checkResult `json:"checks"`
			Pass   int           `json:"pass"`
			Fail   int           `json:"fail"`
			Warn   int           `json:"warn"`
			Ok     bool          `json:"ok"`
		}
		o := out{Checks: results, Ok: true}
		for _, r := range results {
			switch r.status {
			case "PASS":
				o.Pass++
			case "FAIL":
				o.Fail++
				o.Ok = false
			case "WARN":
				o.Warn++
			}
		}
		b, _ := json.MarshalIndent(o, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, r := range results {
			fmt.Printf("%-5s %-28s %s\n", r.status, r.name, r.detail)
		}
		fail := 0
		for _, r := range results {
			if r.status == "FAIL" {
				fail++
			}
		}
		if fail > 0 {
			fmt.Printf("\n%d check(s) failing\n", fail)
		} else {
			fmt.Println("\nall checks passed")
		}
	}
	for _, r := range results {
		if r.status == "FAIL" {
			return 1
		}
	}
	return 0
}

func runChecks() []checkResult {
	var out []checkResult
	out = append(out, checkConfig()...)
	out = append(out, checkForkd()...)
	out = append(out, checkBackend()...)
	out = append(out, checkGatewayPort()...)
	out = append(out, checkLLM()...)
	out = append(out, checkPool()...)
	out = append(out, checkTLS()...)
	out = append(out, checkDisk()...)
	return out
}

// checkConfig validates that the backend's required env is present and sane.
func checkConfig() []checkResult {
	var out []checkResult

	tokens := strings.TrimSpace(os.Getenv("CONSUMER_TOKENS"))
	if tokens == "" {
		out = append(out, checkResult{"config: CONSUMER_TOKENS", "FAIL", "unset — backend refuses to start"})
	} else {
		n := len(strings.Split(tokens, ","))
		out = append(out, checkResult{"config: CONSUMER_TOKENS", "PASS", fmt.Sprintf("%d consumer(s)", n)})
	}

	forkdURL := strings.TrimSpace(os.Getenv("FORKD_URL"))
	if forkdURL == "" {
		out = append(out, checkResult{"config: FORKD_URL", "FAIL", "unset (default http://127.0.0.1:8889)"})
	} else {
		out = append(out, checkResult{"config: FORKD_URL", "PASS", forkdURL})
	}

	bind := strings.TrimSpace(os.Getenv("BIND_ADDR"))
	if bind == "" {
		bind = "127.0.0.1:8890"
		out = append(out, checkResult{"config: BIND_ADDR", "WARN", "unset — defaulting to " + bind})
	} else {
		out = append(out, checkResult{"config: BIND_ADDR", "PASS", bind})
	}
	return out
}

// checkForkd pings the forkd-controller and lists sandboxes, proving both
// connectivity and that the controller API actually answers.
func checkForkd() []checkResult {
	var out []checkResult
	forkdURL := strings.TrimSpace(os.Getenv("FORKD_URL"))
	if forkdURL == "" {
		forkdURL = "http://127.0.0.1:8889"
	}
	fc := forkd.NewClient(forkdURL, os.Getenv("FORKD_TOKEN"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := reachable(forkdURL); err != nil {
		out = append(out, checkResult{"forkd-controller: connect", "FAIL", fmt.Sprintf("%v", err)})
		return out
	}
	out = append(out, checkResult{"forkd-controller: connect", "PASS", forkdURL})

	boxes, err := fc.ListSandboxes(ctx)
	if err != nil {
		out = append(out, checkResult{"forkd-controller: list sandboxes", "FAIL", fmt.Sprintf("%v", err)})
	} else {
		out = append(out, checkResult{"forkd-controller: list sandboxes", "PASS", fmt.Sprintf("%d sandbox(es)", len(boxes))})
	}
	return out
}

// checkBackend verifies the lease API listener is up on BIND_ADDR. The
// healthz endpoint is auth-free, so this works with any consumer token.
func checkBackend() []checkResult {
	var out []checkResult
	bind := strings.TrimSpace(os.Getenv("BIND_ADDR"))
	if bind == "" {
		bind = "127.0.0.1:8890"
	}
	conn, err := net.DialTimeout("tcp", bind, 3*time.Second)
	if err != nil {
		out = append(out, checkResult{"lease API: listener", "FAIL", fmt.Sprintf("connect %s: %v", bind, err)})
		return out
	}
	conn.Close()
	out = append(out, checkResult{"lease API: listener", "PASS", bind})

	// Probe /healthz. For https, trust the local backend by loading its own
	// cert chain from disk — never skip verification (no curl -k). When the
	// bind address is a wildcard the cert SAN (hostname) won't match it, so
	// probe via a DNS SAN from the leaf cert itself (e.g. vm2.lacy.casa).
	probeHost := bind
	if h, p, err := net.SplitHostPort(bind); err == nil && (h == "" || h == "0.0.0.0" || h == "::") {
		if san := leafCertSAN(os.Getenv("TLS_CERT")); san != "" {
			probeHost = net.JoinHostPort(san, p)
		} else if hn, err := os.Hostname(); err == nil && hn != "" {
			probeHost = net.JoinHostPort(hn, p)
		}
	}
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}
	if cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY"); cert != "" && key != "" {
		scheme = "https"
		roots, err := x509PoolFromCert(cert)
		if err != nil {
			out = append(out, checkResult{"lease API: TLS trust", "FAIL", fmt.Sprintf("%v", err)})
			return out
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	}
	resp, err := client.Get(scheme + "://" + probeHost + "/healthz")
	if err != nil {
		out = append(out, checkResult{"lease API: /healthz", "WARN", fmt.Sprintf("%v", err)})
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"status":"ok"`) {
		out = append(out, checkResult{"lease API: /healthz", "PASS", "200 ok"})
	} else {
		out = append(out, checkResult{"lease API: /healthz", "FAIL", fmt.Sprintf("HTTP %d %s", resp.StatusCode, body)})
	}
	return out
}

// checkGatewayPort verifies the SSH gateway listener (default :2222) is up.
func checkGatewayPort() []checkResult {
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:2222"
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return []checkResult{{"ssh gateway: listener", "FAIL", fmt.Sprintf("connect %s: %v", addr, err)}}
	}
	conn.Close()
	return []checkResult{{"ssh gateway: listener", "PASS", addr}}
}

// checkLLM verifies the LLM gateway upstream: key presence, connectivity,
// and key validity against the OpenAI-compatible /models endpoint.
func checkLLM() []checkResult {
	var out []checkResult
	up := strings.TrimSpace(os.Getenv("LLM_UPSTREAM_URL"))
	key := strings.TrimSpace(os.Getenv("LLM_UPSTREAM_KEY"))
	if up == "" {
		out = append(out, checkResult{"llm gateway: upstream", "WARN", "LLM_UPSTREAM_URL unset — gateway disabled"})
		return out
	}
	if key == "" {
		out = append(out, checkResult{"llm gateway: key", "FAIL", "LLM_UPSTREAM_KEY unset with URL configured"})
	} else {
		out = append(out, checkResult{"llm gateway: key", "PASS", "present"})
	}
	if err := reachable(up); err != nil {
		out = append(out, checkResult{"llm gateway: upstream connect", "FAIL", fmt.Sprintf("%v", err)})
		return out
	}
	out = append(out, checkResult{"llm gateway: upstream connect", "PASS", up})

	// Probe /models with the key. 200 = valid key; 401/403 = reachable but
	// auth rejected; anything else = odd but reachable.
	req, _ := http.NewRequest("GET", strings.TrimRight(up, "/")+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		out = append(out, checkResult{"llm gateway: /models", "FAIL", fmt.Sprintf("%v", err)})
		return out
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		out = append(out, checkResult{"llm gateway: /models", "PASS", "200 — key accepted"})
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		out = append(out, checkResult{"llm gateway: /models", "FAIL", fmt.Sprintf("HTTP %d — key rejected", resp.StatusCode)})
	default:
		out = append(out, checkResult{"llm gateway: /models", "WARN", fmt.Sprintf("HTTP %d — reachable, unexpected status", resp.StatusCode)})
	}
	return out
}

// checkPool reports warm-pool health from the controller's sandbox list.
func checkPool() []checkResult {
	forkdURL := strings.TrimSpace(os.Getenv("FORKD_URL"))
	if forkdURL == "" {
		forkdURL = "http://127.0.0.1:8889"
	}
	fc := forkd.NewClient(forkdURL, os.Getenv("FORKD_TOKEN"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boxes, err := fc.ListSandboxes(ctx)
	if err != nil {
		return []checkResult{{"warm pool: sandbox count", "FAIL", fmt.Sprintf("%v", err)}}
	}
	if len(boxes) == 0 {
		return []checkResult{{"warm pool: sandbox count", "WARN", "0 sandboxes — pool cold (will pay cold-start)"}}
	}
	return []checkResult{{"warm pool: sandbox count", "PASS", fmt.Sprintf("%d sandbox(es)", len(boxes))}}
}

// checkTLS verifies TLS material is present and loadable when configured.
func checkTLS() []checkResult {
	cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY")
	if cert == "" && key == "" {
		return []checkResult{{"tls: cert/key", "WARN", "not configured — API will serve plain HTTP"}}
	}
	if cert == "" || key == "" {
		return []checkResult{{"tls: cert/key", "FAIL", "only one of TLS_CERT/TLS_KEY set"}}
	}
	if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
		return []checkResult{{"tls: cert/key", "FAIL", fmt.Sprintf("%v", err)}}
	}
	return []checkResult{{"tls: cert/key", "PASS", "loadable"}}
}

// checkDisk reports the root filesystem fill level.
func checkDisk() []checkResult {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return []checkResult{{"disk: root", "WARN", fmt.Sprintf("statfs: %v", err)}}
	}
	if st.Blocks == 0 {
		return []checkResult{{"disk: root", "WARN", "statfs: no block info"}}
	}
	pct := float64(st.Blocks-st.Bavail) / float64(st.Blocks) * 100
	if pct > 90 {
		return []checkResult{{"disk: root", "FAIL", fmt.Sprintf("%.0f%% full", pct)}}
	}
	if pct > 75 {
		return []checkResult{{"disk: root", "WARN", fmt.Sprintf("%.0f%% full", pct)}}
	}
	return []checkResult{{"disk: root", "PASS", fmt.Sprintf("%.0f%% used", pct)}}
}

// reachable reports whether a base URL's host:port accepts TCP connections.
// The URL may carry a path (e.g. https://ollama.com/v1) — only the host is
// dialed, with the scheme's default port.
func reachable(baseURL string) error {
	u := baseURL
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return err
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// leafCertSAN returns the first DNS SAN of the leaf cert in certPath, or ""
// when the file is missing/unparseable. Used to pick a probe hostname that
// the backend's own cert will validate against.
func leafCertSAN(certPath string) string {
	if certPath == "" {
		return ""
	}
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	return ""
}

// x509PoolFromCert builds a root pool containing the given PEM cert file,
// so a local TLS probe can trust the backend's own cert without skipping
// verification.
func x509PoolFromCert(certPath string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates parsed from %s", certPath)
	}
	return pool, nil
}
