package install

import (
	"context"
	"errors"
	"fmt"
)

// The host phase: packages, kernel modules, hugepages, the iptables
// backend, the snapshot store, Docker and its registry, and the AppArmor
// check (docs/plans/2026-10-16-4.0-install.md, the `host` row).
//
// Each step lives in its own file and registers itself in hostSteps from
// an init function; hostOrder fixes the run order.

func init() { Phases[PhaseHost] = hostPhase }

// hostOrder is the step order: each may depend on the ones before it.
var hostOrder = []string{
	"packages",  // host_system.go
	"modules",   // host_system.go
	"udev",      // host_system.go
	"hugepages", // host_system.go
	"iptables",  // host_system.go
	"store",     // host_store.go
	"docker",    // host_store.go
	"apparmor",  // host_store.go
}

// hostStep is one host step. It adds its rows to r and returns the
// first failure, with the fix in the message and no phase prefix.
type hostStep func(ctx context.Context, env *Env, r *Reporter, st *hostState) error

// hostSteps maps a step name to its implementation.
var hostSteps = map[string]hostStep{}

// hostState carries what one step learns for the ones after it.
type hostState struct {
	// Hugepages is the page count the hugepages step settled on.
	Hugepages int
	// RebootNeeded is set when the kernel could not reserve Hugepages
	// on the running host; the phase finishes the other steps, then
	// returns ErrRebootNeeded.
	RebootNeeded bool
}

// ErrRebootNeeded ends a host phase that has done everything it can
// before the kernel reserves the hugepages at boot.
var ErrRebootNeeded = errors.New("reboot so the kernel reserves the hugepages at boot, then run `spoond install` again")

func hostPhase(ctx context.Context, env *Env) ([]Item, error) {
	r := &Reporter{Phase: PhaseHost, Out: env.Out}
	st := &hostState{}
	for _, name := range hostOrder {
		step, ok := hostSteps[name]
		if !ok {
			return r.Items(), fmt.Errorf("step %s: %w", name, errNotYet())
		}
		if err := step(ctx, env, r, st); err != nil {
			return r.Items(), err
		}
	}
	if st.RebootNeeded {
		return r.Items(), fmt.Errorf("%w (%d hugepages)", ErrRebootNeeded, st.Hugepages)
	}
	return r.Items(), nil
}
