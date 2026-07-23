package action

import (
	"context"
)

// SetupGoHandler implements actions/setup-go: download Go, set PATH/GOROOT.
type SetupGoHandler struct{}

func (h *SetupGoHandler) Execute(ctx context.Context, step *Step, exec Executor, logger Logger) (int, error) {
	version := step.With["go-version"]
	logger.Logf("actions/setup-go: installing Go %s", version)
	// TODO: implement Go download + PATH setup via exec.Exec
	return 0, nil
}
