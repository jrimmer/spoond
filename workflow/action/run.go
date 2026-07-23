package action

import (
	"context"
	"fmt"
	"strings"
)

// RunHandler implements the "run:" step: shell execution via guest-agent.
type RunHandler struct{}

func (h *RunHandler) Execute(ctx context.Context, step *Step, exec Executor, logger Logger) (int, error) {
	if step.Run == "" {
		return 1, fmt.Errorf("run: no command specified")
	}

	shell := step.Shell
	if shell == "" {
		shell = "bash"
	}

	var argv []string
	switch shell {
	case "bash":
		argv = []string{"/bin/bash", "-c", step.Run}
	case "sh":
		argv = []string{"/bin/sh", "-c", step.Run}
	case "python":
		argv = []string{"python3", "-c", step.Run}
	default:
		argv = []string{shell, "-c", step.Run}
	}

	logger.Logf("run: %s", strings.Join(argv, " "))

	exitCode, err := exec.ExecStreaming(argv, step.Env, step.WorkingDir, func(line string) {
		logger.Log(line)
	})
	return exitCode, err
}
