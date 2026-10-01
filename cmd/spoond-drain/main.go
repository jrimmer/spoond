// Command spoond-drain implements the `spoond drain` subcommand (U10):
// the e2b-orchestrator unit's ExecStop/ExecStartPost hook. `--stop`
// drains the spoond backend (pausing every running sandbox) before the
// orchestrator restarts; `--start` undrains it afterwards, resuming the
// drained leases.
//
// Configuration comes from the environment (vm2: /etc/e2b/drain.env):
//
//	SPOOND_DRAIN_URL          backend base URL(s), comma-separated
//	SPOOND_ADMIN_TOKEN_FILE   file(s) holding the ADMIN_TOKEN bearer
//	                          token, a list of the same length
//	SPOOND_DRAIN_INSECURE     1 skips TLS verification (the certificate
//	                          is for vm2.lacy.casa; the call goes to
//	                          127.0.0.1)
//
// Each URL/token pair is called in list order; a failure on one pair is
// logged and the next pair still runs. The command exits 0 even when a
// call fails: a failed drain must never block the stop.
package spoondrain

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// The call timeouts leave room for the drain's own internal waits
// (pausing every sandbox, then waiting for the node to quiesce) and for
// undrain's wait for the node to come back.
const (
	stopTimeout  = 240 * time.Second
	startTimeout = 300 * time.Second
)

// target is one resolved URL/token pair.
type target struct {
	url   string
	token string
}

// Main is the `spoond drain` entry point.
func Main(args []string) int {
	mode := ""
	for _, a := range args {
		switch a {
		case "--stop":
			mode = "stop"
		case "--start":
			mode = "start"
		default:
			fmt.Fprintf(os.Stderr, "drain: unknown argument %q; usage: spoond drain --stop|--start\n", a)
			return 2
		}
	}
	if mode == "" {
		fmt.Fprintln(os.Stderr, "drain: usage: spoond drain --stop|--start")
		return 2
	}

	// systemd runs ExecStop even after the orchestrator crashed or was
	// killed; a drain then must not run — there is nothing left to
	// pause, and the resumes happen on the next start.
	if mode == "stop" {
		if sr := os.Getenv("SERVICE_RESULT"); sr != "" && sr != "success" {
			fmt.Printf("drain: skipped (SERVICE_RESULT=%s)\n", sr)
			return 0
		}
	}

	urls := splitList(os.Getenv("SPOOND_DRAIN_URL"))
	tokenFiles := splitList(os.Getenv("SPOOND_ADMIN_TOKEN_FILE"))
	if len(urls) != len(tokenFiles) {
		log.Printf("drain: SPOOND_DRAIN_URL has %d entries but SPOOND_ADMIN_TOKEN_FILE has %d; same length required",
			len(urls), len(tokenFiles))
		return 0
	}

	path, timeout := "/api/admin/drain", stopTimeout
	if mode == "start" {
		path, timeout = "/api/admin/undrain", startTimeout
	}

	client := newHTTPClient()
	for i, u := range urls {
		raw, err := os.ReadFile(tokenFiles[i])
		if err != nil {
			log.Printf("drain: %s %s: read token file %s: %v", mode, u, tokenFiles[i], err)
			continue
		}
		tgt := target{url: u, token: strings.TrimSpace(string(raw))}
		if err := postAdmin(client, tgt, path, timeout); err != nil {
			log.Printf("drain: %s %s: %v", mode, u, err)
		}
	}
	return 0
}

// postAdmin POSTs one admin endpoint with the target's bearer token and
// prints the response JSON.
func postAdmin(client *http.Client, tgt target, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(tgt.url, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tgt.token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Println(strings.TrimSpace(string(body)))
	return nil
}

// newHTTPClient builds the HTTP client. SPOOND_DRAIN_INSECURE=1 skips
// TLS verification: the certificate is for vm2.lacy.casa while the call
// goes to 127.0.0.1.
func newHTTPClient() *http.Client {
	tr := &http.Transport{}
	if os.Getenv("SPOOND_DRAIN_INSECURE") == "1" {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // vm2 cert vs 127.0.0.1 loopback call
	}
	return &http.Client{Transport: tr}
}

// splitList splits a comma-separated env value into trimmed, non-empty
// entries.
func splitList(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}
