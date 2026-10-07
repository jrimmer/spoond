// offline.go is the Env the CLI runs the checks with: image lookup goes
// to the lease API for real, and every step that has to run on the host
// (building the worker image, cloning with the deploy key, the trial
// lease, the gates) or against the owner (the budget) reports Skip,
// because a laptop is not the host. The server's POST /hive/check
// (step 3) swaps in the real thing.
package hive

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

// DefaultAPI is the lease API the checks talk to when SPOOND_API is
// unset.
const DefaultAPI = "https://spoond.example.com:8890"

// SkipError is returned by an Env for a step it cannot run where it
// runs. The check reports Skip with the error's reason instead of Fail,
// so a report says "not attempted here" rather than "broken".
type SkipError struct {
	Reason string
}

// Error implements error.
func (e *SkipError) Error() string { return e.Reason }

// Skipf returns a SkipError with a formatted reason.
func Skipf(format string, args ...any) *SkipError {
	return &SkipError{Reason: fmt.Sprintf(format, args...)}
}

// hostReason is what an OfflineEnv says for a step that runs on the
// host: the CLI cannot do it, the check API can.
const hostReason = "runs on the host: POST /hive/check"

// OfflineEnv looks the image catalog up against the lease API, serves
// the instance facts it was built with, and skips everything that runs
// on the host.
type OfflineEnv struct {
	// API is the lease API base URL.
	API string
	// Token is the consumer bearer token.
	Token string
	// Inst holds the instance facts.
	Inst Instance

	client *http.Client
}

// NewOfflineEnv builds an OfflineEnv for the given API, token and
// instance facts. Tests pass an httptest server's URL as the API.
func NewOfflineEnv(api, token string, inst Instance) *OfflineEnv {
	return &OfflineEnv{API: api, Token: token, Inst: inst, client: offlineClient("")}
}

// EnvFromEnv builds the OfflineEnv the CLI runs with: the API base and
// token from SPOOND_API and SPOOND_TOKEN, and the instance facts from
// InstanceFromEnv.
func EnvFromEnv() *OfflineEnv {
	api := envOr("SPOOND_API", DefaultAPI)
	inst, err := InstanceFromEnv(api)
	if err != nil {
		inst = Instance{}
	}
	return &OfflineEnv{
		API:    api,
		Token:  os.Getenv("SPOOND_TOKEN"),
		Inst:   inst,
		client: offlineClient(os.Getenv("SPOOND_API_CA")),
	}
}

// InstanceFromEnv derives the instance facts from the API base URL and
// the environment: the lease API is the base URL's host:port, the
// registry the same host on :5000 unless SPOOND_REGISTRY is set, and
// the model service and Agent Mail hosts from SPOOND_MODEL_SERVICE
// (default llm.example.com) and SPOOND_AGENT_MAIL (default
// mail.example.com). The guide (C11) serves the real values; these
// defaults hold until then.
func InstanceFromEnv(api string) (Instance, error) {
	u, err := url.Parse(api)
	if err != nil || u.Host == "" {
		return Instance{}, fmt.Errorf("parse API %q: no usable URL", api)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = u.Hostname() + ":443"
		} else {
			host = u.Hostname() + ":80"
		}
	}
	return Instance{
		LeaseAPI:     host,
		Registry:     envOr("SPOOND_REGISTRY", u.Hostname()+":5000"),
		ModelService: envOr("SPOOND_MODEL_SERVICE", "llm.example.com"),
		AgentMail:    envOr("SPOOND_AGENT_MAIL", "mail.example.com"),
	}, nil
}

// offlineClient builds the HTTP client for the lease API. caFile, when
// set, is a PEM bundle the instance's cert chains to (a LAN CA); TLS
// verification is never skipped.
func offlineClient(caFile string) *http.Client {
	c := &http.Client{Timeout: 30 * time.Second}
	if caFile == "" {
		return c
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return c
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return c
	}
	c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	return c
}

// Facts returns the instance facts the derivations use.
func (e *OfflineEnv) Facts(ctx context.Context) (Instance, error) {
	return e.Inst, nil
}

// ListImages returns the image catalog's names from the lease API,
// fetched with the bearer token.
func (e *OfflineEnv) ListImages(ctx context.Context) ([]string, error) {
	if e.Token == "" {
		return nil, errors.New("SPOOND_TOKEN is not set")
	}
	var out struct {
		Images []string `json:"images"`
	}
	if err := e.get(ctx, "/api/images", &out); err != nil {
		return nil, err
	}
	return out.Images, nil
}

// BuildImage runs on the host: the CLI cannot build the worker image.
func (e *OfflineEnv) BuildImage(ctx context.Context, image string) error {
	return Skipf("%s", hostReason)
}

// PushScratch runs on the host: the CLI holds no deploy key.
func (e *OfflineEnv) PushScratch(ctx context.Context, repo string) error {
	return Skipf("%s", hostReason)
}

// Reachable runs on the host: only the host mints a trial lease.
func (e *OfflineEnv) Reachable(ctx context.Context, allowlist, needs []string) (string, error) {
	return "", Skipf("%s", hostReason)
}

// RunGate runs on the host: gates run in a worker, not on a laptop.
func (e *OfflineEnv) RunGate(ctx context.Context, repo, gate string) error {
	return Skipf("%s", hostReason)
}

// Budget is the owner's to set; the CLI cannot read it.
func (e *OfflineEnv) Budget(ctx context.Context, project string) (Budget, error) {
	return Budget{}, Skipf("%s", hostReason)
}

// get fetches path from the lease API with the bearer token and decodes
// its JSON body into out.
func (e *OfflineEnv) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.API+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	req.Header.Set("Accept", "application/json")
	if e.client == nil {
		e.client = offlineClient("")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("parse GET %s: %w", path, err)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
