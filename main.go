package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"code.lacy.casa/jrimmer/hyper-forgejo-runner/hyper"
	"code.lacy.casa/jrimmer/hyper-forgejo-runner/hyper/proto"
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
		configOnly    = flag.Bool("config", false, "print resolved config and exit")
		register      = flag.Bool("register", false, "register the runner with Forgejo and exit")
		testHyper     = flag.String("test-hyper", "", "connect to Hyper, boot a VM from the given image ID, exec 'echo hello', and tear down (verifiable checkpoint for T4)")
		hyperAddr     = flag.String("hyper-addr", "", "override HYPER_ADDR for --test-hyper")
		healthTimeout = flag.Duration("health-timeout", 30*time.Second, "timeout for guest-agent health polling (default 30s)")
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

	if *testHyper != "" {
		addr := cfg.HyperAddr
		if *hyperAddr != "" {
			addr = *hyperAddr
		}
		return runHyperCheckpoint(addr, *testHyper, *healthTimeout)
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

// runHyperCheckpoint is the verifiable checkpoint for T4: connect to Hyper,
// boot a VM, wait for the guest-agent, exec "echo hello", then tear down.
func runHyperCheckpoint(addr, imgID string, healthTimeout time.Duration) error {
	log.Printf("T4 checkpoint: dialing Hyper at %s", addr)

	hc, err := hyper.NewClient(addr)
	if err != nil {
		return fmt.Errorf("hyper client: %w", err)
	}
	defer hc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Boot a VM from the provided image ID.
	log.Printf("T4 checkpoint: creating VM (image=%s, type=CENTI, arch=x86_64)", imgID)
	createResp, err := hc.CreateVm(ctx, imgID, proto.InstanceType_INSTANCE_TYPE_CENTI, proto.Architecture_ARCHITECTURE_X86_64)
	if err != nil {
		return fmt.Errorf("create vm: %w", err)
	}
	vmID := createResp.VmId
	node := createResp.Node
	log.Printf("T4 checkpoint: VM created (id=%s, node=%s)", vmID, node)

	// Ensure teardown regardless of outcome.
	teardown := func() {
		log.Printf("T4 checkpoint: stopping VM %s", vmID)
		if err := hc.StopVm(ctx, vmID); err != nil {
			log.Printf("T4 checkpoint: stop vm error: %v", err)
		}
	}
	defer teardown()

	// Connect to the guest-agent via the per-VM Unix socket.
	sockPath := fmt.Sprintf("unix:///srv/hyper/socks/grpc-%s.sock", vmID)
	log.Printf("T4 checkpoint: dialing guest-agent at %s", sockPath)

	agent, err := hyper.NewAgentClient(sockPath)
	if err != nil {
		return fmt.Errorf("agent client: %w", err)
	}
	defer agent.Close()

	// Wait for the guest-agent to be ready.
	log.Printf("T4 checkpoint: waiting for guest-agent health (timeout=%s)", healthTimeout)
	if err := agent.WaitForHealth(ctx, healthTimeout); err != nil {
		return fmt.Errorf("wait for health: %w", err)
	}
	log.Printf("T4 checkpoint: guest-agent is healthy")

	// Exec "echo hello" inside the VM.
	log.Printf("T4 checkpoint: executing 'echo hello' in VM")
	stdout, stderr, exitCode, err := agent.ExecSimple(ctx, "echo", "hello")
	if err != nil {
		return fmt.Errorf("exec echo hello: %w", err)
	}

	fmt.Printf("=== T4 Checkpoint Results ===\n")
	fmt.Printf("VM ID:      %s\n", vmID)
	fmt.Printf("Node:       %s\n", node)
	fmt.Printf("Exit Code:  %d\n", exitCode)
	fmt.Printf("Stdout:     %s\n", string(stdout))
	fmt.Printf("Stderr:     %s\n", string(stderr))

	if exitCode != 0 {
		return fmt.Errorf("exec returned non-zero exit code %d", exitCode)
	}
	if string(stdout) != "hello\n" {
		return fmt.Errorf("unexpected stdout: %q (expected %q)", string(stdout), "hello\n")
	}

	log.Printf("T4 checkpoint: PASSED — VM booted, exec succeeded, tearing down")
	return nil
}