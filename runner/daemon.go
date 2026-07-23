package runner

import (
	"context"
	"fmt"
	"log"
)

// Daemon is the long-running runner process that polls Forgejo for tasks
// and dispatches them to Hyper microVMs.
type Daemon struct {
	cfg *Config
}

// NewDaemon creates a new runner daemon from the loaded Config.
func NewDaemon(cfg *Config) (*Daemon, error) {
	if cfg.ForgejoToken == "" {
		return nil, fmt.Errorf("forgejo token is required")
	}
	return &Daemon{cfg: cfg}, nil
}

// Register performs one-time runner registration with Forgejo.
func (d *Daemon) Register() error {
	log.Printf("registering runner with %s (labels: %v)", d.cfg.ForgejoURL, d.cfg.Labels)
	// TODO: implement gRPC registration via connectrpc
	return fmt.Errorf("registration not yet implemented")
}

// Run starts the long-poll loop, blocking until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	log.Printf("runner started, polling %s for tasks", d.cfg.ForgejoURL)
	// TODO: implement FetchTask long-poll loop
	<-ctx.Done()
	log.Printf("runner shutting down")
	return nil
}
