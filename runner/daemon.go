package runner

import (
	"fmt"
)

// Daemon is the long-running runner process that polls Forgejo for tasks
// and dispatches them to Hyper microVMs.
//
// Register() and Run() methods are implemented in register.go and poll.go
// respectively.
type Daemon struct {
	cfg     *Config
	forgejo *ForgejoClient
	creds   *RunnerCredentials
}

// NewDaemon creates a new runner daemon from the loaded Config.
func NewDaemon(cfg *Config) (*Daemon, error) {
	if cfg.ForgejoToken == "" {
		return nil, fmt.Errorf("forgejo token is required")
	}
	return &Daemon{cfg: cfg}, nil
}