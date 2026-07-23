package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"code.lacy.casa/jrimmer/hyper-forgejo-runner/runner"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "hyper-forgejo-runner: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configOnly = flag.Bool("config", false, "print resolved config and exit")
		register   = flag.Bool("register", false, "register the runner with Forgejo and exit")
	)
	flag.Parse()

	cfg, err := runner.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if *configOnly {
		fmt.Printf("Forgejo URL:    %s\n", cfg.ForgejoURL)
		fmt.Printf("Hyper Addr:     %s\n", cfg.HyperAddr)
		fmt.Printf("Labels:         %v\n", cfg.Labels)
		fmt.Printf("Default Image:  %s\n", cfg.DefaultImage)
		fmt.Printf("Instance Type:  %s\n", cfg.InstanceType)
		fmt.Printf("Template Map:   %v\n", cfg.TemplateMap)
		return nil
	}

	log.Printf("hyper-forgejo-runner starting (Forgejo=%s, Hyper=%s)", cfg.ForgejoURL, cfg.HyperAddr)

	daemon, err := runner.NewDaemon(cfg)
	if err != nil {
		return fmt.Errorf("create runner daemon: %w", err)
	}

	if *register {
		return daemon.Register()
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(nil, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return daemon.Run(ctx)
}
