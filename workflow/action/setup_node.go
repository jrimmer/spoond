package action

import (
	"context"
)

// SetupNodeHandler implements actions/setup-node: download Node, set PATH.
type SetupNodeHandler struct{}

func (h *SetupNodeHandler) Execute(ctx context.Context, step *Step, exec Executor, logger Logger) (int, error) {
	version := step.With["node-version"]
	logger.Logf("actions/setup-node: installing Node %s", version)
	// TODO: implement Node download + PATH setup via exec.Exec
	return 0, nil
}
