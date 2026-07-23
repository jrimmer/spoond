package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// RunnerVersion is the version string advertised to Forgejo.
const RunnerVersion = "hyper-forgejo-runner/0.1.0"

// RunnerCredentials is the persisted state after a successful Register call.
// It is written to the .runner file and loaded on subsequent starts.
type RunnerCredentials struct {
	UUID   string `json:"uuid"`
	Token  string `json:"token"`
	Name   string `json:"name"`
	ID     int64  `json:"id"`
	Labels []string `json:"labels"`
}

// runnerFilePath returns the path to the .runner credentials file.
// It defaults to ./.runner but can be overridden with RUNNER_CRED_FILE.
func runnerFilePath() string {
	if p := os.Getenv("RUNNER_CRED_FILE"); p != "" {
		return p
	}
	return ".runner"
}

// SaveCredentials writes runner credentials to the .runner file.
func SaveCredentials(creds *RunnerCredentials) error {
	path := runnerFilePath()
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	log.Printf("runner credentials saved to %s (uuid=%s, name=%s)", path, creds.UUID, creds.Name)
	return nil
}

// LoadCredentials reads runner credentials from the .runner file.
// Returns an error if the file does not exist (runner not registered yet).
func LoadCredentials() (*RunnerCredentials, error) {
	path := runnerFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("runner not registered: %s not found (run with --register first)", path)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var creds RunnerCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &creds, nil
}

// Register performs one-time runner registration with Forgejo.
// It calls the Register RPC with the registration token, persists the
// returned credentials, and updates the daemon's client with the new auth token.
func (d *Daemon) Register() error {
	// The FORGEJO_TOKEN env var holds the *registration* token during
	// the initial register flow. It is different from the runner auth
	// token returned by the Register RPC.
	registrationToken := d.cfg.ForgejoToken

	name := os.Getenv("RUNNER_NAME")
	if name == "" {
		name = "hyper-runner"
	}

	fc := NewForgejoClient(d.cfg.ForgejoURL, registrationToken)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("registering runner %q with %s (labels: %v)", name, d.cfg.ForgejoURL, d.cfg.Labels)

	resp, err := fc.Register(ctx, name, registrationToken, d.cfg.Labels, RunnerVersion)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	if resp.Runner == nil {
		return fmt.Errorf("register: server returned nil runner")
	}

	creds := &RunnerCredentials{
		UUID:   resp.Runner.Uuid,
		Token:  resp.Runner.Token,
		Name:   resp.Runner.Name,
		ID:     resp.Runner.Id,
		Labels: resp.Runner.Labels,
	}

	if err := SaveCredentials(creds); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}

	log.Printf("runner registered successfully (id=%d, uuid=%s)", creds.ID, creds.UUID)
	return nil
}

// loadOrRegister loads existing credentials or returns an error indicating
// the runner needs to be registered first.
func (d *Daemon) loadOrRegister() (*RunnerCredentials, error) {
	creds, err := LoadCredentials()
	if err != nil {
		return nil, fmt.Errorf("load credentials: %w (run 'hyper-runner --register' first)", err)
	}
	if creds.Token == "" {
		return nil, fmt.Errorf("credentials file has empty token, re-register")
	}
	return creds, nil
}

// absRunnerPath is used in error messages and logging.
func absRunnerPath() string {
	p := runnerFilePath()
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}