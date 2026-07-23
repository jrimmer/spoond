package action

import (
	"context"
	"fmt"
)

// CheckoutHandler implements actions/checkout: git clone + checkout SHA.
type CheckoutHandler struct{}

func (h *CheckoutHandler) Execute(ctx context.Context, step *Step, exec Executor, logger Logger) (int, error) {
	logger.Logf("actions/checkout: cloning repository")
	repo := step.With["repository"]
	ref := step.With["ref"]
	path := step.With["path"]
	if path == "" {
		path = "."
	}

	if repo == "" {
		return 1, fmt.Errorf("checkout: repository is required")
	}

	logger.Logf("cloning %s (ref=%s) into %s", repo, ref, path)

	// TODO: implement git clone via exec.Exec
	// For now, this is a stub that will be filled in during T4/T5.
	return 0, nil
}
