// The spoond phase's item 11: the backend's /healthz wait. GETs go
// through a small interface faked in tests; the production fetcher
// verifies the certificate the phase wrote itself (cert.pem) with
// ServerName = host name, so a name-mismatch failure is impossible.
//
// The health item is skipped in a dry run (no server is written, so
// nothing can answer) — this file's functions are called only when
// DryRun is off.
package install

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// fetcher is the HTTP GET and POST the health wait and the bootstrap
// POST make, faked in tests (the task's "HTTP through small
// interfaces"). The production fetcher is a real client over a
// transport that verifies cert.pem.
type fetcher interface {
	// Get issues the request and returns the status code.
	Get(r *http.Request) (int, error)
	// Post issues it likewise.
	Post(r *http.Request) (int, error)
}

// HealthFetcher is the fetcher hook: nil in production (the client
// below), set by tests to a fake. It stands in for the host, like the
// check phase's Prober and Spacer, and is read by the spoond phase's
// health wait and bootstrap POST.
var HealthFetcher fetcher

// clientDoer is the production fetcher over an http.Client.
type clientDoer struct{ c *http.Client }

func (d clientDoer) do(r *http.Request) (int, error) {
	resp, err := d.c.Do(r)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Get and Post are the same round trip for either method; the method
// split keeps call sites readable.
func (d clientDoer) Get(r *http.Request) (int, error)  { return d.do(r) }
func (d clientDoer) Post(r *http.Request) (int, error) { return d.do(r) }

// healthFetcher is the Env's fetcher: the HealthFetcher hook when set,
// else the real client (RootCA = cert.pem, ServerName = host name) —
// no InsecureSkipVerify: the certificate the phase wrote is the one
// verified.
func (env *Env) healthFetcher(dd hostDefaults) fetcher {
	if HealthFetcher != nil {
		return HealthFetcher
	}
	pool, err := readCertPool(env.tlsCertFull())
	if err != nil {
		// The certificate was just written or passed with
		// --tls-cert; an unreadable one is phase-fatal further up.
		// A pool-less client would fail every verify, so report it
		// the same way: the fetch fails on every try.
		return failingFetcher{err: err}
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		ServerName: dd.hostName,
		MinVersion: tls.VersionTLS12,
	}}
	return clientDoer{c: &http.Client{Timeout: 5 * time.Second, Transport: tr}}
}

// failingFetcher is the fetcher when cert.pem cannot be read: every
// try fails with the pool error.
type failingFetcher struct{ err error }

func (f failingFetcher) Get(r *http.Request) (int, error)  { return 0, f.err }
func (f failingFetcher) Post(r *http.Request) (int, error) { return 0, f.err }

// healthPoll is the pause between two /healthz tries.
const healthPoll = time.Second

// waitBackendHealthy gets /healthz for up to healthWait and adds the
// row: kept when healthy, failed with the journalctl hint when not. The
// hint names where the unit's log is (item 11), never a token.
func waitBackendHealthy(ctx context.Context, env *Env, r *Reporter, dd hostDefaults) error {
	fetch := env.healthFetcher(dd)
	deadline := time.Now().Add(healthWait)
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthzURL(), nil)
		if err != nil {
			return fmt.Errorf("building the health request: %w", err)
		}
		code, err := fetch.Get(req)
		switch {
		case err == nil && code == http.StatusOK:
			r.Add(Kept, healthzURL(), "spoond-backend is healthy")
			return nil
		case time.Now().After(deadline):
			if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("status %d", code)
			}
			note := "spoond-backend is not healthy: journalctl -u spoond-backend -n 50 (last: " + lastErr.Error() + ")"
			r.Add(Failed, healthzURL(), note)
			return fmt.Errorf("%s", note)
		}
		lastErr = fmt.Errorf("status %d", code)
		select {
		case <-ctx.Done():
			return fmt.Errorf("spoond-backend is not healthy: journalctl -u spoond-backend -n 50 (cancelled)")
		case <-time.After(healthPoll):
		}
	}
}

// healthzURL is the backend's health endpoint.
func healthzURL() string { return backendURL + "/healthz" }

// readCertPool parses cert.pem (the phase's own output or the
// operator's --tls-cert) into a RootCA pool. A failure names the file.
func readCertPool(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(pemBytes); !ok {
		return nil, fmt.Errorf("%s: no certificate found", path)
	}
	return pool, nil
}
