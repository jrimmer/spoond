package e2b

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Per-call RPC bounds. Every orchestrator gRPC call a background loop
// can make runs under one of these, so a hung RPC frees the loop, the
// lease's busy flag and the snapshot limiter instead of wedging them
// for the process's lifetime (spoond-j3a). Create includes a resume
// (both are SandboxService.Create), and Pause covers checkpoint-style
// snapshot writes. A zero field takes the default; see withDefaults.
const (
	DefaultCreateTimeout     = 5 * time.Minute
	DefaultPauseTimeout      = 5 * time.Minute
	DefaultCheckpointTimeout = 5 * time.Minute
	DefaultDeleteTimeout     = 2 * time.Minute
	DefaultNodeInfoTimeout   = 15 * time.Second
	DefaultControlTimeout    = 30 * time.Second
)

// Config is the E2B substrate connection configuration.
type Config struct {
	GRPCAddr  string
	ProxyURL  string
	TeamID    string
	TokenSeed []byte

	// Timeouts bound each orchestrator gRPC call when it is not already
	// bounded by the caller's context. A zero value uses the matching
	// Default* constant; a negative value is also treated as the
	// default. Exec keeps its own per-request timeout.
	CreateTimeout     time.Duration // SandboxService.Create (create and resume)
	PauseTimeout      time.Duration // SandboxService.Pause
	CheckpointTimeout time.Duration // SandboxService.Checkpoint
	DeleteTimeout     time.Duration // SandboxService.Delete
	NodeInfoTimeout   time.Duration // InfoService.ServiceInfo
	ControlTimeout    time.Duration // everything else: List, Update, SetDraining, builds, envd control
	// EnvdVersion and FirecrackerVersion are the host's envd and
	// firecracker versions, when the operator sets E2B_ENVD_VERSION /
	// E2B_FIRECRACKER_VERSION. They gate a named-snapshot start: a saved
	// version built against a different envd or firecracker cannot run
	// on this host (2.7, #83 S2). Empty disables that half of the check.
	EnvdVersion        string
	FirecrackerVersion string
}

// FromEnv reads the E2B configuration from the environment, applying the
// defaults below for variables that are unset or empty.
func FromEnv() (Config, error) {
	cfg := Config{
		GRPCAddr:           envOr("E2B_GRPC_ADDR", "127.0.0.1:5008"),
		ProxyURL:           envOr("E2B_PROXY_URL", "http://127.0.0.1:5007"),
		TeamID:             envOr("E2B_TEAM_ID", "5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95"),
		EnvdVersion:        os.Getenv("E2B_ENVD_VERSION"),
		FirecrackerVersion: os.Getenv("E2B_FIRECRACKER_VERSION"),
		CreateTimeout:      envDuration("E2B_CREATE_TIMEOUT", DefaultCreateTimeout),
		PauseTimeout:       envDuration("E2B_PAUSE_TIMEOUT", DefaultPauseTimeout),
		CheckpointTimeout:  envDuration("E2B_CHECKPOINT_TIMEOUT", DefaultCheckpointTimeout),
		DeleteTimeout:      envDuration("E2B_DELETE_TIMEOUT", DefaultDeleteTimeout),
		NodeInfoTimeout:    envDuration("E2B_NODEINFO_TIMEOUT", DefaultNodeInfoTimeout),
		ControlTimeout:     envDuration("E2B_CONTROL_TIMEOUT", DefaultControlTimeout),
	}
	seedFile := envOr("E2B_TOKEN_SEED_FILE", "/etc/spoond/e2b-token-seed")
	b, err := os.ReadFile(seedFile)
	if err != nil {
		return Config{}, fmt.Errorf("e2b: read token seed: %w", err)
	}
	seed := strings.TrimSpace(string(b))
	if len(seed) < 32 {
		return Config{}, fmt.Errorf("e2b: token seed %s: shorter than 32 bytes", seedFile)
	}
	cfg.TokenSeed = []byte(seed)
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDuration reads a Go duration ("5m") or a plain integer number of
// seconds from the environment. An unset, unparseable or non-positive
// value returns def.
func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return def
		}
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// withDefaults fills every zero (or negative) per-call timeout with its
// default, so callers that build a Config by hand still get bounded
// RPCs.
func (c Config) withDefaults() Config {
	if c.CreateTimeout <= 0 {
		c.CreateTimeout = DefaultCreateTimeout
	}
	if c.PauseTimeout <= 0 {
		c.PauseTimeout = DefaultPauseTimeout
	}
	if c.CheckpointTimeout <= 0 {
		c.CheckpointTimeout = DefaultCheckpointTimeout
	}
	if c.DeleteTimeout <= 0 {
		c.DeleteTimeout = DefaultDeleteTimeout
	}
	if c.NodeInfoTimeout <= 0 {
		c.NodeInfoTimeout = DefaultNodeInfoTimeout
	}
	if c.ControlTimeout <= 0 {
		c.ControlTimeout = DefaultControlTimeout
	}
	return c
}
