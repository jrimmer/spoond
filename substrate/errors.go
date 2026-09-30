package substrate

import "errors"

var (
	ErrNotFound = errors.New("substrate: not found") // sandbox or build unknown to the orchestrator
	ErrCapacity = errors.New("substrate: capacity")  // node refused (sandbox cap or starting limit)
)
