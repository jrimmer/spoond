package runner

import (
	"fmt"
	"os"
	"strings"
)

// Config holds all runtime configuration for the forkd-service runner.
// Values are sourced from environment variables with sensible defaults.
type Config struct {
	// ForgejoURL is the base URL of the Forgejo instance (e.g. https://code.lacy.casa).
	ForgejoURL string
	// ForgejoToken is the runner registration token.
	ForgejoToken string
	// HyperAddr is the gRPC address of the Hyper cluster (e.g. 172.30.0.1:50051).
	HyperAddr string
	// Labels are the runner labels advertised to Forgejo.
	Labels []string
	// TemplateMap maps runner labels to Hyper image IDs.
	TemplateMap map[string]string
	// DefaultImage is the fallback Hyper image ID when no label matches.
	DefaultImage string
	// InstanceType is the Hyper InstanceType name to use for CI jobs.
	InstanceType string
}

// LoadConfig reads configuration from environment variables.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		ForgejoURL:   getEnv("FORGEJO_URL", "https://code.lacy.casa"),
		ForgejoToken: getEnv("FORGEJO_TOKEN", ""),
		HyperAddr:    getEnv("HYPER_ADDR", "172.30.0.1:50051"),
		Labels:       splitCSV(getEnv("RUNNER_LABELS", "microvm,x86-64,linux")),
		TemplateMap:  parseTemplateMap(getEnv("RUNNER_TEMPLATE_MAP", "")),
		DefaultImage: getEnv("RUNNER_DEFAULT_IMAGE", ""),
		InstanceType: getEnv("RUNNER_INSTANCE_TYPE", "INSTANCE_TYPE_CENTI"),
	}

	if cfg.ForgejoToken == "" {
		return nil, fmt.Errorf("FORGEJO_TOKEN is required")
	}
	if cfg.ForgejoURL == "" {
		return nil, fmt.Errorf("FORGEJO_URL is required")
	}

	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// parseTemplateMap parses a string like "x86-64=sha256:abc,linux=sha256:def"
// into a label to image-ID map.
func parseTemplateMap(s string) map[string]string {
	m := make(map[string]string)
	if s == "" {
		return m
	}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return m
}
