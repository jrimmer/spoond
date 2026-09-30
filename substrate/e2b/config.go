package e2b

import (
	"fmt"
	"os"
	"strings"
)

// Config is the E2B substrate connection configuration.
type Config struct {
	GRPCAddr  string
	ProxyURL  string
	TeamID    string
	TokenSeed []byte
}

// FromEnv reads the E2B configuration from the environment, applying the
// defaults below for variables that are unset or empty.
func FromEnv() (Config, error) {
	cfg := Config{
		GRPCAddr: envOr("E2B_GRPC_ADDR", "127.0.0.1:5008"),
		ProxyURL: envOr("E2B_PROXY_URL", "http://127.0.0.1:5007"),
		TeamID:   envOr("E2B_TEAM_ID", "5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95"),
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
